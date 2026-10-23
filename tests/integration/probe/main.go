// Command probe runs inside a test container and reports what the kernel allows,
// one check per invocation, as "ok" or the error name (e.g. EPERM).
//
//	probe caps          print the bounding and effective capability masks
//	probe mount         try to mount a tmpfs
//	probe unshare-user  try unshare(CLONE_NEWUSER), which needs no capability
//	probe clone-newuser try clone(CLONE_NEWUSER) by starting a child in a new user ns
//	probe keyctl        try to read the session keyring
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: probe caps|mount|unshare-user|clone-newuser|keyctl")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "noop":
		return
	case "caps":
		status, _ := os.ReadFile("/proc/self/status")
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "CapBnd:") || strings.HasPrefix(line, "CapEff:") {
				fmt.Println(strings.Join(strings.Fields(line), " "))
			}
		}
	case "mount":
		report(unix.Mount("tmpfs", "/mnt", "tmpfs", 0, ""))
	case "unshare-user":
		// The kernel refuses unshare(CLONE_NEWUSER) with EINVAL in a multi-threaded
		// process, and every Go process is one. Go's fork/exec makes the call in the
		// freshly forked, single-threaded child instead.
		cmd := exec.Command("/proc/self/exe", "noop")
		cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: unix.CLONE_NEWUSER}
		report(cmd.Run())
	case "clone-newuser":
		// Go starts this child with plain clone(2), passing the flag where the
		// seccomp filter can see it.
		cmd := exec.Command("/proc/self/exe", "noop")
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER}
		report(cmd.Run())
	case "keyctl":
		_, err := unix.KeyctlInt(unix.KEYCTL_GET_KEYRING_ID, unix.KEY_SPEC_SESSION_KEYRING, 0, 0, 0)
		report(err)
	default:
		fmt.Println("unknown probe", os.Args[1])
		os.Exit(2)
	}
}

func report(err error) {
	var errno syscall.Errno
	switch {
	case err == nil:
		fmt.Println("ok")
	case errors.As(err, &errno):
		fmt.Println(unix.ErrnoName(errno))
	default:
		fmt.Println("error:", err)
	}
}
