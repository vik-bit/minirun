package seccomp

import "golang.org/x/sys/unix"

// Legacy or x86-only calls with no place in a container.
var archSpecific = map[string]uint32{
	"iopl": unix.SYS_IOPL, "ioperm": unix.SYS_IOPERM,
	"create_module": unix.SYS_CREATE_MODULE, "get_kernel_syms": unix.SYS_GET_KERNEL_SYMS,
	"query_module": unix.SYS_QUERY_MODULE, "nfsservctl": unix.SYS_NFSSERVCTL,
	"uselib": unix.SYS_USELIB, "_sysctl": unix.SYS__SYSCTL, "lookup_dcookie": unix.SYS_LOOKUP_DCOOKIE,
}
