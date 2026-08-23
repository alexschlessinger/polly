//go:build linux

package sandbox

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	linuxEnvBootstrapArg                 = "--pollytool-internal-linux-env-bootstrap-v1"
	linuxEnvBootstrapMagic               = "pollytool-linux-env-v1"
	linuxEnvPayloadLimit                 = 16 << 20
	linuxReservationIdentityMagic        = "pollytool-linux-reservation-identities-v1"
	linuxReservationIdentityPayloadLimit = 16 << 20
	linuxReservationIdentityFieldCount   = 5
)

type linuxReservationMountIdentity struct {
	path     string
	device   uint64
	inode    uint64
	fileType uint64
	rdev     uint64
}

// init handles the trusted post-containment re-exec used by the Linux backend.
// The executable is reached through an inherited /proc/self/exe FD rather than
// a mutable pathname. No target environment is installed until this code is
// already running behind bwrap's namespaces, mounts, and seccomp filter.
func init() {
	if len(os.Args) < 8 || os.Args[1] != linuxEnvBootstrapArg {
		return
	}
	bootstrapFD, bootstrapErr := strconv.Atoi(os.Args[2])
	envFD, envErr := strconv.Atoi(os.Args[3])
	reservationValidationFD, reservationErr := parseLinuxOptionalFD(os.Args[4])
	authorityFDs, authorityErr := parseLinuxFDList(os.Args[5])
	if bootstrapErr != nil || envErr != nil || bootstrapFD < 3 || envFD < 3 ||
		reservationErr != nil || authorityErr != nil || os.Args[0] != "/proc/self/fd/"+strconv.Itoa(bootstrapFD) {
		return
	}
	if err := runLinuxEnvBootstrap(bootstrapFD, envFD, reservationValidationFD, authorityFDs, os.Args[6], os.Args[7:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "pollytool sandbox bootstrap: %v\n", err)
		os.Exit(126)
	}
	os.Exit(126)
}

