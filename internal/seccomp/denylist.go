package seccomp

import "golang.org/x/sys/unix"

// common is the part of the deny list that exists on every supported architecture.
// Each entry is a way to affect the host or escape the container's view of it.
var common = map[string]uint32{
	// Loading kernel code or rebooting.
	"init_module": unix.SYS_INIT_MODULE, "finit_module": unix.SYS_FINIT_MODULE,
	"delete_module": unix.SYS_DELETE_MODULE, "kexec_load": unix.SYS_KEXEC_LOAD,
	"kexec_file_load": unix.SYS_KEXEC_FILE_LOAD, "reboot": unix.SYS_REBOOT,
	"bpf": unix.SYS_BPF, "perf_event_open": unix.SYS_PERF_EVENT_OPEN,
	// Changing the filesystem view, through both the old and the new mount API.
	"mount": unix.SYS_MOUNT, "umount2": unix.SYS_UMOUNT2, "pivot_root": unix.SYS_PIVOT_ROOT,
	"fsopen": unix.SYS_FSOPEN, "fsmount": unix.SYS_FSMOUNT, "fsconfig": unix.SYS_FSCONFIG,
	"move_mount": unix.SYS_MOVE_MOUNT, "open_tree": unix.SYS_OPEN_TREE,
	"open_by_handle_at": unix.SYS_OPEN_BY_HANDLE_AT, "name_to_handle_at": unix.SYS_NAME_TO_HANDLE_AT,
	"swapon": unix.SYS_SWAPON, "swapoff": unix.SYS_SWAPOFF, "quotactl": unix.SYS_QUOTACTL,
	// Entering or creating namespaces (clone is checked by flags instead).
	"unshare": unix.SYS_UNSHARE, "setns": unix.SYS_SETNS,
	// Host-wide state: clocks, keyrings, process accounting.
	"settimeofday": unix.SYS_SETTIMEOFDAY, "clock_settime": unix.SYS_CLOCK_SETTIME,
	"clock_adjtime": unix.SYS_CLOCK_ADJTIME, "adjtimex": unix.SYS_ADJTIMEX,
	"add_key": unix.SYS_ADD_KEY, "request_key": unix.SYS_REQUEST_KEY, "keyctl": unix.SYS_KEYCTL,
	"acct": unix.SYS_ACCT, "userfaultfd": unix.SYS_USERFAULTFD,
}

// Denied lists the names of every syscall the filter refuses, for tests and docs.
func Denied() map[string]uint32 {
	out := make(map[string]uint32, len(common)+len(archSpecific))
	for k, v := range common {
		out[k] = v
	}
	for k, v := range archSpecific {
		out[k] = v
	}
	return out
}

func denied() []uint32 {
	m := Denied()
	out := make([]uint32, 0, len(m))
	for _, nr := range m {
		out = append(out, nr)
	}
	return out
}
