//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// This kernel property is why the seccomp exception is limited to private
// pairs. Check real, listening filesystem and abstract endpoints, both before
// and after the peer closes, including an attempted AF_UNSPEC disconnect.
func TestLinuxSequencedPacketPairsCannotReconnect(t *testing.T) {
	dir, err := os.MkdirTemp("", "polly-seq-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	for _, name := range []string{filepath.Join(dir, "listener"), "\x00" + filepath.Join(dir, "abstract")} {
		t.Run(name, func(t *testing.T) {
			listener, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(listener)
			address := &unix.SockaddrUnix{Name: name}
			if err := unix.Bind(listener, address); err != nil {
				t.Fatal(err)
			}
			if err := unix.Listen(listener, 1); err != nil {
				t.Fatal(err)
			}
			pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(pair[0])
			defer func() {
				if pair[1] >= 0 {
					unix.Close(pair[1])
				}
			}()
			// A destination on sendto must not redirect an already-connected pair.
			if err := unix.Sendto(pair[0], []byte("private"), unix.MSG_NOSIGNAL, address); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 16)
			if n, _, err := unix.Recvfrom(pair[1], buf, 0); err != nil || string(buf[:n]) != "private" {
				t.Fatalf("message left its private pair: %q %v", buf, err)
			}
			for _, closePeer := range []bool{false, true} {
				if closePeer {
					unix.Close(pair[1])
					pair[1] = -1
				}
				unspec := unix.RawSockaddr{Family: unix.AF_UNSPEC}
				_, _, errno := unix.Syscall(unix.SYS_CONNECT, uintptr(pair[0]), uintptr(unsafe.Pointer(&unspec)), unsafe.Sizeof(unspec))
				if errno == 0 {
					t.Fatal("private pair disconnected")
				}
				if err := unix.Connect(pair[0], address); err != unix.EISCONN {
					t.Fatalf("private pair could reconnect: %v", err)
				}
			}
			if fd, _, err := unix.Accept(listener); err != unix.EAGAIN {
				if err == nil {
					unix.Close(fd)
				}
				t.Fatalf("listener received an unexpected connection: %v", err)
			}
		})
	}
}
