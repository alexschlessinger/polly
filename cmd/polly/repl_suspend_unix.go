//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package main

import "syscall"

// suspendCurrentProcessGroup mirrors the job-control signal a terminal driver
// normally sends for Ctrl-Z. Signaling the group also pauses any active tool
// subprocesses; an interactive shell resumes the same group with `fg`.
func suspendCurrentProcessGroup() error {
	return syscall.Kill(-syscall.Getpgrp(), syscall.SIGTSTP)
}
