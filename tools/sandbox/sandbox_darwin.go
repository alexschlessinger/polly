//go:build darwin

package sandbox

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	darwinSandboxExecPath = "/usr/bin/sandbox-exec"
	// The bootstrap interpreter must be a fixed, root-owned, SIP-sealed binary
	// that ships with macOS (validateDarwinTrustedExecutable), and must read
	// the NUL-framed payload from inherited pipes, stat and close descriptors,
	// rebuild the environment, and exec with an explicit argv[0] — natively,
	// in one process, with no string interpolation. /usr/bin/perl is the only
	// stock interpreter that satisfies all of that: shell variables cannot
	// hold NUL bytes and POSIX sh has no argv[0] control; python3 is an
	// on-demand CLT stub; ruby is nearer removal than perl; env(1) would
	// expose secrets in visible argv; and a Go helper cannot be pinned the way
	// Linux pins /proc/self/exe — it would have to be exec'd from
	// user-writable disk, which the trust check exists to forbid.
	darwinEnvBootstrapPath         = "/usr/bin/perl"
	darwinEnvBootstrapMagic        = "pollytool-darwin-env-v2"
	darwinEnvBootstrapMaxPayload   = 1 << 20
	darwinEnvBootstrapMaxPipeCount = 512
	darwinEnvBootstrapCode         = `use strict;
use warnings;
use POSIX ();
my $pipe_count = shift @ARGV;
my $first_fd = shift @ARGV;
defined($pipe_count) && defined($first_fd) or die "missing environment descriptors";
$pipe_count =~ /\A[1-9][0-9]*\z/ && $pipe_count <= 512 or die "invalid environment descriptor count";
$first_fd =~ /\A(?:0|[1-9][0-9]*)\z/ && $first_fd >= 3 or die "invalid environment descriptor";
my $framed = "";
my %transport_identities = ();
for (my $offset = 0; $offset < $pipe_count; $offset++) {
    my $fd = $first_fd + $offset;
    open(my $fh, "<&=$fd") or die "open environment descriptor: $!";
    my @identity = stat($fh);
    @identity or die "stat environment descriptor: $!";
    $transport_identities{join(":", @identity[0, 1, 2, 6])} = 1;
    binmode($fh);
    local $/;
    my $chunk = <$fh>;
    $chunk = "" unless defined($chunk);
    close($fh) or die "close environment descriptor: $!";
    $framed .= $chunk;
}
opendir(my $fd_dir, "/dev/fd") or die "open descriptor directory: $!";
my @open_fds = grep { /\A(?:0|[1-9][0-9]*)\z/ } readdir($fd_dir);
closedir($fd_dir) or die "close descriptor directory: $!";
my @transport_duplicates = ();
for my $fd (@open_fds) {
    open(my $candidate, "<&", $fd) or next;
    my @identity = stat($candidate);
    close($candidate) or die "close descriptor probe: $!";
    next unless @identity;
    push @transport_duplicates, 0 + $fd
        if $transport_identities{join(":", @identity[0, 1, 2, 6])};
}
for my $fd (@transport_duplicates) {
    defined(POSIX::close($fd)) or die "close duplicate environment descriptor: $!";
}
my ($magic, $length_field, $data) = split(/\0/, $framed, 3);
defined($magic) && defined($length_field) && defined($data) or die "invalid environment payload";
$magic eq "pollytool-darwin-env-v2" or die "invalid environment payload";
$length_field =~ /\A(?:0|[1-9][0-9]*)\z/ or die "invalid environment payload";
my $expected = 0 + $length_field;
length($data) == $expected or die "truncated environment payload";
my @parts = ();
if (length($data)) {
    @parts = split(/\0/, $data, -1);
    my $tail = pop @parts;
    defined($tail) && $tail eq "" or die "invalid environment payload";
}
%ENV = ();
for my $entry (@parts) {
    my ($name, $value) = split(/=/, $entry, 2);
    die "invalid environment entry" unless defined($value) && length($name);
    $ENV{$name} = $value;
}
die "missing target" unless @ARGV;
my $target = shift @ARGV;
die "missing target argument vector" unless @ARGV;
exec {$target} @ARGV;
die "exec target: $!";
`
)

type darwinSandbox struct {
	cfg             Config
	sandboxExecPath string
	writePaths      []string
	authorityPaths  []authorityPathIdentity
}

