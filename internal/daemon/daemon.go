// Package daemon is `minicni daemon`, which runs on every Kubernetes node (as a
// DaemonSet) and does the cluster-wide part of networking that a CNI plugin,
// invoked once per pod, cannot:
//
//  1. install the minicni binary into the node's CNI bin directory;
//  2. wait for the node's pod subnet (spec.podCIDR) and write the CNI config
//     that tells the kubelet to use minicni with that subnet;
//  3. keep one route per other node: "its pod subnet is reachable via its IP".
//
// It talks to the Kubernetes API with plain HTTPS and the pod's service-account
// token, so it needs no client library.
package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/vishvananda/netlink"
)

// routeProtocol tags the routes this daemon owns, so it can tell them apart from
// everyone else's and remove the ones for nodes that left. Any value above 4 that
// no routing daemon uses works; this one is unassigned in /etc/iproute2/rt_protos.
const routeProtocol = 199

// Config for one node's daemon.
type Config struct {
	NodeName    string
	ClusterCIDR string
	PluginSrc   string // this binary, copied into BinDir as "minicni"
	BinDir      string // e.g. /host/opt/cni/bin
	ConfDir     string // e.g. /host/etc/cni/net.d
	Interval    time.Duration
}

type node struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		PodCIDR string `json:"podCIDR"`
	} `json:"spec"`
	Status struct {
		Addresses []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"addresses"`
	} `json:"status"`
}

func (n node) internalIP() net.IP {
	for _, a := range n.Status.Addresses {
		if a.Type == "InternalIP" {
			return net.ParseIP(a.Address).To4()
		}
	}
	return nil
}

// Run blocks until ctx is cancelled.
func Run(ctx context.Context, cfg Config) error {
	api, err := inCluster()
	if err != nil {
		return err
	}
	if err := installBinary(cfg.PluginSrc, filepath.Join(cfg.BinDir, "minicni")); err != nil {
		return err
	}
	log.Printf("installed plugin into %s", cfg.BinDir)

	var self node
	for {
		if err := api.get(ctx, "/api/v1/nodes/"+cfg.NodeName, &self); err != nil {
			log.Printf("read node %s: %v", cfg.NodeName, err)
		} else if self.Spec.PodCIDR != "" {
			break
		} else {
			log.Printf("node %s has no pod CIDR yet", cfg.NodeName)
		}
		if !sleep(ctx, 2*time.Second) {
			return ctx.Err()
		}
	}
	if err := writeConfig(cfg, self.Spec.PodCIDR); err != nil {
		return err
	}
	log.Printf("node %s uses pod subnet %s", cfg.NodeName, self.Spec.PodCIDR)

	for {
		if err := syncRoutes(ctx, api, cfg.NodeName); err != nil {
			log.Printf("sync routes: %v", err)
		}
		if !sleep(ctx, cfg.Interval) {
			return ctx.Err()
		}
	}
}

func writeConfig(cfg Config, podCIDR string) error {
	conf := map[string]any{
		"cniVersion": "1.0.0",
		"name":       "minicni",
		"plugins": []map[string]any{{
			"type":        "minicni",
			"bridge":      "mcni0",
			"subnet":      podCIDR,
			"clusterCIDR": cfg.ClusterCIDR,
		}},
	}
	b, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(cfg.ConfDir, "10-minicni.conflist"), b, 0o644)
}

// syncRoutes makes the node's routing table match the cluster: one route per other
// node's pod subnet via that node's address, and none for nodes that are gone.
func syncRoutes(ctx context.Context, api *client, self string) error {
	var list struct {
		Items []node `json:"items"`
	}
	if err := api.get(ctx, "/api/v1/nodes", &list); err != nil {
		return err
	}
	want := map[string]net.IP{}
	for _, n := range list.Items {
		if n.Metadata.Name == self || n.Spec.PodCIDR == "" || n.internalIP() == nil {
			continue
		}
		want[n.Spec.PodCIDR] = n.internalIP()
	}
	for cidr, via := range want {
		_, dst, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		r := &netlink.Route{Dst: dst, Gw: via, Protocol: routeProtocol}
		if err := netlink.RouteReplace(r); err != nil {
			log.Printf("route %s via %s: %v", cidr, via, err)
		}
	}
	existing, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Protocol: routeProtocol}, netlink.RT_FILTER_PROTOCOL)
	if err != nil {
		return err
	}
	for _, r := range existing {
		if r.Dst == nil {
			continue
		}
		if _, ok := want[r.Dst.String()]; !ok {
			log.Printf("removing route for departed node subnet %s", r.Dst)
			_ = netlink.RouteDel(&r)
		}
	}
	return nil
}

// installBinary copies src to dst through a temporary file and a rename, so the
// kubelet never runs a half-written plugin.
func installBinary(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeAtomic(dst, b, 0o755)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// client is a minimal Kubernetes API client using the pod's service account.
type client struct {
	base, token string
	http        *http.Client
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

func inCluster() (*client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		return nil, errors.New("daemon: not running inside Kubernetes (KUBERNETES_SERVICE_HOST unset)")
	}
	token, err := os.ReadFile(filepath.Join(saDir, "token"))
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(filepath.Join(saDir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("daemon: cannot parse service-account CA")
	}
	return &client{
		base:  "https://" + net.JoinHostPort(host, port),
		token: string(token),
		http:  &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
	}, nil
}

func (c *client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
