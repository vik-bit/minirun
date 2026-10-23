//go:build integration

// Integration tests: real containers from an Alpine root filesystem, so they need
// Linux with cgroups v2, root, iptables, and MINIRUN_IMAGE pointing at an
// unpacked rootfs. scripts/integration.sh sets all of this up in a privileged
// Docker container.
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	bin     string // directory with the built minirun, minicni and probe
	image   string // rootfs with probe copied in
	cniConf string
	cniData string
)

func TestMain(m *testing.M) {
	if os.Geteuid() != 0 || os.Getenv("MINIRUN_IMAGE") == "" {
		fmt.Println("skipping integration tests: need root and MINIRUN_IMAGE (run scripts/integration.sh)")
		os.Exit(0)
	}
	tmp, err := os.MkdirTemp("", "minirun-it")
	must(err)
	bin = filepath.Join(tmp, "bin")
	for pkg, name := range map[string]string{"../../cmd/minirun": "minirun", "../../cmd/minicni": "minicni", "./probe": "probe"} {
		// -buildvcs=false: the checkout belongs to another user than this root
		// process, so git refuses to read it and the build would fail.
		cmd := exec.Command("go", "build", "-buildvcs=false", "-o", filepath.Join(bin, name), pkg)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("build %s: %v\n%s", pkg, err, out)
			os.Exit(1)
		}
	}
	image = filepath.Join(tmp, "image")
	must(exec.Command("cp", "-a", os.Getenv("MINIRUN_IMAGE"), image).Run())
	must(exec.Command("cp", filepath.Join(bin, "probe"), filepath.Join(image, "usr/local/bin/probe")).Run())

	cniData = filepath.Join(tmp, "cni")
	cniConf = filepath.Join(tmp, "net.conflist")
	conf, _ := json.Marshal(map[string]any{
		"cniVersion": "1.0.0", "name": "it",
		"plugins": []map[string]any{{"type": "minicni", "bridge": "mcni-it", "subnet": "10.99.0.0/24",
			"clusterCIDR": "10.99.0.0/16", "dataDir": cniData}},
	})
	must(os.WriteFile(cniConf, conf, 0o644))
	os.Exit(m.Run())
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

type result struct {
	stdout, stderr string
	code           int
}