// New creates a Sandbox for macOS using sandbox-exec with Seatbelt profiles.
func New(cfg Config) (Sandbox, error) {
	if err := validateDarwinSandboxExecExecutable(darwinSandboxExecPath); err != nil {
		return nil, err
	}
	if err := validateDarwinEnvBootstrapExecutable(darwinEnvBootstrapPath); err != nil {
		return nil, err
	}
	var err error
	cfg, err = PrepareConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if !cfg.DenyWrite {
		if _, err := resolveDenyWritePaths(cfg.DenyWritePaths, cfg.WritablePaths); err != nil {
			return nil, err
		}
	}
	writePaths := darwinWritePaths(cfg)
	authorityRoutes := append([]string{}, cfg.ReadPaths...)
	if !cfg.DenyWrite {
		authorityRoutes = append(authorityRoutes, writePaths...)
	}
	authorityPaths, err := captureAuthorityPathIdentities(authorityRoutes)
	if err != nil {
		return nil, err
	}
	return &darwinSandbox{
		cfg:             cfg,
		sandboxExecPath: darwinSandboxExecPath,
		writePaths:      append([]string(nil), writePaths...),
		authorityPaths:  authorityPaths,
	}, nil
}

func freezeAuthorityPathsForPlatform(cfg Config) (Config, error) {
	return freezeAuthorityPaths(cfg)
}

func (s *darwinSandbox) Wrap(cmd *exec.Cmd) error {
	return ErrManagedWrapRequired
}

func (s *darwinSandbox) WrapWithEnv(cmd *exec.Cmd, explicitEnv map[string]string) error {
	if err := validateExplicitEnv(explicitEnv); err != nil {
		return err
	}
	return ErrManagedWrapRequired
}

