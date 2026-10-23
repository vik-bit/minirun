package ipam

import (
	"fmt"
	"net"
	"sync"
	"testing"
)

func store(t *testing.T, cidr string) *Store {
	t.Helper()
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.TempDir(), subnet)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSkipsNetworkGatewayAndBroadcast(t *testing.T) {
	s := store(t, "10.244.1.0/29") // .0 network, .1 gateway, .7 broadcast: .2-.6 usable
	if got := s.Gateway().String(); got != "10.244.1.1" {
		t.Fatalf("gateway %s", got)
	}
	var got []string
	for i := 0; i < 5; i++ {
		ip, err := s.Allocate(fmt.Sprintf("c%d", i), "eth0")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ip.String())
	}
	if fmt.Sprint(got) != "[10.244.1.2 10.244.1.3 10.244.1.4 10.244.1.5 10.244.1.6]" {
		t.Fatalf("allocated %v", got)
	}
	if _, err := s.Allocate("c5", "eth0"); err != ErrExhausted {
		t.Fatalf("sixth allocation: %v, want ErrExhausted", err)
	}
}

func TestAllocateIsIdempotentPerContainer(t *testing.T) {
	s := store(t, "10.244.1.0/24")
	a, _ := s.Allocate("abc", "eth0")
	b, _ := s.Allocate("abc", "eth0")
	c, _ := s.Allocate("abc", "eth1")
	if !a.Equal(b) || a.Equal(c) {
		t.Fatalf("same owner got %s and %s; another interface got %s", a, b, c)
	}
}

func TestReleaseIsIdempotentAndAddressesAreNotReusedImmediately(t *testing.T) {
	s := store(t, "10.244.1.0/29")
	first, _ := s.Allocate("a", "eth0")
	if err := s.Release("a", "eth0"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release("a", "eth0"); err != nil {
		t.Fatalf("second release: %v", err)
	}
	if err := s.Release("never-seen", "eth0"); err != nil {
		t.Fatalf("releasing an unknown container: %v", err)
	}
	next, _ := s.Allocate("b", "eth0")
	if next.Equal(first) {
		t.Fatalf("freed address %s was handed out again straight away", first)
	}
	if _, ok := s.Lookup("a", "eth0"); ok {
		t.Fatal("released address is still recorded")
	}
}

func TestWrapsAroundWhenTheEndIsReached(t *testing.T) {
	s := store(t, "10.244.1.0/29")
	for i := 0; i < 5; i++ {
		s.Allocate(fmt.Sprintf("c%d", i), "eth0")
	}
	s.Release("c1", "eth0") // frees .3
	ip, err := s.Allocate("new", "eth0")
	if err != nil || ip.String() != "10.244.1.3" {
		t.Fatalf("got %v, %v; want 10.244.1.3 after wrapping around", ip, err)
	}
}

func TestConcurrentAllocationsNeverCollide(t *testing.T) {
	s := store(t, "10.244.1.0/24")
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A separate Store per goroutine, as separate CNI processes would have.
			other := &Store{Dir: s.Dir, Subnet: s.Subnet}
			ip, err := other.Allocate(fmt.Sprintf("c%d", i), "eth0")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[ip.String()] {
				t.Errorf("%s allocated twice", ip)
			}
			seen[ip.String()] = true
		}(i)
	}
	wg.Wait()
}

func TestRejectsIPv6AndTinySubnets(t *testing.T) {
	for _, cidr := range []string{"fd00::/64", "10.0.0.0/31"} {
		_, subnet, _ := net.ParseCIDR(cidr)
		if _, err := Open(t.TempDir(), subnet); err == nil {
			t.Errorf("%s accepted", cidr)
		}
	}
}
