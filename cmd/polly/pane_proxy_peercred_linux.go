//go:build linux

package main

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func verifyPeerUID(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var optErr error
	if err := raw.Control(func(fd uintptr) {
		cred, optErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if optErr != nil {
		return optErr
	}
	if want := uint32(os.Geteuid()); cred.Uid != want {
		return fmt.Errorf("socket peer uid %d does not match %d", cred.Uid, want)
	}
	return nil
}