func (s *darwinSandbox) wrapManaged(cmd *exec.Cmd, explicitEnv map[string]string) error {
	if err := validateExplicitEnv(explicitEnv); err != nil {
		return err
	}
	if err := validateAuthorityPathIdentities(s.authorityPaths); err != nil {
		return err
	}
	if err := validateReadPathAliasIdentities(s.cfg.readPathAliases); err != nil {
		return err
	}
	if !s.cfg.DenyWrite {
		if _, err := resolveDenyWritePaths(s.cfg.DenyWritePaths, s.cfg.WritablePaths); err != nil {
			return err
		}
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	filtered, stripped := filterEnv(env, s.cfg.AllowEnv, s.cfg.PassEnv)
	filtered = mergeExplicitEnv(filtered, explicitEnv)

	origArgs := cmd.Args
	origPath, err := resolvedExecutablePath(cmd)
	if err != nil {
		return err
	}
	denied := allDeniedPaths(s.cfg)
	slog.Debug("sandbox_wrap",
		"command", commandSummary(origArgs),
		"network", s.cfg.AllowNetwork,
		"deny_write", s.cfg.DenyWrite,
		"writable_paths", s.cfg.WritablePaths,
		"env_stripped", stripped,
		"denied_paths", len(denied))
	bootstrapFD, bootstrapPipeCount, err := attachDarwinEnvBootstrap(cmd, filtered)
	if err != nil {
		return fmt.Errorf("prepare target environment: %w", err)
	}
	cmd.Path = s.sandboxExecPath
	// The deny profile is rebuilt on every wrap so newly created credential
	// paths are covered. Approved write roots remain the construction-time set.
	targetArgs := origArgs
	if len(targetArgs) == 0 {
		targetArgs = []string{origPath}
	}
	cmd.Args = []string{
		"sandbox-exec", "-p", buildProfileWithWritePaths(s.cfg, s.writePaths, denied), darwinEnvBootstrapPath,
		"-e", darwinEnvBootstrapCode, strconv.Itoa(bootstrapPipeCount), strconv.Itoa(bootstrapFD), origPath,
	}
	cmd.Args = append(cmd.Args, targetArgs...)
	// sandbox-exec and the fixed, root-owned bootstrap receive no target
	// environment. Perl concatenates the anonymous, prefilled pipe shards and
	// validates their length-framed payload only after Seatbelt is active, then
	// closes every reader and any inherited duplicate that still identifies the
	// same pipe, clears its own environment, and uses its builtin exec to launch
	// the target with the exact filtered environment, resolved executable,
	// original argv, inherited stdin, and unrelated caller-owned ExtraFiles. No
	// intermediate exec argv contains a target value.
	cmd.Env = []string{}

	// Run in a new session, detaching the controlling terminal. This is the
	// macOS counterpart to bwrap's --new-session on Linux: it closes terminal
	// injection vectors and, by giving the process its own process group, makes
	// the (allow signal (target pgrp)) rule in the profile mean "own children
	// only" — so a sandboxed tool can signal its own jobs but not the user's
	// other processes. The two must change together.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	return nil
}

func validateDarwinEnvBootstrapExecutable(path string) error {
	return validateDarwinTrustedExecutable(path, "environment bootstrap")
}

func validateDarwinSandboxExecExecutable(path string) error {
	return validateDarwinTrustedExecutable(path, "backend")
}

func validateDarwinTrustedExecutable(path, description string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("Darwin sandbox %s unavailable: %w", description, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("Darwin sandbox %s %q is not a regular executable", description, path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("Darwin sandbox %s %q is not root-owned and immutable to non-root users", description, path)
	}
	return nil
}

// attachDarwinEnvBootstrap transports the target environment through
// anonymous, prefilled pipe shards. Each writer is nonblocking and closed
// before Wrap returns; when one pipe fills, the remainder goes into another.
// This handles payloads larger than a pipe buffer without a named object,
// target-visible writer, or goroutine that could outlive an abandoned raw Wrap.
// On any setup error cmd.ExtraFiles is left unchanged.
func attachDarwinEnvBootstrap(cmd *exec.Cmd, env []string) (int, int, error) {
	payload, err := darwinEnvBootstrapPayload(env)
	if err != nil {
		return 0, 0, err
	}
	if len(payload) > darwinEnvBootstrapMaxPayload {
		return 0, 0, fmt.Errorf("target environment payload is %d bytes; maximum is %d", len(payload), darwinEnvBootstrapMaxPayload)
	}
	readers, err := prefillDarwinEnvPipes(payload)
	if err != nil {
		return 0, 0, err
	}
	firstFD := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, readers...)
	return firstFD, len(readers), nil
}

func prefillDarwinEnvPipes(payload []byte) ([]*os.File, error) {
	readers := make([]*os.File, 0, 1)
	closeReaders := func() {
		for _, reader := range readers {
			_ = reader.Close()
		}
	}
	for len(payload) > 0 {
		if len(readers) >= darwinEnvBootstrapMaxPipeCount {
			closeReaders()
			return nil, fmt.Errorf("target environment requires more than %d anonymous pipe shards", darwinEnvBootstrapMaxPipeCount)
		}
		reader, writer, err := os.Pipe()
		if err != nil {
			closeReaders()
			return nil, fmt.Errorf("create anonymous environment pipe: %w", err)
		}
		// Cache Fd once: os.File.Fd may restore blocking mode for descriptors
		// managed by the runtime poller, which would defeat the shard boundary.
		writerFD := int(writer.Fd())
		if err := unix.SetNonblock(writerFD, true); err != nil {
			_ = reader.Close()
			_ = writer.Close()
			closeReaders()
			return nil, fmt.Errorf("make anonymous environment pipe nonblocking: %w", err)
		}
		writtenTotal := 0
		for len(payload) > 0 {
			written, writeErr := unix.Write(writerFD, payload)
			if written > 0 {
				payload = payload[written:]
				writtenTotal += written
			}
			if writeErr == nil {
				if written == 0 {
					_ = reader.Close()
					_ = writer.Close()
					closeReaders()
					return nil, fmt.Errorf("prefill anonymous environment pipe: zero-byte write")
				}
				continue
			}
			if writeErr == unix.EINTR {
				continue
			}
			if writeErr == unix.EAGAIN || writeErr == unix.EWOULDBLOCK {
				break
			}
			_ = reader.Close()
			_ = writer.Close()
			closeReaders()
			return nil, fmt.Errorf("prefill anonymous environment pipe: %w", writeErr)
		}
		if closeErr := writer.Close(); closeErr != nil {
			_ = reader.Close()
			closeReaders()
			return nil, fmt.Errorf("close anonymous environment pipe writer: %w", closeErr)
		}
		if writtenTotal == 0 {
			_ = reader.Close()
			closeReaders()
			return nil, fmt.Errorf("anonymous environment pipe accepted no payload")
		}
		readers = append(readers, reader)
	}
	return readers, nil
}

func darwinEnvBootstrapPayload(env []string) ([]byte, error) {
	var body strings.Builder
	for _, entry := range env {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" || strings.ContainsRune(name, '=') || strings.IndexByte(name, 0) >= 0 || strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("invalid target environment entry for %q", name)
		}
		if len(entry)+1 > darwinEnvBootstrapMaxPayload-body.Len() {
			return nil, fmt.Errorf("target environment payload exceeds %d bytes", darwinEnvBootstrapMaxPayload)
		}
		bodyLen := body.Len() + len(entry) + 1
		framedLen := len(darwinEnvBootstrapMagic) + 1 + len(strconv.Itoa(bodyLen)) + 1 + bodyLen
		if framedLen > darwinEnvBootstrapMaxPayload {
			return nil, fmt.Errorf("target environment payload exceeds %d bytes", darwinEnvBootstrapMaxPayload)
		}
		body.WriteString(entry)
		body.WriteByte(0)
	}
	var payload strings.Builder
	payload.WriteString(darwinEnvBootstrapMagic)
	payload.WriteByte(0)
	payload.WriteString(strconv.Itoa(body.Len()))
	payload.WriteByte(0)
	payload.WriteString(body.String())
	return []byte(payload.String()), nil
}

