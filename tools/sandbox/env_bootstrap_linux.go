//go:build linux

package sandbox

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	linuxEnvBootstrapArg   = "--pollytool-internal-linux-env-bootstrap-v1"
	linuxEnvBootstrapMagic = "pollytool-linux-env-v1"
	linuxEnvPayloadLimit   = 16 << 20
)

// init handles the trusted post-containment re-exec used by the Linux backend.
// The executable is reached through an inherited /proc/self/exe FD rather than
// a mutable pathname. No target environment is installed until this code is
// already running behind bwrap's namespaces, mounts, and seccomp filter.
func init() {
	if len(os.Args) < 7 || os.Args[1] != linuxEnvBootstrapArg {
		return
	}
	bootstrapFD, bootstrapErr := strconv.Atoi(os.Args[2])
	envFD, envErr := strconv.Atoi(os.Args[3])
	authorityFDs, authorityErr := parseLinuxFDList(os.Args[4])
	if bootstrapErr != nil || envErr != nil || bootstrapFD < 3 || envFD < 3 ||
		authorityErr != nil || os.Args[0] != "/proc/self/fd/"+strconv.Itoa(bootstrapFD) {
		return
	}
	if err := runLinuxEnvBootstrap(bootstrapFD, envFD, authorityFDs, os.Args[5], os.Args[6:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "pollytool sandbox bootstrap: %v\n", err)
		os.Exit(126)
	}
	os.Exit(126)
}

func runLinuxEnvBootstrap(bootstrapFD, envFD int, authorityFDs []int, target string, argv []string) error {
	// These two unconsumed descriptors retain their identities through bwrap.
	// Do not close numeric slots for descriptors bwrap already consumed: the
	// loader or Go runtime may have reused those numbers before package init.
	_ = syscall.Close(bootstrapFD)
	for _, fd := range authorityFDs {
		_ = syscall.Close(fd)
	}

	envFile := os.NewFile(uintptr(envFD), "pollytool-target-environment")
	if envFile == nil {
		return fmt.Errorf("invalid target environment descriptor")
	}
	data, readErr := io.ReadAll(io.LimitReader(envFile, linuxEnvPayloadLimit+1))
	closeErr := envFile.Close()
	if readErr != nil {
		return fmt.Errorf("read target environment: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close target environment: %w", closeErr)
	}
	if len(data) > linuxEnvPayloadLimit {
		return fmt.Errorf("target environment payload exceeds limit")
	}
	env, err := parseLinuxTargetEnvPayload(data)
	if err != nil {
		return err
	}
	if target == "" || len(argv) == 0 {
		return fmt.Errorf("missing target executable or argument vector")
	}
	if err := syscall.Exec(target, argv, env); err != nil {
		return fmt.Errorf("exec target: %w", err)
	}
	return fmt.Errorf("exec target returned unexpectedly")
}

func formatLinuxFDList(fds []int) string {
	if len(fds) == 0 {
		return "-"
	}
	parts := make([]string, len(fds))
	for i, fd := range fds {
		parts[i] = strconv.Itoa(fd)
	}
	return strings.Join(parts, ",")
}

func parseLinuxFDList(value string) ([]int, error) {
	if value == "-" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	fds := make([]int, len(parts))
	for i, part := range parts {
		fd, err := strconv.Atoi(part)
		if err != nil || fd < 3 {
			return nil, fmt.Errorf("invalid sandbox descriptor list")
		}
		fds[i] = fd
	}
	return fds, nil
}

func attachLinuxTargetEnvironment(cmd *exec.Cmd, env []string) (int, error) {
	payload, err := linuxTargetEnvPayload(env)
	if err != nil {
		return 0, err
	}
	memfd, err := unix.MemfdCreate("pollytool-target-env", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return 0, err
	}
	f := os.NewFile(uintptr(memfd), "pollytool-target-env")
	if f == nil {
		_ = unix.Close(memfd)
		return 0, fmt.Errorf("create target environment descriptor")
	}
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		return 0, err
	}
	seals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("seal target environment descriptor: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		_ = f.Close()
		return 0, err
	}
	fd := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	return fd, nil
}

func linuxTargetEnvPayload(env []string) ([]byte, error) {
	last := make(map[string]int, len(env))
	for i, entry := range env {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" || strings.ContainsRune(name, '=') || strings.IndexByte(name, 0) >= 0 || strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("invalid target environment entry for %q", name)
		}
		last[name] = i
	}
	var payload strings.Builder
	payload.WriteString(linuxEnvBootstrapMagic)
	payload.WriteByte(0)
	for i, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if last[name] != i {
			continue
		}
		payload.WriteString(entry)
		payload.WriteByte(0)
	}
	return []byte(payload.String()), nil
}

func parseLinuxTargetEnvPayload(data []byte) ([]string, error) {
	parts := strings.Split(string(data), "\x00")
	if len(parts) < 2 || parts[0] != linuxEnvBootstrapMagic || parts[len(parts)-1] != "" {
		return nil, fmt.Errorf("invalid target environment payload")
	}
	env := append([]string(nil), parts[1:len(parts)-1]...)
	for _, entry := range env {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" || strings.ContainsRune(name, '=') || strings.IndexByte(name, 0) >= 0 || strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("invalid target environment entry for %q", name)
		}
	}
	return env, nil
}
