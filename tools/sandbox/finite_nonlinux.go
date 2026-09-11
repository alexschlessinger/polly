//go:build unix && !linux

package sandbox

func finiteNamespaceCancellation(Sandbox) bool { return false }