func parseDarwinEnvBootstrapPayload(data []byte) ([]string, error) {
	magic, rest, found := strings.Cut(string(data), "\x00")
	if !found || magic != darwinEnvBootstrapMagic {
		return nil, fmt.Errorf("invalid bootstrap environment payload")
	}
	lengthField, body, found := strings.Cut(rest, "\x00")
	expected, err := strconv.Atoi(lengthField)
	if !found || err != nil || expected < 0 || strconv.Itoa(expected) != lengthField || len(body) != expected {
		return nil, fmt.Errorf("invalid bootstrap environment payload")
	}
	if body == "" {
		return nil, nil
	}
	parts := strings.Split(body, "\x00")
	if parts[len(parts)-1] != "" {
		return nil, fmt.Errorf("invalid bootstrap environment payload")
	}
	env := append([]string(nil), parts[:len(parts)-1]...)
	for _, entry := range env {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" || strings.ContainsRune(name, '=') || strings.IndexByte(name, 0) >= 0 || strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("invalid bootstrap environment entry for %q", name)
		}
	}
	return env, nil
}

// pathAndResolved returns path plus its symlink-resolved target when that
// differs. Seatbelt matches resolved vnode paths, so a rule on a
// dotfiles-managed symlink (~/.npmrc -> ~/dotfiles/npmrc) never fires for the
// real file; rules must name both.
func pathAndResolved(path string) []string {
	if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
		return []string{path, real}
	}
	return []string{path}
}

// denyWriteAncestors returns every writable ancestor whose directory entry
// must remain pinned for a DenyWritePaths rule to keep naming the same object.
// Denying writes only at /work/nested/.git is insufficient if a process can
// rename /work/nested and create a replacement tree at the original pathname.
// Seatbelt can deny unlink/rename of these ancestor entries without making
// their contents read-only, so ordinary workspace edits remain available.
func denyWriteAncestors(protectedPaths, writablePaths []string) []string {
	var ancestors []string
	seen := make(map[string]bool)
	for _, protected := range protectedPaths {
		for _, protectedPath := range pathAndResolved(expandTilde(protected)) {
			for _, writable := range writablePaths {
				writable = expandTilde(writable)
				writable = filepath.Clean(writable)
				for ancestor := filepath.Dir(protectedPath); pathWithinPolicy(ancestor, writable); ancestor = filepath.Dir(ancestor) {
					if !seen[ancestor] {
						seen[ancestor] = true
						ancestors = append(ancestors, ancestor)
					}
					if ancestor == writable {
						break
					}
				}
			}
		}
	}
	return ancestors
}

