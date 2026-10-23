// Package ipam hands out IPv4 addresses from one subnet per node, recorded as one
// file per address on disk so allocations survive plugin restarts and node reboots.
//
// Each CNI invocation is a separate process, and the kubelet may run several at
// once, so every operation holds an exclusive flock on the network's directory.
package ipam

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Store keeps the allocations for one network in Dir.
type Store struct {
	Dir    string
	Subnet *net.IPNet
}

// ErrExhausted means every usable address in the subnet is taken.
var ErrExhausted = errors.New("ipam: no free addresses left in subnet")

const lastFile = "last_reserved_ip"

// Open prepares the store for subnet, which must be IPv4.
func Open(dir string, subnet *net.IPNet) (*Store, error) {
	if subnet.IP.To4() == nil {
		return nil, fmt.Errorf("ipam: only IPv4 subnets are supported, got %s", subnet)
	}
	if ones, bits := subnet.Mask.Size(); bits-ones < 2 {
		return nil, fmt.Errorf("ipam: subnet %s is too small", subnet)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{Dir: dir, Subnet: subnet}, nil
}

// Gateway is the subnet's first address, used by the bridge.
func (s *Store) Gateway() net.IP { return addIP(s.network(), 1) }

// Allocate returns the address held by (containerID, ifName), assigning one if it
// has none. Asking twice gives the same answer, because the kubelet retries ADD.
//
// Addresses are handed out round-robin from the last one assigned rather than
// lowest-first, so a freed address is not reused at once while other nodes may
// still hold connection-tracking state or ARP entries for the old pod.
func (s *Store) Allocate(containerID, ifName string) (net.IP, error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	owner := owner(containerID, ifName)
	if ip, ok := s.findOwner(owner); ok {
		return ip, nil
	}
	first, last := addIP(s.network(), 2), addIP(s.broadcast(), -1) // skip network, gateway, broadcast
	size := int(ipToUint(last) - ipToUint(first) + 1)
	start := first
	if b, err := os.ReadFile(filepath.Join(s.Dir, lastFile)); err == nil {
		if prev := net.ParseIP(strings.TrimSpace(string(b))).To4(); prev != nil && s.Subnet.Contains(prev) {
			start = addIP(prev, 1)
		}
	}
	for i := 0; i < size; i++ {
		ip := addIP(first, int((ipToUint(start)-ipToUint(first)+uint32(i))%uint32(size)))
		f, err := os.OpenFile(filepath.Join(s.Dir, ip.String()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		_, werr := f.WriteString(owner)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(f.Name())
			return nil, werr
		}
		if err := os.WriteFile(filepath.Join(s.Dir, lastFile), []byte(ip.String()), 0o644); err != nil {
			return nil, err
		}
		return ip, nil
	}
	return nil, ErrExhausted
}

// Release frees whatever (containerID, ifName) holds. Releasing nothing is not an
// error: CNI DEL must succeed when called twice or for a container it never saw.
func (s *Store) Release(containerID, ifName string) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if ip, ok := s.findOwner(owner(containerID, ifName)); ok {
		return os.Remove(filepath.Join(s.Dir, ip.String()))
	}
	return nil
}

// Lookup reports the address held by (containerID, ifName), if any.
func (s *Store) Lookup(containerID, ifName string) (net.IP, bool) {
	unlock, err := s.lock()
	if err != nil {
		return nil, false
	}
	defer unlock()
	return s.findOwner(owner(containerID, ifName))
}

func (s *Store) findOwner(owner string) (net.IP, bool) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, false
	}
	for _, e := range entries {
		ip := net.ParseIP(e.Name()).To4()
		if ip == nil {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(s.Dir, e.Name())); err == nil && string(b) == owner {
			return ip, true
		}
	}
	return nil, false
}

func (s *Store) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.Dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}

func (s *Store) network() net.IP { return s.Subnet.IP.Mask(s.Subnet.Mask).To4() }

func (s *Store) broadcast() net.IP {
	n, mask := s.network(), net.IP(s.Subnet.Mask).To4()
	out := make(net.IP, 4)
	for i := range out {
		out[i] = n[i] | ^mask[i]
	}
	return out
}

func owner(containerID, ifName string) string { return containerID + "\n" + ifName }

func ipToUint(ip net.IP) uint32 { return binary.BigEndian.Uint32(ip.To4()) }

func addIP(ip net.IP, n int) net.IP {
	out := make(net.IP, 4)
	binary.BigEndian.PutUint32(out, uint32(int64(ipToUint(ip))+int64(n)))
	return out
}
