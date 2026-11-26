//go:build unix && !darwin && !freebsd && !dragonfly && !linux

package main

import "net"

func verifyPeerUID(*net.UnixConn) error { return nil }