func authorityWritePins(authorityPaths, writablePaths []string) []string {
	var pins []string
	seen := make(map[string]bool)
	add := func(path string) {
		path = filepath.Clean(expandTilde(path))
		if !seen[path] {
			seen[path] = true
			pins = append(pins, path)
		}
	}
	for _, authority := range authorityPaths {
		authority = filepath.Clean(expandTilde(authority))
		for _, writable := range writablePaths {
			writable = filepath.Clean(expandTilde(writable))
			if !pathWithinPolicy(authority, writable) {
				continue
			}
			add(authority)
			for ancestor := filepath.Dir(authority); pathWithinPolicy(ancestor, writable); ancestor = filepath.Dir(ancestor) {
				add(ancestor)
				if ancestor == writable {
					break
				}
			}
		}
	}
	return pins
}

func darwinWritePaths(cfg Config) []string {
	paths := []string{"/private/tmp"}
	if tmpdir := os.TempDir(); tmpdir != "" && tmpdir != "/private/tmp" && tmpdir != "/tmp" {
		if real, err := filepath.EvalSymlinks(tmpdir); err == nil {
			tmpdir = real
		}
		paths = append(paths, tmpdir)
	}
	for _, path := range cfg.WritablePaths {
		paths = append(paths, filepath.Clean(expandTilde(path)))
	}
	seen := make(map[string]bool, len(paths))
	kept := paths[:0]
	for _, path := range paths {
		path = filepath.Clean(path)
		if !seen[path] {
			seen[path] = true
			kept = append(kept, path)
		}
	}
	return kept
}

func buildProfile(cfg Config, deniedLists ...[]DeniedPath) string {
	return buildProfileWithWritePaths(cfg, darwinWritePaths(cfg), deniedLists...)
}

