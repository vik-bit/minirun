package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/vik-bit/minirun/internal/caps"
	"github.com/vik-bit/minirun/internal/seccomp"
	"golang.org/x/sys/unix"
)

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Init runs inside the new namespaces as the container's PID 1. It never returns
// on success: the last step replaces this process with the user's command.
func Init() error {
	// Capabilities are per thread and execve keeps only the calling thread, so the
	// thread that drops them must be the one that execs.
	runtime.LockOSThread()

	var cfg Config
	if err := json.NewDecoder(os.NewFile(fdConfig, "config")).Decode(&cfg); err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	sync := os.NewFile(fdSync, "sync")
	if n, err := sync.Read(make([]byte, 1)); n != 1 {
		return fmt.Errorf("parent did not release the container: %v", err)
	}
	sync.Close()

	if err := setupRoot(cfg); err != nil {
		return err
	}
	if err := unix.Sethostname([]byte(cfg.Hostname)); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}
	if err := setupLoopback(); err != nil {
		return err
	}
	if err := caps.Restrict(caps.Default); err != nil {
		return err
	}
	if cfg.Seccomp {
		if err := seccomp.Install(); err != nil {
			return err
		}
	}

	env := append([]string{"PATH=" + defaultPath, "HOSTNAME=" + cfg.Hostname, "HOME=/root"}, cfg.Env...)
	_ = os.Setenv("PATH", defaultPath)
	bin, err := exec.LookPath(cfg.Args[0])
	if err != nil {
		return err
	}
	return unix.Exec(bin, cfg.Args, env)
}

// setupRoot builds the container's filesystem and makes it "/".
func setupRoot(cfg Config) error {
	// Mounts made below must not propagate back to the host's mount namespace.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", cfg.Image, cfg.Upper, cfg.Work)
	if err := unix.Mount("overlay", cfg.Merged, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay root: %w", err)
	}
	root := cfg.Merged

	type mnt struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
	}
	const nosuid, nodev, noexec = unix.MS_NOSUID, unix.MS_NODEV, unix.MS_NOEXEC
	for _, m := range []mnt{
		// A fresh proc for the new PID namespace shows only the container's processes.
		{"proc", "/proc", "proc", nosuid | nodev | noexec, ""},
		{"tmpfs", "/dev", "tmpfs", nosuid, "mode=755,size=65536k"},
		{"tmpfs", "/dev/shm", "tmpfs", nosuid | nodev | noexec, "mode=1777,size=65536k"},
		{"devpts", "/dev/pts", "devpts", nosuid | noexec, "newinstance,ptmxmode=0666,mode=620"},
		{"sysfs", "/sys", "sysfs", nosuid | nodev | noexec | unix.MS_RDONLY, ""},
		{"tmpfs", "/tmp", "tmpfs", nosuid | nodev, "mode=1777"},
	} {
		target := filepath.Join(root, m.target)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(m.source, target, m.fstype, m.flags, m.data); err != nil {
			return fmt.Errorf("mount %s on %s: %w", m.fstype, m.target, err)
		}
	}
	if err := makeDevices(filepath.Join(root, "dev")); err != nil {
		return err
	}
	if err := maskProc(root); err != nil {
		return err
	}

	// pivot_root, unlike chroot, actually detaches the old root, so a process that
	// escapes the chroot-style jail has nothing to walk back to.
	old := filepath.Join(root, ".old_root")
	if err := os.MkdirAll(old, 0o700); err != nil {
		return err
	}
	if err := unix.PivotRoot(root, old); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	if err := unix.Unmount("/.old_root", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	return os.Remove("/.old_root")
}

// makeDevices creates the handful of device nodes programs expect. Nothing else
// from the host's /dev is visible.
func makeDevices(dev string) error {
	for _, d := range []struct {
		name         string
		major, minor uint32
	}{{"null", 1, 3}, {"zero", 1, 5}, {"full", 1, 7}, {"random", 1, 8}, {"urandom", 1, 9}, {"tty", 5, 0}} {
		path := filepath.Join(dev, d.name)
		if err := unix.Mknod(path, unix.S_IFCHR|0o666, int(unix.Mkdev(d.major, d.minor))); err != nil {
			return fmt.Errorf("mknod %s: %w", path, err)
		}
		// mknod honours the umask; set the mode explicitly.
		if err := os.Chmod(path, 0o666); err != nil {
			return err
		}
	}
	for link, target := range map[string]string{"fd": "/proc/self/fd", "stdin": "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2", "ptmx": "pts/ptmx"} {
		if err := os.Symlink(target, filepath.Join(dev, link)); err != nil {
			return err
		}
	}
	return nil
}

// maskProc hides /proc files that leak host information or allow host changes,
// as Docker does: kernel memory and keys are covered with an empty file, and the
// sysctl tree is made read-only.
func maskProc(root string) error {
	for _, p := range []string{"/proc/kcore", "/proc/keys", "/proc/timer_list", "/proc/sched_debug"} {
		target := filepath.Join(root, p)
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := unix.Mount("/dev/null", target, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("mask %s: %w", p, err)
		}
	}
	for _, p := range []string{"/proc/sys", "/proc/sysrq-trigger", "/proc/irq", "/proc/bus"} {
		target := filepath.Join(root, p)
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := unix.Mount(target, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("bind %s: %w", p, err)
		}
		if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("make %s read-only: %w", p, err)
		}
	}
	return nil
}

// setupLoopback brings up lo, so a container with no network can still talk to
// itself on 127.0.0.1.
func setupLoopback() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("bring up loopback: %w", err)
	}
	return nil
}
