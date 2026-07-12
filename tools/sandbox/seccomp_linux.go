//go:build linux

package sandbox

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"golang.org/x/sys/unix"
)

const (
	seccompDataNR       = 0
	seccompDataArch     = 4
	seccompDataFirstArg = 16
	x32SyscallBit       = 0x40000000
)

// attachUnixSocketFilter passes a classic-BPF seccomp program to bubblewrap.
// The target may use socketpair for private child-process IPC, but it cannot
// create an AF_UNIX socket capable of connecting to filesystem sockets exposed
// by the read-only root bind. io_uring_setup is also denied because modern
// kernels can create and connect sockets through io_uring without SYS_SOCKET.
func attachUnixSocketFilter(cmd *exec.Cmd) (int, error) {
	arch, byteOrder, err := nativeAuditArch()
	if err != nil {
		return 0, err
	}

	filters := []unix.SockFilter{
		// Reject attempts to switch to an ABI this filter did not audit.
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataArch},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: arch},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},

		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataNR},
		// AUDIT_ARCH_X86_64 is shared with the x32 ABI. Deny its tagged
		// syscall numbers instead of letting them bypass the SYS_SOCKET rule.
		{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jf: 1, K: x32SyscallBit},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},

		// io_uring can issue socket/connect operations outside the ordinary
		// syscall path, so disable creation of rings in the sandbox.
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(unix.SYS_IO_URING_SETUP)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EACCES)},

		// Non-socket syscalls skip directly to the final allow.
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 3, K: uint32(unix.SYS_SOCKET)},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataFirstArg},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: unix.AF_UNIX},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EACCES)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}

	f, err := os.CreateTemp("", "pollytool-seccomp-*")
	if err != nil {
		return 0, err
	}
	name := f.Name()
	if err := os.Remove(name); err != nil {
		_ = f.Close()
		return 0, err
	}
	if err := binary.Write(f, byteOrder, filters); err != nil {
		_ = f.Close()
		return 0, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		_ = f.Close()
		return 0, err
	}

	fd := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	return fd, nil
}

func nativeAuditArch() (uint32, binary.ByteOrder, error) {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64, binary.LittleEndian, nil
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64, binary.LittleEndian, nil
	case "386":
		return unix.AUDIT_ARCH_I386, binary.LittleEndian, nil
	case "arm":
		return unix.AUDIT_ARCH_ARM, binary.LittleEndian, nil
	case "ppc64":
		return unix.AUDIT_ARCH_PPC64, binary.BigEndian, nil
	case "ppc64le":
		return unix.AUDIT_ARCH_PPC64LE, binary.LittleEndian, nil
	case "s390x":
		return unix.AUDIT_ARCH_S390X, binary.BigEndian, nil
	case "riscv64":
		return unix.AUDIT_ARCH_RISCV64, binary.LittleEndian, nil
	case "loong64":
		return unix.AUDIT_ARCH_LOONGARCH64, binary.LittleEndian, nil
	case "mips":
		return unix.AUDIT_ARCH_MIPS, binary.BigEndian, nil
	case "mipsle":
		return unix.AUDIT_ARCH_MIPSEL, binary.LittleEndian, nil
	case "mips64":
		return unix.AUDIT_ARCH_MIPS64, binary.BigEndian, nil
	case "mips64le":
		return unix.AUDIT_ARCH_MIPSEL64, binary.LittleEndian, nil
	case "mips64p32":
		return unix.AUDIT_ARCH_MIPS64N32, binary.BigEndian, nil
	case "mips64p32le":
		return unix.AUDIT_ARCH_MIPSEL64N32, binary.LittleEndian, nil
	default:
		return 0, nil, fmt.Errorf("Unix-socket seccomp policy is unsupported on linux/%s", runtime.GOARCH)
	}
}
