package seccomp

import (
	"encoding/binary"
	"runtime"
	"testing"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// run executes the filter in golang.org/x/net/bpf's interpreter. That VM reads
// packet data big-endian while the kernel reads seccomp_data in native order, so
// the fields are written big-endian here to get the same values the kernel sees.
func run(t *testing.T, prog []bpf.RawInstruction, arch, nr, arg0 uint32) Action {
	t.Helper()
	insns := make([]bpf.Instruction, len(prog))
	for i, raw := range prog {
		insns[i] = raw.Disassemble()
	}
	vm, err := bpf.NewVM(insns)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 64)
	binary.BigEndian.PutUint32(data[offNr:], nr)
	binary.BigEndian.PutUint32(data[offArch:], arch)
	binary.BigEndian.PutUint32(data[offArg0Low:], arg0)
	out, err := vm.Run(data)
	if err != nil {
		t.Fatal(err)
	}
	return Action(out)
}

func program(t *testing.T) []bpf.RawInstruction {
	t.Helper()
	prog, err := Program()
	if err != nil {
		t.Fatal(err)
	}
	return prog
}

func TestDeniedSyscallsReturnEPERM(t *testing.T) {
	prog, arch := program(t), auditArch[runtime.GOARCH]
	for name, nr := range Denied() {
		if got := run(t, prog, arch, nr, 0); got != DenyEPERM {
			t.Errorf("%s (%d): got %#x, want EPERM", name, nr, got)
		}
	}
}

func TestOrdinarySyscallsAreAllowed(t *testing.T) {
	prog, arch := program(t), auditArch[runtime.GOARCH]
	for name, nr := range map[string]uint32{"read": unix.SYS_READ, "write": unix.SYS_WRITE,
		"openat": unix.SYS_OPENAT, "execve": unix.SYS_EXECVE, "exit_group": unix.SYS_EXIT_GROUP,
		"ptrace": unix.SYS_PTRACE, "socket": unix.SYS_SOCKET} {
		if got := run(t, prog, arch, nr, 0); got != Allow {
			t.Errorf("%s: got %#x, want allow", name, got)
		}
	}
}

func TestCloneIsCheckedByFlags(t *testing.T) {
	prog, arch := program(t), auditArch[runtime.GOARCH]
	threadFlags := uint32(unix.CLONE_VM | unix.CLONE_FS | unix.CLONE_FILES | unix.CLONE_SIGHAND | unix.CLONE_THREAD)
	if got := run(t, prog, arch, unix.SYS_CLONE, threadFlags); got != Allow {
		t.Errorf("clone for a thread: got %#x, want allow", got)
	}
	for _, flag := range []uint32{unix.CLONE_NEWUSER, unix.CLONE_NEWNS, unix.CLONE_NEWNET, unix.CLONE_NEWPID} {
		if got := run(t, prog, arch, unix.SYS_CLONE, threadFlags|flag); got != DenyEPERM {
			t.Errorf("clone with namespace flag %#x: got %#x, want EPERM", flag, got)
		}
	}
	if got := run(t, prog, arch, unix.SYS_CLONE3, 0); got != DenyENOSYS {
		t.Errorf("clone3: got %#x, want ENOSYS so libc falls back to clone", got)
	}
}

func TestOtherArchitecturesAreKilled(t *testing.T) {
	prog := program(t)
	const auditArchI386 = 0x40000003
	if got := run(t, prog, auditArchI386, unix.SYS_READ, 0); got != KillProcess {
		t.Errorf("i386 syscall: got %#x, want kill", got)
	}
}

func TestX32SyscallsAreDenied(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("x32 exists only on x86-64")
	}
	if got := run(t, program(t), auditArch["amd64"], x32SyscallBit|unix.SYS_READ, 0); got != DenyEPERM {
		t.Errorf("x32 read: got %#x, want EPERM", got)
	}
}
