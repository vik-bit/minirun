// Command minirun runs a command in an isolated container:
//
//	minirun run --image ./rootfs --memory 64M --cpus 0.5 --pids 64 -- /bin/sh
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/vik-bit/minirun/internal/cgroups"
	"github.com/vik-bit/minirun/internal/container"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == container.InitArg {
		if err := container.Init(); err != nil {
			fmt.Fprintln(os.Stderr, "minirun: container init:", err)
			os.Exit(127)
		}
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: minirun run [flags] -- command [args...]")
		os.Exit(2)
	}
	os.Exit(run(os.Args[2:]))
}

type envList []string

func (e *envList) String() string     { return strings.Join(*e, ",") }
func (e *envList) Set(v string) error { *e = append(*e, v); return nil }

func run(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	image := fs.String("image", "", "root filesystem directory (read-only lower layer)")
	hostname := fs.String("hostname", "", "hostname inside the container (default: the container id)")
	memory := fs.String("memory", "", "memory limit, e.g. 64M")
	cpus := fs.Float64("cpus", 0, "CPU limit in cores, e.g. 0.5")
	pids := fs.Int64("pids", 0, "maximum number of processes")
	network := fs.String("net", "none", `"none" (loopback only) or "cni"`)
	cniConf := fs.String("cni-conf", "/etc/cni/net.d/10-minicni.conflist", "CNI network config list")
	cniPath := fs.String("cni-path", "/opt/cni/bin", "directories to search for CNI plugins")
	noSeccomp := fs.Bool("no-seccomp", false, "do not install the seccomp filter")
	keep := fs.Bool("keep", false, "keep the container's writable layer after it exits")
	stateDir := fs.String("state-dir", "/var/lib/minirun", "where container layers live")
	var env envList
	fs.Var(&env, "env", "KEY=VALUE to set inside the container (repeatable)")
	_ = fs.Parse(args)

	if *image == "" || fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "minirun run: --image and a command are required")
		return 2
	}
	mem, err := cgroups.ParseBytes(*memory)
	if err != nil {
		fmt.Fprintln(os.Stderr, "minirun run:", err)
		return 2
	}
	id := container.NewID()
	if *hostname == "" {
		*hostname = id
	}
	cfg := container.Config{
		ID: id, Args: fs.Args(), Env: env, Hostname: *hostname, Image: *image, Seccomp: !*noSeccomp,
		Limits:  cgroups.Limits{MemoryBytes: mem, CPUs: *cpus, Pids: *pids},
		Network: container.Network{Mode: *network, CNIConfig: *cniConf, CNIBinDirs: *cniPath},
	}
	res, err := container.Run(cfg, container.Options{StateDir: *stateDir, CgroupPath: "minirun", Keep: *keep})
	if err != nil {
		fmt.Fprintln(os.Stderr, "minirun:", err)
		return 125
	}
	if res.OOMKilled {
		fmt.Fprintf(os.Stderr, "minirun: container %s was killed for exceeding its memory limit\n", id)
	}
	return res.ExitCode
}