func buildProfileWithWritePaths(cfg Config, writePaths []string, deniedLists ...[]DeniedPath) string {
	deniedPaths := allDeniedPaths(cfg)
	if len(deniedLists) > 0 {
		deniedPaths = deniedLists[0]
	}
	var sb strings.Builder
	sb.WriteString("(version 1)\n")
	sb.WriteString("(allow default)\n")
	sb.WriteString("(deny file-write*)\n")

	// Always allow writes to the standard character devices. /dev/null in
	// particular is a universal shell idiom (`>/dev/null 2>&1`) and blocking
	// it breaks otherwise-innocuous tools. The kernel's device drivers handle
	// the discard/zero/random semantics; there's no data at rest, so allowing
	// these writes does not weaken the sandbox. Apply even under DenyWrite —
	// matches bwrap's behavior on Linux (--dev /dev gives a fresh devtmpfs).
	for _, dev := range []string{
		"/dev/null",
		"/dev/zero",
		"/dev/random",
		"/dev/urandom",
		"/dev/stdout",
		"/dev/stderr",
	} {
		sb.WriteString(fmt.Sprintf("(allow file-write* (literal %q))\n", dev))
	}

	if !cfg.DenyWrite {
		// Allow writes to OS temp dir and configured paths.

		for _, p := range writePaths {
			sb.WriteString(fmt.Sprintf("(allow file-write* (subpath %q))\n", filepath.Clean(expandTilde(p))))
		}
	}

	// Freeze writable/read grant routing entries and their mutable ancestors.
	// This denies only unlink/rename of the directory entries; writes beneath a
	// writable directory remain permitted.
	authorityPaths := append([]string{}, cfg.ReadPaths...)
	for _, alias := range cfg.readPathAliases {
		for _, symlink := range alias.symlinks {
			authorityPaths = append(authorityPaths, symlink.path)
		}
	}
	if !cfg.DenyWrite {
		authorityPaths = append(authorityPaths, writePaths...)
	}
	for _, path := range authorityWritePins(authorityPaths, writePaths) {
		sb.WriteString(fmt.Sprintf("(deny file-write-unlink (literal %q))\n", path))
	}

	// Pin every mutable ancestor before applying the write-denied islands. This
	// prevents moving an ancestor and rebuilding a replacement at the guarded
	// pathname while still allowing data writes beneath those ancestors.
	for _, ancestor := range denyWriteAncestors(cfg.DenyWritePaths, writePaths) {
		sb.WriteString(fmt.Sprintf("(deny file-write-unlink (literal %q))\n", ancestor))
	}

	// A denied credential entry must not be movable to a new readable name
	// under a broad writable grant. Pin the entry itself and every mutable
	// routing ancestor, including the literal side of a symlinked deny path.
	var deniedRoutes []string
	for _, denied := range deniedPaths {
		deniedRoutes = append(deniedRoutes, pathAndResolved(denied.Path)...)
	}
	for _, path := range authorityWritePins(deniedRoutes, writePaths) {
		sb.WriteString(fmt.Sprintf("(deny file-write-unlink (literal %q))\n", path))
	}

	// Write-denied islands inside the writable subpaths. Emitted after the
	// allows so last-match-wins blocks the write; reads stay allowed (unlike
	// deniedPaths below, which blocks both). Literal rules protect the entry
	// itself, while subpath rules protect its contents. New and Wrap reject a
	// missing protected entry because Seatbelt cannot reserve a nonexistent
	// vnode reliably.
	for _, p := range cfg.DenyWritePaths {
		for _, rp := range pathAndResolved(expandTilde(p)) {
			sb.WriteString(fmt.Sprintf("(deny file-write* (literal %q))\n", rp))
			sb.WriteString(fmt.Sprintf("(deny file-write* (subpath %q))\n", rp))
		}
	}

	// Deny access to sensitive credential paths — both reads AND writes.
	// The write deny matters because a writablePaths entry that is an ancestor
	// of a denied path (e.g. writablePaths ["~"] over ~/.ssh) would otherwise
	// re-open write access through the (allow file-write* (subpath ...)) rules
	// emitted above: a sandboxed process couldn't read ~/.ssh but could plant
	// ~/.ssh/authorized_keys or overwrite ~/.aws/credentials. These rules come
	// after the writable allows so Seatbelt's last-match-wins blocks the write.
	// (On Linux the same case is covered structurally — the tmpfs/dev-null mask
	// sits over the writable bind, so writes hit the ephemeral overlay.)
	// readPaths re-allows reads below, but deliberately never writes.
	for _, denied := range deniedPaths {
		for _, p := range pathAndResolved(denied.Path) {
			sb.WriteString(fmt.Sprintf("(deny file-read* (literal %q))\n", p))
			sb.WriteString(fmt.Sprintf("(deny file-read* (subpath %q))\n", p))
			sb.WriteString(fmt.Sprintf("(deny file-write* (literal %q))\n", p))
			sb.WriteString(fmt.Sprintf("(deny file-write* (subpath %q))\n", p))
		}
	}

	// Re-allow read access for exempted paths (last-match-wins in Seatbelt).
	for _, p := range cfg.ReadPaths {
		sb.WriteString(fmt.Sprintf("(allow file-read* (subpath %q))\n", filepath.Clean(expandTilde(p))))
	}
	for _, p := range readPathAliasPaths(cfg) {
		sb.WriteString(fmt.Sprintf("(allow file-read* (subpath %q))\n", filepath.Clean(p)))
	}

	// Deny signaling unrelated processes while still allowing a script to manage
	// its own descendants. (target same-sandbox) matches exactly the processes
	// running inside this sandbox instance — the script's own children,
	// including ones it detached into their own session/process group (job
	// control, `setsid` workers) — while the user's other processes, which are
	// not in the sandbox, stay unreachable. This is stricter and more robust
	// than a process-group scope: it doesn't depend on the Setsid in Wrap and
	// doesn't break a tool that re-groups its children. macOS approximation of
	// the isolation Linux gets for free from the PID namespace (where other
	// processes are simply invisible).
	sb.WriteString("(deny signal)\n")
	sb.WriteString("(allow signal (target self))\n")
	sb.WriteString("(allow signal (target same-sandbox))\n")

	if !cfg.AllowNetwork {
		sb.WriteString("(deny network*)\n")
	} else {
		// Enabling TCP/UDP must not also expose host Docker, VM, agent, or
		// service Unix sockets. macOS DNS normally uses the fixed
		// mDNSResponder socket, so re-allow only that system endpoint unless
		// DNS itself was denied.
		sb.WriteString("(deny network-outbound (remote unix-socket))\n")
		if !cfg.DenyDNS {
			sb.WriteString("(allow network-outbound (remote unix-socket (path-literal \"/private/var/run/mDNSResponder\")))\n")
		} else {
			// Block direct DNS queries (port 53) as a fallback.
			sb.WriteString("(deny network-outbound (remote udp \"*:53\"))\n")
			sb.WriteString("(deny network-outbound (remote tcp \"*:53\"))\n")
		}
	}

	return sb.String()
}
