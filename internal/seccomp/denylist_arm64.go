package seccomp

import "golang.org/x/sys/unix"

var archSpecific = map[string]uint32{
	"lookup_dcookie": unix.SYS_LOOKUP_DCOOKIE, "nfsservctl": unix.SYS_NFSSERVCTL,
}