func runLinuxEnvBootstrap(bootstrapFD, envFD, reservationValidationFD int, authorityFDs []int, target string, argv []string) error {
	// Reservation bind sources use one pinned descriptor per reservation root,
	// rather than one per sibling. Verify the final mounted objects before
	// releasing those descriptors or reading any target-controlled environment.
	if err := validateLinuxReservationMounts(reservationValidationFD); err != nil {
		return err
	}

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

func formatLinuxOptionalFD(fd int) string {
	if fd < 0 {
		return "-"
	}
	return strconv.Itoa(fd)
}

func parseLinuxOptionalFD(value string) (int, error) {
	if value == "-" {
		return -1, nil
	}
	fd, err := strconv.Atoi(value)
	if err != nil || fd < 3 {
		return -1, fmt.Errorf("invalid sandbox descriptor")
	}
	return fd, nil
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

func linuxReservationMountIdentities(reservations []deniedReservation, laterOvermounts []string) ([]linuxReservationMountIdentity, error) {
	// A nested reservation replaces the outer sibling mount at exactly its root
	// with a private tmpfs. Its own restored children are validated instead; the
	// intentionally overmounted outer destination must not be compared.
	overmounted := make(map[string]bool, len(reservations))
	for _, reservation := range reservations {
		overmounted[filepath.Clean(reservation.root)] = true
	}
	for _, path := range laterOvermounts {
		overmounted[filepath.Clean(path)] = true
	}

	var identities []linuxReservationMountIdentity
	for _, reservation := range reservations {
		for _, entry := range reservation.entries {
			if entry.symlink || entry.info == nil {
				continue
			}
			path := filepath.Join(reservation.root, entry.name)
			if overmounted[path] {
				continue
			}
			stat, ok := entry.info.Sys().(*syscall.Stat_t)
			if !ok || stat == nil {
				return nil, fmt.Errorf("inspect denied reservation identity %q: unsupported file metadata", path)
			}
			identities = append(identities, linuxReservationMountIdentity{
				path:     path,
				device:   uint64(stat.Dev),
				inode:    stat.Ino,
				fileType: uint64(stat.Mode & unix.S_IFMT),
				rdev:     uint64(stat.Rdev),
			})
		}
	}
	return identities, nil
}

func linuxReservationIdentityPayload(identities []linuxReservationMountIdentity) ([]byte, error) {
	var payload strings.Builder
	appendField := func(value string) error {
		if strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("invalid NUL in denied reservation identity payload")
		}
		if len(value)+1 > linuxReservationIdentityPayloadLimit-payload.Len() {
			return fmt.Errorf("denied reservation identity payload exceeds limit")
		}
		payload.WriteString(value)
		payload.WriteByte(0)
		return nil
	}
	if err := appendField(linuxReservationIdentityMagic); err != nil {
		return nil, err
	}
	if err := appendField(strconv.Itoa(len(identities))); err != nil {
		return nil, err
	}
	for _, identity := range identities {
		if identity.path == "" || identity.path[0] != os.PathSeparator || filepath.Clean(identity.path) != identity.path {
			return nil, fmt.Errorf("invalid denied reservation identity path %q", identity.path)
		}
		fields := []string{
			identity.path,
			strconv.FormatUint(identity.device, 10),
			strconv.FormatUint(identity.inode, 10),
			strconv.FormatUint(identity.fileType, 10),
			strconv.FormatUint(identity.rdev, 10),
		}
		for _, field := range fields {
			if err := appendField(field); err != nil {
				return nil, err
			}
		}
	}
	return []byte(payload.String()), nil
}

func parseLinuxReservationIdentityPayload(data []byte) ([]linuxReservationMountIdentity, error) {
	parts := strings.Split(string(data), "\x00")
	if len(parts) < 3 || parts[0] != linuxReservationIdentityMagic || parts[len(parts)-1] != "" {
		return nil, fmt.Errorf("invalid denied reservation identity payload")
	}
	count, err := strconv.Atoi(parts[1])
	if err != nil || count < 0 || count > (len(parts)-3)/linuxReservationIdentityFieldCount ||
		len(parts) != 3+count*linuxReservationIdentityFieldCount {
		return nil, fmt.Errorf("invalid denied reservation identity payload")
	}
	identities := make([]linuxReservationMountIdentity, 0, count)
	seen := make(map[string]bool, count)
	for i := 0; i < count; i++ {
		fields := parts[2+i*linuxReservationIdentityFieldCount : 2+(i+1)*linuxReservationIdentityFieldCount]
		path := fields[0]
		if path == "" || path[0] != os.PathSeparator || filepath.Clean(path) != path || seen[path] {
			return nil, fmt.Errorf("invalid denied reservation identity path %q", path)
		}
		seen[path] = true
		unsigned := make([]uint64, 4)
		for j := range unsigned {
			unsigned[j], err = strconv.ParseUint(fields[j+1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid denied reservation identity for %q", path)
			}
		}
		identities = append(identities, linuxReservationMountIdentity{
			path: path, device: unsigned[0], inode: unsigned[1], fileType: unsigned[2], rdev: unsigned[3],
		})
	}
	return identities, nil
}

func validateLinuxReservationMountIdentities(identities []linuxReservationMountIdentity) error {
	for _, identity := range identities {
		var stat unix.Stat_t
		if err := unix.Lstat(identity.path, &stat); err != nil {
			return fmt.Errorf("inspect denied reservation mount %q: %w", identity.path, err)
		}
		fileType := uint64(stat.Mode & unix.S_IFMT)
		specialDeviceChanged := (fileType == unix.S_IFCHR || fileType == unix.S_IFBLK) && uint64(stat.Rdev) != identity.rdev
		// Device+inode is deliberately the same identity relation used by the
		// previous per-entry os.SameFile check. File type (and device identity for
		// special files) rules out route substitutions without rejecting ordinary
		// writes or metadata changes to an allowed sibling.
		if uint64(stat.Dev) != identity.device || stat.Ino != identity.inode ||
			fileType != identity.fileType || specialDeviceChanged {
			return fmt.Errorf("denied reservation mount %q was replaced or changed (got dev=%d ino=%d type=%#o rdev=%d, want dev=%d ino=%d type=%#o rdev=%d)",
				identity.path, stat.Dev, stat.Ino, fileType, stat.Rdev,
				identity.device, identity.inode, identity.fileType, identity.rdev)
		}
	}
	return nil
}

func validateLinuxReservationMounts(fd int) error {
	if fd < 0 {
		return nil
	}
	file := os.NewFile(uintptr(fd), "pollytool-denied-reservation-identities")
	if file == nil {
		return fmt.Errorf("invalid denied reservation identity descriptor")
	}
	seals, sealErr := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	data, readErr := io.ReadAll(io.LimitReader(file, linuxReservationIdentityPayloadLimit+1))
	closeErr := file.Close()
	if sealErr != nil {
		return fmt.Errorf("inspect denied reservation identity descriptor: %w", sealErr)
	}
	wantSeals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if seals&wantSeals != wantSeals {
		return fmt.Errorf("denied reservation identity descriptor is not sealed")
	}
	if readErr != nil {
		return fmt.Errorf("read denied reservation identities: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close denied reservation identities: %w", closeErr)
	}
	if len(data) > linuxReservationIdentityPayloadLimit {
		return fmt.Errorf("denied reservation identity payload exceeds limit")
	}
	identities, err := parseLinuxReservationIdentityPayload(data)
	if err != nil {
		return err
	}
	return validateLinuxReservationMountIdentities(identities)
}

func attachLinuxReservationValidation(cmd *exec.Cmd, reservations []deniedReservation, laterOvermounts []string) (int, error) {
	identities, err := linuxReservationMountIdentities(reservations, laterOvermounts)
	if err != nil {
		return -1, err
	}
	if len(identities) == 0 {
		return -1, nil
	}
	payload, err := linuxReservationIdentityPayload(identities)
	if err != nil {
		return -1, err
	}
	memfd, err := unix.MemfdCreate("pollytool-denied-reservation-identities", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return -1, err
	}
	file := os.NewFile(uintptr(memfd), "pollytool-denied-reservation-identities")
	if file == nil {
		_ = unix.Close(memfd)
		return -1, fmt.Errorf("create denied reservation identity descriptor")
	}
	if n, err := file.Write(payload); err != nil || n != len(payload) {
		_ = file.Close()
		if err == nil {
			err = io.ErrShortWrite
		}
		return -1, err
	}
	seals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		_ = file.Close()
		return -1, fmt.Errorf("seal denied reservation identity descriptor: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = file.Close()
		return -1, err
	}
	fd := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, file)
	return fd, nil
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
