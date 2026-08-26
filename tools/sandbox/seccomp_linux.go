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
	seccompDataNR        = 0
	seccompDataArch      = 4
	seccompDataFirstArg  = 16
	seccompDataSecondArg = 24
	x32SyscallBit        = 0x40000000
	socketTypeMask       = 0xf
)

// attachUnixSocketFilter passes a classic-BPF seccomp program to bubblewrap.
// Filesystem AF_UNIX sockets, AF_UNIX socketpairs, and io_uring socket creation
// are denied even when ordinary network access is enabled. When networking is
// disabled, all socket creation is denied as a defense in depth against address
// families such as AF_VSOCK that are not isolated by a network namespace.
// allowUnixStream (set when the config grants Unix sockets) additionally
// permits socket(AF_UNIX, SOCK_STREAM); which endpoints are then reachable is
// bounded by the mount namespace, since seccomp cannot inspect connect()'s
// sockaddr path.
func attachUnixSocketFilter(cmd *exec.Cmd, allowNetwork, allowUnixStream bool) (int, error) {
	arch, byteOrder, err := nativeAuditArch()
	if err != nil {
		return 0, err
	}
	filters := socketFilterProgram(arch, allowNetwork, allowUnixStream)

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

func socketFilterProgram(arch uint32, allowNetwork, allowUnixStream bool) []unix.SockFilter {
	deny := uint32(unix.SECCOMP_RET_ERRNO | uint32(unix.EACCES))
	filters := []unix.SockFilter{
		// Reject attempts to switch to an ABI this filter did not audit.
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataArch},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: arch},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},

		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataNR},
	}
	if runtime.GOARCH == "amd64" {
		// AUDIT_ARCH_X86_64 is shared with the x32 ABI. Deny its tagged
		// syscall numbers instead of letting them bypass the SYS_SOCKET rule.
		filters = append(filters,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jf: 1, K: x32SyscallBit},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS})
	}

	filters = append(filters,
		// io_uring can issue socket/connect operations outside ordinary syscalls.
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(unix.SYS_IO_URING_SETUP)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny})

	if !allowNetwork {
		if allowUnixStream {
			// socket and socketpair share one policy here: allowed only for
			// AF_UNIX SOCK_STREAM (after masking the type flags); every other
			// family and type is denied. Everything else falls through to
			// allow.
			return append(filters,
				unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: uint32(unix.SYS_SOCKET)},
				unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 6, K: uint32(unix.SYS_SOCKETPAIR)},
				unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataFirstArg},
				unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 3, K: unix.AF_UNIX},
				unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataSecondArg},
				unix.SockFilter{Code: unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, K: socketTypeMask},
				unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: unix.SOCK_STREAM},
				unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
				unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
		}
		return append(filters,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(unix.SYS_SOCKET)},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 6, K: uint32(unix.SYS_SOCKETPAIR)},
			unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataFirstArg},
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 3, K: unix.AF_UNIX},
			unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataSecondArg},
			unix.SockFilter{Code: unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, K: socketTypeMask},
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: unix.SOCK_STREAM},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	}

	if allowUnixStream {
		// With a Unix-socket grant, socket() and socketpair() coincide:
		// AF_VSOCK denied, AF_UNIX allowed only as SOCK_STREAM, every other
		// family allowed. Route both syscalls into the shared family/type
		// check, which is the unchanged socketpair block below.
		return append(filters,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: uint32(unix.SYS_SOCKET)},
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 8, K: uint32(unix.SYS_SOCKETPAIR)},
			unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataFirstArg},
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: unix.AF_VSOCK},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 4, K: unix.AF_UNIX},
			unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataSecondArg},
			unix.SockFilter{Code: unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, K: socketTypeMask},
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: unix.SOCK_STREAM},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	}

	return append(filters,
		// socket(AF_UNIX/AF_VSOCK) is always denied. AF_UNIX stream
		// socketpairs remain available for private child IPC, while reconnectable
		// datagram/seqpacket pairs are denied.
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: uint32(unix.SYS_SOCKET)},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 4, Jf: 12, K: uint32(unix.SYS_SOCKETPAIR)},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataFirstArg},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: unix.AF_UNIX},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 9, K: unix.AF_VSOCK},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataFirstArg},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: unix.AF_VSOCK},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 4, K: unix.AF_UNIX},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataSecondArg},
		unix.SockFilter{Code: unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, K: socketTypeMask},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: unix.SOCK_STREAM},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
}

func nativeAuditArch() (uint32, binary.ByteOrder, error) {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64, binary.LittleEndian, nil
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64, binary.LittleEndian, nil
	case "arm":
		return unix.AUDIT_ARCH_ARM, binary.LittleEndian, nil
	case "riscv64":
		return unix.AUDIT_ARCH_RISCV64, binary.LittleEndian, nil
	case "loong64":
		return unix.AUDIT_ARCH_LOONGARCH64, binary.LittleEndian, nil
	case "386", "ppc64", "ppc64le", "s390x", "mips", "mipsle", "mips64", "mips64le", "mips64p32", "mips64p32le":
		return 0, nil, fmt.Errorf("Unix-socket seccomp policy is unsupported on linux/%s because socketcall cannot be filtered safely", runtime.GOARCH)
	default:
		return 0, nil, fmt.Errorf("Unix-socket seccomp policy is unsupported on linux/%s", runtime.GOARCH)
	}
}
