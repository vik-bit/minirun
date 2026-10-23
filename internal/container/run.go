package container

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/vik-bit/minirun/internal/cgroups"
	"github.com/vik-bit/minirun/internal/cni"
	"golang.org/x/sys/unix"
)

// Result describes how a container ended.
type Result struct {
	ExitCode   int
	OOMKilled  bool
	PeakMemory int64
}

// Options for Run that are not part of the child's Config.
type Options struct {
	StateDir   string // per-container overlay directories live here
	CgroupPath string // parent cgroup, relative to /sys/fs/cgroup
	Keep       bool   // keep the upper layer after exit, for inspection
}

// NewID returns a random 12-hex-digit container id.
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Run starts the container described by cfg, waits for it, and cleans up.
func Run(cfg Config, opt Options) (Result, error) {
	// Pdeathsig fires when the thread that forked the child exits, not the process.
	// Go may retire idle threads, so pin this goroutine to its thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	dir := filepath.Join(opt.StateDir, cfg.ID)
	cfg.Upper, cfg.Work, cfg.Merged = filepath.Join(dir, "upper"), filepath.Join(dir, "work"), filepath.Join(dir, "merged")
	for _, d := range []string{cfg.Upper, cfg.Work, cfg.Merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return Result{}, err
		}
	}
	// The kernel refuses (with a bare EINVAL) an overlay whose upper layer is itself
	// on overlayfs, which is the case inside most Docker containers.
	var fs unix.Statfs_t
	if err := unix.Statfs(cfg.Upper, &fs); err == nil && fs.Type == unix.OVERLAYFS_SUPER_MAGIC {
		return Result{}, fmt.Errorf("state dir %s is on overlayfs, which cannot hold an overlay's upper layer; "+
			"use a directory on ext4, xfs or tmpfs (--state-dir)", opt.StateDir)
	}
	if !opt.Keep {
		defer os.RemoveAll(dir)
	}

	group, groupDir, err := cgroups.Create(opt.CgroupPath, cfg.ID, cfg.Limits)
	if err != nil {
		return Result{}, err
	}
	defer group.Remove()
	defer groupDir.Close()

	configR, configW, err := os.Pipe()
	if err != nil {
		return Result{}, err
	}
	syncR, syncW, err := os.Pipe()
	if err != nil {
		return Result{}, err
	}

	cmd := exec.Command("/proc/self/exe", InitArg)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{configR, syncR} // become fds 3 and 4
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: unix.CLONE_NEWUTS | unix.CLONE_NEWPID | unix.CLONE_NEWNS | unix.CLONE_NEWIPC |
			unix.CLONE_NEWNET | unix.CLONE_NEWCGROUP,
		// Start the child already inside its cgroup (clone3 CLONE_INTO_CGROUP), so no
		// instruction runs outside the limits and there is no window to race.
		UseCgroupFD: true,
		CgroupFD:    int(groupDir.Fd()),
		Pdeathsig:   unix.SIGKILL,
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start container: %w", err)
	}
	configR.Close()
	syncR.Close()

	release := func() error {
		defer syncW.Close()
		if err := json.NewEncoder(configW).Encode(cfg); err != nil {
			return err
		}
		configW.Close()
		if cfg.Network.Mode == "cni" {
			if err := attachNetwork(cfg, cmd.Process.Pid); err != nil {
				return err
			}
		}
		_, err := syncW.Write([]byte{1})
		return err
	}
	if err := release(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return Result{}, err
	}
	if cfg.Network.Mode == "cni" {
		defer detachNetwork(cfg)
	}

	// Pass Ctrl-C and termination on to the container's init process.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, unix.SIGINT, unix.SIGTERM, unix.SIGHUP, unix.SIGQUIT)
	defer signal.Stop(signals)
	go func() {
		for s := range signals {
			_ = cmd.Process.Signal(s)
		}
	}()

	err = cmd.Wait()
	res := Result{OOMKilled: group.OOMKills() > 0, PeakMemory: group.PeakMemory()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitCode(exitErr)
	default:
		return res, err
	}
	return res, nil
}

// exitCode follows the shell convention: 128 + signal number for a killed process.
func exitCode(e *exec.ExitError) int {
	if ws, ok := e.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return e.ExitCode()
}

func netnsPath(pid int) string { return fmt.Sprintf("/proc/%d/ns/net", pid) }

func attachNetwork(cfg Config, pid int) error {
	conf, err := os.ReadFile(cfg.Network.CNIConfig)
	if err != nil {
		return err
	}
	_, err = cni.Exec(cni.Invocation{
		Command: "ADD", ContainerID: cfg.ID, NetNS: netnsPath(pid), IfName: "eth0",
		Path: cfg.Network.CNIBinDirs, Config: conf,
	})
	return err
}

// detachNetwork runs CNI DEL after the container exits. By then its network
// namespace is gone, and the veth pair with it, so DEL only has to release the IP
// address. The CNI spec allows an empty CNI_NETNS for exactly this case.
func detachNetwork(cfg Config) {
	conf, err := os.ReadFile(cfg.Network.CNIConfig)
	if err != nil {
		return
	}
	_, _ = cni.Exec(cni.Invocation{
		Command: "DEL", ContainerID: cfg.ID, NetNS: "", IfName: "eth0",
		Path: cfg.Network.CNIBinDirs, Config: conf,
	})
}
