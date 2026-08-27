//go:build linux

package sandbox

import (
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

type seccompTestData struct {
	nr   uint32
	arch uint32
	arg0 uint32
	arg1 uint32
}

// evalSocketFilter interprets the classic-BPF subset socketFilterProgram
// emits, so every hand-derived jump offset is proven against the full
// decision matrix without a kernel.
func evalSocketFilter(t *testing.T, filters []unix.SockFilter, data seccompTestData) uint32 {
	t.Helper()
	load := func(offset uint32) uint32 {
		switch offset {
		case seccompDataNR:
			return data.nr
		case seccompDataArch:
			return data.arch
		case seccompDataFirstArg:
			return data.arg0
		case seccompDataSecondArg:
			return data.arg1
		default:
			t.Fatalf("filter loads unexpected seccomp_data offset %d", offset)
			return 0
		}
	}
	var acc uint32
	for pc := 0; pc < len(filters); pc++ {
		ins := filters[pc]
		switch ins.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			acc = load(ins.K)
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if acc == ins.K {
				pc += int(ins.Jt)
			} else {
				pc += int(ins.Jf)
			}
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			if acc&ins.K != 0 {
				pc += int(ins.Jt)
			} else {
				pc += int(ins.Jf)
			}
		case unix.BPF_ALU | unix.BPF_AND | unix.BPF_K:
			acc &= ins.K
		case unix.BPF_RET | unix.BPF_K:
			return ins.K
		default:
			t.Fatalf("filter uses unhandled BPF opcode %#x", ins.Code)
		}
	}
	t.Fatal("filter fell off the end of the program")
	return 0
}

func TestSocketFilterProgramDecisions(t *testing.T) {
	arch, _, err := nativeAuditArch()
	if err != nil {
		t.Skipf("seccomp policy unsupported on this architecture: %v", err)
	}
	allow := uint32(unix.SECCOMP_RET_ALLOW)
	deny := uint32(unix.SECCOMP_RET_ERRNO | uint32(unix.EACCES))
	kill := uint32(unix.SECCOMP_RET_KILL_PROCESS)

	socket := func(family, kind uint32) seccompTestData {
		return seccompTestData{nr: uint32(unix.SYS_SOCKET), arch: arch, arg0: family, arg1: kind}
	}
	socketpair := func(family, kind uint32) seccompTestData {
		return seccompTestData{nr: uint32(unix.SYS_SOCKETPAIR), arch: arch, arg0: family, arg1: kind}
	}
	constant := func(want uint32) func(network, unixStream bool) uint32 {
		return func(bool, bool) uint32 { return want }
	}
	unixStreamGated := func(_, unixStream bool) uint32 {
		if unixStream {
			return allow
		}
		return deny
	}
	networkGated := func(network, _ bool) uint32 {
		if network {
			return allow
		}
		return deny
	}
	calls := []struct {
		name string
		data seccompTestData
		want func(network, unixStream bool) uint32
	}{
		{"socket unix stream", socket(unix.AF_UNIX, unix.SOCK_STREAM), unixStreamGated},
		{"socket unix stream with flags", socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK), unixStreamGated},
		{"socket unix dgram", socket(unix.AF_UNIX, unix.SOCK_DGRAM), constant(deny)},
		{"socket unix seqpacket", socket(unix.AF_UNIX, unix.SOCK_SEQPACKET), constant(deny)},
		{"socket vsock stream", socket(unix.AF_VSOCK, unix.SOCK_STREAM), constant(deny)},
		{"socket inet stream", socket(unix.AF_INET, unix.SOCK_STREAM), networkGated},
		{"socket inet6 dgram", socket(unix.AF_INET6, unix.SOCK_DGRAM), networkGated},
		{"socketpair unix stream", socketpair(unix.AF_UNIX, unix.SOCK_STREAM), constant(allow)},
		{"socketpair unix stream with flags", socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC), constant(allow)},
		{"socketpair unix dgram", socketpair(unix.AF_UNIX, unix.SOCK_DGRAM), constant(deny)},
		{"socketpair vsock stream", socketpair(unix.AF_VSOCK, unix.SOCK_STREAM), constant(deny)},
		{"socketpair inet stream", socketpair(unix.AF_INET, unix.SOCK_STREAM), networkGated},
		{"io_uring_setup", seccompTestData{nr: uint32(unix.SYS_IO_URING_SETUP), arch: arch}, constant(deny)},
		{"unrelated syscall", seccompTestData{nr: uint32(unix.SYS_READ), arch: arch}, constant(allow)},
		{"foreign arch", seccompTestData{nr: uint32(unix.SYS_SOCKET), arch: arch + 1}, constant(kill)},
	}

	for _, network := range []bool{false, true} {
		for _, unixStream := range []bool{false, true} {
			filters := socketFilterProgram(arch, network, unixStream)
			for _, call := range calls {
				got := evalSocketFilter(t, filters, call.data)
				if want := call.want(network, unixStream); got != want {
					t.Errorf("network=%t unixStream=%t %s: verdict = %#x, want %#x", network, unixStream, call.name, got, want)
				}
			}
			if runtime.GOARCH == "amd64" {
				data := socket(unix.AF_INET, unix.SOCK_STREAM)
				data.nr |= x32SyscallBit
				if got := evalSocketFilter(t, filters, data); got != kill {
					t.Errorf("network=%t unixStream=%t x32-tagged socket: verdict = %#x, want KILL_PROCESS", network, unixStream, got)
				}
			}
		}
	}
}
