// Package caps reduces a process's Linux capabilities to Docker's default set.
//
// A container's process runs as uid 0, which without this would hold every
// capability, including CAP_SYS_ADMIN (mount, most namespace operations) and
// CAP_SYS_MODULE. Capabilities are per thread, so the caller must lock its OS
// thread and exec from the same thread.
package caps

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Default is Docker's default capability set.
var Default = []int{
	unix.CAP_CHOWN, unix.CAP_DAC_OVERRIDE, unix.CAP_FSETID, unix.CAP_FOWNER, unix.CAP_MKNOD,
	unix.CAP_NET_RAW, unix.CAP_SETGID, unix.CAP_SETUID, unix.CAP_SETFCAP, unix.CAP_SETPCAP,
	unix.CAP_NET_BIND_SERVICE, unix.CAP_SYS_CHROOT, unix.CAP_KILL, unix.CAP_AUDIT_WRITE,
}

// Restrict drops every capability not in keep from the bounding, permitted,
// effective and inheritable sets and clears the ambient set. The bounding set is
// what limits a root process after execve, so it is the one that matters most.
func Restrict(keep []int) error {
	last, err := lastCap()
	if err != nil {
		return err
	}
	allowed := map[int]bool{}
	for _, c := range keep {
		allowed[c] = true
	}
	for c := 0; c <= last; c++ {
		if !allowed[c] {
			if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil {
				return fmt.Errorf("caps: drop %d from bounding set: %w", c, err)
			}
		}
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("caps: clear ambient set: %w", err)
	}
	var data [2]unix.CapUserData
	for c := range allowed {
		data[c/32].Effective |= 1 << (uint(c) % 32)
	}
	for i := range data {
		data[i].Permitted = data[i].Effective
		data[i].Inheritable = data[i].Effective
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("caps: capset: %w", err)
	}
	return nil
}

func lastCap() (int, error) {
	b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}
