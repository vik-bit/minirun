// Package seccomp builds and installs a syscall filter as a raw classic-BPF program.
//
// The kernel runs the filter on every system call the container makes. It sees a
// struct seccomp_data:
//
//	offset 0: int   nr                  (syscall number)
//	offset 4: u32   arch                (AUDIT_ARCH_*)
//	offset 8: u64   instruction_pointer
//	offset 16: u64  args[6]
//
// and returns an action. This filter allows everything except a deny list modelled
// on Docker's default profile: kernel module loading, mounting, namespace creation,
// keyrings, clock changes, kexec, BPF and similar calls that could reach outside the
// container or change the host.
package seccomp

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// Return actions from linux/seccomp.h.
const (
	retKillProcess = 0x80000000
	retErrno       = 0x00050000
	retAllow       = 0x7fff0000

	offNr      = 0
	offArch    = 4
	offArg0Low = 16 // low 32 bits of args[0]; amd64 and arm64 are little-endian

	x32SyscallBit = 0x40000000

	// Namespace flags a container must not pass to clone(2).
	cloneNamespaceFlags = unix.CLONE_NEWNS | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWUSER |
		unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWCGROUP
)

// Action is what the filter decides for one syscall.
type Action uint32

const (
	Allow       Action = retAllow
	DenyEPERM   Action = retErrno | Action(unix.EPERM)
	DenyENOSYS  Action = retErrno | Action(unix.ENOSYS)
	KillProcess Action = retKillProcess
)

// Program assembles the filter for this machine's architecture.
func Program() ([]bpf.RawInstruction, error) {
	arch, ok := auditArch[runtime.GOARCH]
	if !ok {
		return nil, fmt.Errorf("seccomp: unsupported architecture %s", runtime.GOARCH)
	}
	return build(arch, denied(), unix.SYS_CLONE, unix.SYS_CLONE3)
}

func build(arch uint32, deny []uint32, sysClone, sysClone3 uint32) ([]bpf.RawInstruction, error) {
	prog := []bpf.Instruction{
		// A filter for one architecture must reject the others: a 32-bit syscall
		// number means something different, so the deny list would not apply.
		bpf.LoadAbsolute{Off: offArch, Size: 4},
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: arch, SkipTrue: 1},
		bpf.RetConstant{Val: retKillProcess},
		bpf.LoadAbsolute{Off: offNr, Size: 4},
	}
	if arch == auditArch["amd64"] {
		// x32 syscalls reuse the x86-64 table with bit 30 set, which would slip
		// past every comparison below.
		prog = append(prog,
			bpf.JumpIf{Cond: bpf.JumpGreaterOrEqual, Val: x32SyscallBit, SkipFalse: 1},
			bpf.RetConstant{Val: uint32(DenyEPERM)},
		)
	}
	// clone3 passes its flags in a struct the filter cannot read, so it is refused
	// with ENOSYS. C libraries then fall back to clone, whose flags we can check.
	prog = append(prog,
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: sysClone3, SkipFalse: 1},
		bpf.RetConstant{Val: uint32(DenyENOSYS)},
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: sysClone, SkipFalse: 4},
		bpf.LoadAbsolute{Off: offArg0Low, Size: 4},
		bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: cloneNamespaceFlags, SkipFalse: 1},
		bpf.RetConstant{Val: uint32(DenyEPERM)},
		bpf.RetConstant{Val: retAllow},
	)
	for _, nr := range deny {
		prog = append(prog,
			bpf.JumpIf{Cond: bpf.JumpEqual, Val: nr, SkipFalse: 1},
			bpf.RetConstant{Val: uint32(DenyEPERM)},
		)
	}
	prog = append(prog, bpf.RetConstant{Val: retAllow})
	return bpf.Assemble(prog)
}

// Install loads the filter into the calling process. It sets no_new_privs, which
// the kernel requires before an unprivileged filter, and syncs the filter to every
// thread (Go programs are always multi-threaded). The filter survives execve.
func Install() error {
	raw, err := Program()
	if err != nil {
		return err
	}
	filter := make([]unix.SockFilter, len(raw))
	for i, ins := range raw {
		filter[i] = unix.SockFilter{Code: ins.Op, Jt: ins.Jt, Jf: ins.Jf, K: ins.K}
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("seccomp: no_new_privs: %w", err)
	}
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&prog)))
	runtime.KeepAlive(filter)
	if errno != 0 {
		return fmt.Errorf("seccomp: install filter: %w", errno)
	}
	return nil
}

var auditArch = map[string]uint32{
	"amd64": 0xc000003e, // AUDIT_ARCH_X86_64
	"arm64": 0xc00000b7, // AUDIT_ARCH_AARCH64
}
