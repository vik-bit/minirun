// Package plugin is the minicni CNI plugin: it connects a container to a per-node
// Linux bridge with a veth pair and gives it an address from the node's subnet.
//
//	container netns            node (host) netns
//	  eth0 10.244.1.5/24 ── veth mc1a2b3c4d ── bridge mcni0 10.244.1.1/24
//	  default via 10.244.1.1                     │
//	                                             └─ routes to other nodes' subnets
//	                                                (installed by `minicni daemon`)
//
// Pods on the same node talk through the bridge. Traffic to another node's pod
// subnet leaves through the node's own interface, following a route the daemon
// installs ("host-gw": nodes must share a layer-2 network, as Kind nodes do).
// Traffic leaving the cluster is masqueraded to the node's address.
package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/vik-bit/minirun/internal/cni"
	"github.com/vik-bit/minirun/internal/ipam"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// NetConf is minicni's network configuration.
type NetConf struct {
	CNIVersion  string `json:"cniVersion"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Bridge      string `json:"bridge"`      // default "mcni0"
	Subnet      string `json:"subnet"`      // this node's pod subnet, e.g. 10.244.1.0/24
	ClusterCIDR string `json:"clusterCIDR"` // all pod subnets; not masqueraded
	MTU         int    `json:"mtu"`         // default 1500
	DataDir     string `json:"dataDir"`     // IP allocations; default /var/lib/minicni
}

// Args are the CNI_* environment variables for one invocation.
type Args struct {
	Command, ContainerID, NetNS, IfName string
}

func parse(stdin []byte) (*NetConf, *net.IPNet, error) {
	conf := &NetConf{Bridge: "mcni0", MTU: 1500, DataDir: "/var/lib/minicni"}
	if err := json.Unmarshal(stdin, conf); err != nil {
		return nil, nil, &cni.Error{Code: cni.ErrDecodingFailure, Msg: "cannot parse network config", Details: err.Error()}
	}
	_, subnet, err := net.ParseCIDR(conf.Subnet)
	if err != nil {
		return nil, nil, &cni.Error{Code: cni.ErrInvalidConfig, Msg: "invalid subnet", Details: err.Error()}
	}
	if conf.ClusterCIDR == "" {
		conf.ClusterCIDR = subnet.String()
	}
	if _, _, err := net.ParseCIDR(conf.ClusterCIDR); err != nil {
		return nil, nil, &cni.Error{Code: cni.ErrInvalidConfig, Msg: "invalid clusterCIDR", Details: err.Error()}
	}
	if len(conf.Bridge) > unix.IFNAMSIZ-1 {
		return nil, nil, &cni.Error{Code: cni.ErrInvalidConfig, Msg: "bridge name too long"}
	}
	return conf, subnet, nil
}

func (c *NetConf) store(subnet *net.IPNet) (*ipam.Store, error) {
	return ipam.Open(filepath.Join(c.DataDir, "networks", c.Name), subnet)
}

// hostVethName derives a stable, unique, valid (at most 15 bytes) name for the
// node's end of the pair, so DEL and CHECK can find it again.
func hostVethName(containerID, ifName string) string {
	sum := sha256.Sum256([]byte(containerID + "/" + ifName))
	return "mc" + hex.EncodeToString(sum[:])[:11]
}

// Add wires the container into the network and returns the CNI result.
func Add(a Args, stdin []byte) (*cni.Result, error) {
	conf, subnet, err := parse(stdin)
	if err != nil {
		return nil, err
	}
	store, err := conf.store(subnet)
	if err != nil {
		return nil, err
	}
	containerNS, err := netns.GetFromPath(a.NetNS)
	if err != nil {
		return nil, &cni.Error{Code: cni.ErrInvalidEnvironment, Msg: "cannot open network namespace " + a.NetNS, Details: err.Error()}
	}
	defer containerNS.Close()

	gateway := store.Gateway()
	br, err := ensureBridge(conf, gateway, subnet)
	if err != nil {
		return nil, err
	}
	ip, err := store.Allocate(a.ContainerID, a.IfName)
	if err != nil {
		return nil, err
	}
	release := func() { _ = store.Release(a.ContainerID, a.IfName) }

	hostName := hostVethName(a.ContainerID, a.IfName)
	if old, err := netlink.LinkByName(hostName); err == nil {
		_ = netlink.LinkDel(old) // a half-finished earlier ADD for the same container
	}
	// Create both ends in the node's namespace, then move the peer: creating the
	// peer directly inside the container needs a temporary name that cannot clash.
	peerTmp := "tmp" + hostName[2:]
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: hostName, MTU: conf.MTU, MasterIndex: br.Attrs().Index},
		PeerName: peerTmp}
	if err := netlink.LinkAdd(veth); err != nil {
		release()
		return nil, fmt.Errorf("create veth pair: %w", err)
	}
	fail := func(err error) (*cni.Result, error) {
		_ = netlink.LinkDel(veth)
		release()
		return nil, err
	}
	hostLink, err := netlink.LinkByName(hostName)
	if err != nil {
		return fail(err)
	}
	peer, err := netlink.LinkByName(peerTmp)
	if err != nil {
		return fail(err)
	}
	if err := netlink.LinkSetNsFd(peer, int(containerNS)); err != nil {
		return fail(fmt.Errorf("move veth into container: %w", err))
	}
	if err := netlink.LinkSetUp(hostLink); err != nil {
		return fail(err)
	}

	inside, err := netlink.NewHandleAt(containerNS)
	if err != nil {
		return fail(err)
	}
	defer inside.Close()
	ones, _ := subnet.Mask.Size()
	addr := &net.IPNet{IP: ip, Mask: net.CIDRMask(ones, 32)}
	containerMac, err := configureInside(inside, peerTmp, a.IfName, conf.MTU, addr, gateway)
	if err != nil {
		return fail(err)
	}
	if err := ensureHostRules(conf, subnet); err != nil {
		return fail(err)
	}

	two := 2
	return &cni.Result{
		CNIVersion: cni.Version,
		Interfaces: []cni.Interface{
			{Name: br.Attrs().Name, Mac: br.Attrs().HardwareAddr.String()},
			{Name: hostName, Mac: hostLink.Attrs().HardwareAddr.String()},
			{Name: a.IfName, Mac: containerMac, Sandbox: a.NetNS},
		},
		IPs:    []cni.IPConfig{{Address: addr.String(), Gateway: gateway.String(), Interface: &two}},
		Routes: []cni.Route{{Dst: "0.0.0.0/0", GW: gateway.String()}},
	}, nil
}

func configureInside(h *netlink.Handle, tmpName, ifName string, mtu int, addr *net.IPNet, gw net.IP) (string, error) {
	link, err := h.LinkByName(tmpName)
	if err != nil {
		return "", err
	}
	if err := h.LinkSetName(link, ifName); err != nil {
		return "", fmt.Errorf("rename to %s: %w", ifName, err)
	}
	if err := h.LinkSetMTU(link, mtu); err != nil {
		return "", err
	}
	if err := h.AddrAdd(link, &netlink.Addr{IPNet: addr}); err != nil {
		return "", fmt.Errorf("assign %s: %w", addr, err)
	}
	if err := h.LinkSetUp(link); err != nil {
		return "", err
	}
	if lo, err := h.LinkByName("lo"); err == nil {
		_ = h.LinkSetUp(lo)
	}
	if err := h.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: gw}); err != nil {
		return "", fmt.Errorf("add default route: %w", err)
	}
	link, err = h.LinkByName(ifName)
	if err != nil {
		return "", err
	}
	return link.Attrs().HardwareAddr.String(), nil
}

// ensureBridge creates the node's bridge with the subnet's gateway address, or
// returns the existing one. Several ADDs can race here; losing a race is fine.
func ensureBridge(conf *NetConf, gw net.IP, subnet *net.IPNet) (netlink.Link, error) {
	br, err := netlink.LinkByName(conf.Bridge)
	if err != nil {
		attrs := netlink.NewLinkAttrs()
		attrs.Name, attrs.MTU = conf.Bridge, conf.MTU
		if err := netlink.LinkAdd(&netlink.Bridge{LinkAttrs: attrs}); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("create bridge %s: %w", conf.Bridge, err)
		}
		if br, err = netlink.LinkByName(conf.Bridge); err != nil {
			return nil, err
		}
	}
	if _, ok := br.(*netlink.Bridge); !ok {
		return nil, &cni.Error{Code: cni.ErrInvalidConfig, Msg: conf.Bridge + " exists and is not a bridge"}
	}
	ones, _ := subnet.Mask.Size()
	want := &netlink.Addr{IPNet: &net.IPNet{IP: gw, Mask: net.CIDRMask(ones, 32)}}
	if err := netlink.AddrAdd(br, want); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, fmt.Errorf("assign %s to %s: %w", want, conf.Bridge, err)
	}
	if err := netlink.LinkSetUp(br); err != nil {
		return nil, err
	}
	return br, nil
}

// ensureHostRules turns on forwarding and adds the iptables rules the network
// needs: accept forwarded pod traffic (Docker and some distributions default the
// FORWARD chain to DROP), and masquerade traffic leaving the cluster.
func ensureHostRules(conf *NetConf, subnet *net.IPNet) error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	comment := []string{"-m", "comment", "--comment", "minicni " + conf.Name}
	rules := [][]string{
		append([]string{"filter", "FORWARD", "-s", conf.ClusterCIDR}, append(comment, "-j", "ACCEPT")...),
		append([]string{"filter", "FORWARD", "-d", conf.ClusterCIDR}, append(comment, "-j", "ACCEPT")...),
		append([]string{"nat", "POSTROUTING", "-s", subnet.String(), "!", "-d", conf.ClusterCIDR}, append(comment, "-j", "MASQUERADE")...),
	}
	for _, r := range rules {
		table, chain, spec := r[0], r[1], r[2:]
		check := append([]string{"-w", "-t", table, "-C", chain}, spec...)
		if exec.Command("iptables", check...).Run() == nil {
			continue
		}
		add := append([]string{"-w", "-t", table, "-A", chain}, spec...)
		if out, err := exec.Command("iptables", add...).CombinedOutput(); err != nil {
			return fmt.Errorf("iptables %s: %w: %s", strings.Join(add, " "), err, out)
		}
	}
	return nil
}

// Del undoes Add. It must succeed when things are already gone: the kubelet calls
// it again after failures, and after a reboot the namespace no longer exists.
func Del(a Args, stdin []byte) error {
	conf, subnet, err := parse(stdin)
	if err != nil {
		return err
	}
	if link, err := netlink.LinkByName(hostVethName(a.ContainerID, a.IfName)); err == nil {
		_ = netlink.LinkDel(link) // deleting one end of a veth pair deletes both
	}
	store, err := conf.store(subnet)
	if err != nil {
		return err
	}
	return store.Release(a.ContainerID, a.IfName)
}

// Check verifies that the container still has its interface and address.
func Check(a Args, stdin []byte) error {
	conf, subnet, err := parse(stdin)
	if err != nil {
		return err
	}
	store, err := conf.store(subnet)
	if err != nil {
		return err
	}
	ip, ok := store.Lookup(a.ContainerID, a.IfName)
	if !ok {
		return &cni.Error{Code: 100, Msg: "no address recorded for this container"}
	}
	ns, err := netns.GetFromPath(a.NetNS)
	if err != nil {
		return &cni.Error{Code: cni.ErrInvalidEnvironment, Msg: "cannot open network namespace", Details: err.Error()}
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer h.Close()
	link, err := h.LinkByName(a.IfName)
	if err != nil {
		return &cni.Error{Code: 100, Msg: a.IfName + " is missing from the container"}
	}
	addrs, err := h.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, addr := range addrs {
		if addr.IP.Equal(ip) {
			return nil
		}
	}
	return &cni.Error{Code: 100, Msg: fmt.Sprintf("%s does not have its address %s", a.IfName, ip)}
}