func run(t *testing.T, flags []string, args ...string) result {
	t.Helper()
	all := append([]string{"run", "--image", image, "--state-dir", t.TempDir()}, flags...)
	cmd := exec.Command(filepath.Join(bin, "minirun"), append(append(all, "--"), args...)...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	if e, ok := err.(*exec.ExitError); ok {
		code = e.ExitCode()
	} else if err != nil {
		t.Fatalf("minirun: %v", err)
	}
	return result{strings.TrimSpace(out.String()), strings.TrimSpace(errOut.String()), code}
}

func TestRunsACommandAndReturnsItsExitCode(t *testing.T) {
	if r := run(t, nil, "echo", "hello"); r.stdout != "hello" || r.code != 0 {
		t.Fatalf("got %+v", r)
	}
	if r := run(t, nil, "sh", "-c", "exit 7"); r.code != 7 {
		t.Fatalf("exit code %d, want 7", r.code)
	}
}

func TestIsPIDOneInItsOwnPIDNamespace(t *testing.T) {
	r := run(t, nil, "sh", "-c", `echo $$; ls /proc | grep -cE '^[0-9]+$'`)
	lines := strings.Fields(r.stdout)
	if len(lines) != 2 || lines[0] != "1" {
		t.Fatalf("got %q, want PID 1", r.stdout)
	}
	// The shell, ls and grep: a handful of processes, not the host's hundreds.
	if n, err := strconv.Atoi(lines[1]); err != nil || n > 5 {
		t.Fatalf("container sees %s processes", lines[1])
	}
}

func TestHasItsOwnHostname(t *testing.T) {
	if r := run(t, []string{"--hostname", "box"}, "hostname"); r.stdout != "box" {
		t.Fatalf("hostname %q", r.stdout)
	}
}

func TestWritesStayInTheContainerLayer(t *testing.T) {
	r := run(t, nil, "sh", "-c", "echo data > /etc/written-by-container && cat /etc/written-by-container")
	if r.stdout != "data" {
		t.Fatalf("got %+v", r)
	}
	if _, err := os.Stat(filepath.Join(image, "etc/written-by-container")); err == nil {
		t.Fatal("the write reached the read-only image")
	}
}

func TestCapabilitiesAreDockersDefaultSet(t *testing.T) {
	const dockerDefault = "00000000a80425fb"
	r := run(t, nil, "probe", "caps")
	if !strings.Contains(r.stdout, "CapBnd: "+dockerDefault) || !strings.Contains(r.stdout, "CapEff: "+dockerDefault) {
		t.Fatalf("got %q, want bounding and effective sets %s", r.stdout, dockerDefault)
	}
}

func TestMountIsRefused(t *testing.T) {
	if r := run(t, nil, "probe", "mount"); r.stdout != "EPERM" {
		t.Fatalf("mount: %q, want EPERM", r.stdout)
	}
}

// Creating a user namespace needs no capability, so only the seccomp filter can
// stop it. Running each probe with and without the filter shows the filter is
// what makes the difference.
func TestSeccompBlocksWhatCapabilitiesAllow(t *testing.T) {
	for _, probe := range []string{"unshare-user", "clone-newuser"} {
		if r := run(t, []string{"--no-seccomp"}, "probe", probe); r.stdout != "ok" {
			t.Errorf("%s without seccomp: %q, want ok (else this test proves nothing)", probe, r.stdout)
		}
		if r := run(t, nil, "probe", probe); r.stdout != "EPERM" {
			t.Errorf("%s with seccomp: %q, want EPERM", probe, r.stdout)
		}
	}
	if r := run(t, nil, "probe", "keyctl"); r.stdout != "EPERM" {
		t.Errorf("keyctl with seccomp: %q, want EPERM", r.stdout)
	}
}

func TestKernelMemoryIsMaskedAndSysctlsAreReadOnly(t *testing.T) {
	r := run(t, nil, "sh", "-c", "wc -c < /proc/kcore; echo 1 > /proc/sys/kernel/domainname && echo writable")
	if !strings.HasPrefix(r.stdout, "0") || strings.Contains(r.stdout, "writable") {
		t.Fatalf("got %q", r.stdout)
	}
}

func TestMemoryLimitTriggersTheOOMKiller(t *testing.T) {
	// Build a 100 MB string in a 32 MB container.
	r := run(t, []string{"--memory", "32M"}, "sh", "-c", `x=$(head -c 100000000 /dev/zero | tr '\0' a); echo survived`)
	if r.code != 137 || !strings.Contains(r.stderr, "exceeding its memory limit") {
		t.Fatalf("got %+v, want exit 137 and an OOM message", r)
	}
	if r := run(t, []string{"--memory", "32M"}, "sh", "-c", "head -c 1000000 /dev/zero | wc -c"); r.stdout != "1000000" {
		t.Fatalf("a small allocation failed under the limit: %+v", r)
	}
}

func TestPidsLimitStopsAForkBomb(t *testing.T) {
	r := run(t, []string{"--pids", "8"}, "sh", "-c", "for i in $(seq 30); do sleep 2 & done; wait; echo finished")
	if !strings.Contains(r.stderr, "fork") {
		t.Fatalf("30 background processes under a limit of 8: %+v", r)
	}
}

func TestCPULimitSlowsABusyLoop(t *testing.T) {
	loop := []string{"sh", "-c", "i=0; while [ $i -lt 300000 ]; do i=$((i+1)); done"}
	start := time.Now()
	run(t, nil, loop...)
	free := time.Since(start)
	start = time.Now()
	run(t, []string{"--cpus", "0.2"}, loop...)
	limited := time.Since(start)
	t.Logf("busy loop: %v unlimited, %v at 0.2 CPUs", free, limited)
	if limited < 3*free {
		t.Fatalf("0.2 CPUs ran %v vs %v unlimited; expected about 5x slower", limited, free)
	}
}

func TestLoopbackWorksWithoutANetwork(t *testing.T) {
	// One interface in total, and it is the loopback, up.
	r := run(t, nil, "sh", "-c", "ip -o link show | grep -c 'LOOPBACK,UP'; ip -o link show | wc -l")
	if got := strings.Fields(r.stdout); len(got) != 2 || got[0] != "1" || got[1] != "1" {
		t.Fatalf("got %q, want exactly one interface, the loopback, up", r.stdout)
	}
}

func TestCNIGivesContainersAddressesTheyCanReachEachOtherOn(t *testing.T) {
	net := []string{"--net", "cni", "--cni-conf", cniConf, "--cni-path", bin}
	var wg sync.WaitGroup
	var server result
	wg.Add(1)
	go func() {
		defer wg.Done()
		server = run(t, append(net, "--hostname", "server"), "sh", "-c", "ip -4 -o addr show eth0; sleep 4")
	}()
	time.Sleep(1500 * time.Millisecond)
	client := run(t, net, "sh", "-c", "ip -4 -o addr show eth0; ping -c 2 -W 2 10.99.0.2 >/dev/null && echo reachable")
	wg.Wait()

	if !strings.Contains(server.stdout, "10.99.0.2/24") {
		t.Fatalf("server address: %q", server.stdout)
	}
	if !strings.Contains(client.stdout, "10.99.0.3/24") || !strings.Contains(client.stdout, "reachable") {
		t.Fatalf("client: %+v", client)
	}
	// DEL ran for both: their addresses are free again.
	var leftover []string
	filepath.WalkDir(cniData, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "10.99.") {
			leftover = append(leftover, d.Name())
		}
		return nil
	})
	if len(leftover) > 0 {
		t.Fatalf("addresses still allocated after exit: %v", leftover)
	}
}
