//go:build darwin

package sandbox

import (
	"cmp"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/alexschlessinger/pollytool/internal/scratch"
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
    open(my $duplicate, "<&=$fd") or die "open duplicate environment descriptor: $!";
    close($duplicate) or die "close duplicate environment descriptor: $!";
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
	// nestedScratch is the scratch root handed to a command that has no
	// scratch of its own; see darwinNestedScratchRoot.
	nestedScratch string
}

// New creates a Sandbox for macOS using sandbox-exec with Seatbelt profiles.
func New(cfg Config) (Sandbox, error) {
	if err := validateDarwinSandboxExecExecutable(darwinSandboxExecPath); err != nil {
		return nil, err
	}
	if err := validateDarwinEnvBootstrapExecutable(darwinEnvBootstrapPath); err != nil {
		return nil, err
	}
	if _, err := privateHomeRoot(); err != nil {
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
	authorityRoutes := readAuthorityPaths(cfg)
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
		writePaths:      writePaths,
		authorityPaths:  authorityPaths,
		nestedScratch:   darwinNestedScratchRoot(cfg),
	}, nil
}

func freezeAuthorityPathsForPlatform(cfg Config) (Config, error) {
	cfg, err := freezeAuthorityPaths(cfg)
	if err != nil {
		return Config{}, err
	}
	return cfg, rejectHomeGrant(cfg, darwinHomeRoots())
}

// darwinPrivateRoots lists Polly storage and, when selected, private home. Only grants re-allow paths inside
// them; the scratch root additionally keeps its own entry readable, so a
// command can walk into the grant beneath it (traversablePrivateRoots).
func darwinPrivateRoots(cfg Config) []string {
	return policyPrivateRoots(cfg)
}

// darwinHomeRoots names the home directory as a private root, alone: it is the
// one root a caller must not grant back whole.
func darwinHomeRoots() []string {
	if home := resolvedHomeDir(); home != "" {
		return []string{home}
	}
	return nil
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
	if len(s.cfg.Env) > 0 {
		// Policy env is the final layer over ambient and per-call values.
		filtered = mergeExplicitEnv(filtered, s.cfg.Env)
	}
	if s.nestedScratch != "" {
		filtered = mergeExplicitEnv(filtered, map[string]string{scratch.RootEnv: s.nestedScratch})
	}

	origArgs := cmd.Args
	origPath, err := resolvedExecutablePath(cmd)
	if err != nil {
		return err
	}
	// A working directory the policy hides (inside a private root with no
	// grant, or masked) starts the command at the filesystem root, as on
	// Linux; a shell started in an unreadable directory would otherwise fail
	// its startup getcwd before running anything.
	workingDir, err := resolvedCommandDir(cmd.Dir)
	if err != nil {
		return err
	}
	if ReadAllowed(s.cfg, workingDir) != nil {
		cmd.Dir = string(filepath.Separator)
	}
	denied := allDeniedPaths(s.cfg)
	slog.Debug("sandbox_wrap",
		"command", commandSummary(origArgs),
		"network", s.cfg.AllowNetwork,
		"deny_write", s.cfg.DenyWrite,
		"writable_paths", s.cfg.WritablePaths,
		"env_stripped", stripped,
		"private_roots", len(darwinPrivateRoots(s.cfg)),
		"read_grants", len(readAuthorityPaths(s.cfg)),
		"denied_paths", len(denied),
		"unix_sockets", len(s.cfg.AllowUnixSockets))
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
		"sandbox-exec", "-p", buildProfileWithWritePaths(s.cfg, s.writePaths, denied, origPath), darwinEnvBootstrapPath,
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
		if _, err := validateEnvEntry(entry); err != nil {
			return nil, err
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

// darwinNestedScratchRoot names the directory inside the scratch root that
// a command without a scratch of its own may write, or "" when it gets none.
// A macOS command keeps the host temp directory, unlike the private /tmp a
// Linux command sees, so a polly or a test suite started inside it computes
// the host's scratch root, which every profile denies. The command is
// granted a directory inside that root instead and told, through
// scratch.RootEnv, to claim scratch there; siblings still see nothing of it,
// and members with a scratch of their own compute a root under it and need
// nothing. The directory is created here, before the profile is built,
// because a grant must exist to be frozen. A root that cannot be trusted
// grants nothing rather than failing a command that did not ask for scratch.
func darwinNestedScratchRoot(cfg Config) string {
	if cfg.DenyWrite || cfg.DenyHostTemp || cfg.Env["TMPDIR"] != "" {
		return ""
	}
	nested, err := scratch.EnsureNestedRoot()
	if err != nil {
		slog.Debug("darwin_nested_scratch_unavailable", "error", err)
		return ""
	}
	return nested
}

func darwinWritePaths(cfg Config) []string {
	paths := []string{}
	if !cfg.DenyHostTemp {
		paths = append(paths, "/private/tmp")
		if tmpdir := os.TempDir(); tmpdir != "" && tmpdir != "/private/tmp" && tmpdir != "/tmp" {
			if real, err := filepath.EvalSymlinks(tmpdir); err == nil {
				tmpdir = real
			}
			paths = append(paths, tmpdir)
		}
	}
	if nested := darwinNestedScratchRoot(cfg); nested != "" {
		paths = append(paths, nested)
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
	// A grant equal to a private root is dropped; the root wins that tie.
	// This covers the automatic temp grant when TMPDIR is the home directory,
	// which rejectHomeGrant does not see.
	return pathsOutsidePrivateRoots(kept, writePrivateRoots(cfg))
}

// pathsOutsidePrivateRoots drops every path whose canonical route is one of
// the private roots.
func pathsOutsidePrivateRoots(paths, privateRoots []string) []string {
	roots := make(map[string]bool, len(privateRoots))
	for _, root := range privateRoots {
		roots[canonicalPolicyPath(root)] = true
	}
	kept := make([]string, 0, len(paths))
	for _, path := range paths {
		if !roots[canonicalPolicyPath(expandTilde(path))] {
			kept = append(kept, path)
		}
	}
	return kept
}

func buildProfile(cfg Config) string {
	return buildProfileWithWritePaths(cfg, darwinWritePaths(cfg), allDeniedPaths(cfg), "")
}

// darwinPathRule is one path-scoped Seatbelt rule. Rules are emitted in
// increasing path depth, so with Seatbelt's last-match-wins evaluation the
// deepest rule containing a path decides; rank orders rules at one depth.
type darwinPathRule struct {
	path    string
	rank    int
	literal bool
}

func sortDarwinPathRules(rules []darwinPathRule) {
	slices.SortStableFunc(rules, func(a, b darwinPathRule) int {
		if c := comparePathDepth(a.path, b.path); c != 0 {
			return c
		}
		return cmp.Compare(a.rank, b.rank)
	})
}

// darwinDeny renders one deny rule for operation, scoped by filter unless it
// is empty. A config with a denial tag has the rule report under it (see
// DenialObserver); without one the rule is the plain form.
func darwinDeny(cfg Config, operation, filter string) string {
	rule := "(deny " + operation
	if cfg.denialTag != "" {
		rule += fmt.Sprintf(" (with message %q)", cfg.denialTag)
	}
	if filter != "" {
		rule += " " + filter
	}
	return rule + ")\n"
}

// Rule ranks break ties between rules at one path. Read and write rules are
// sorted as separate lists, and one block keeps every rank distinct so a rank
// can never be mistaken for one of the other list.
const (
	darwinReadDeny = iota
	darwinReadAllow
	darwinWriteAllow
	darwinWriteMaskDeny
	darwinWriteLeafDeny
)

// buildProfileWithWritePaths renders the Seatbelt profile. Reads: the home
// directory and every denied path are denied, then read grants, visible
// paths, writable paths, granted symlink spellings and the executable itself
// are re-allowed, all ordered by depth so a grant inside a denied directory
// and a denied path inside a grant both win where they are deepest. Writes:
// the default deny, the writable grants, the private-root and denied-path
// write masks and the deny-write islands, likewise by depth; at one path a
// mask beats a grant (a grant wins that tie for reads only) and a deny-write
// island beats the grant. A grant equal to a private root is dropped, so a
// temp directory that is the home cannot open it. Unlink pins follow so no
// routing entry under a writable grant can be renamed away from its rule.
// A denial tag marks every deny rule but the signal rule, which guards other
// processes rather than anything a command could be granted.
func buildProfileWithWritePaths(cfg Config, writePaths []string, deniedPaths []DeniedPath, executable string) string {
	var sb strings.Builder
	sb.WriteString("(version 1)\n")
	sb.WriteString("(allow default)\n")
	sb.WriteString(darwinDeny(cfg, "file-write*", ""))

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

	privateRoots := darwinPrivateRoots(cfg)
	var deniedRoutes []string
	for _, denied := range deniedPaths {
		deniedRoutes = append(deniedRoutes, pathAndResolved(denied.Path)...)
	}

	var writeRules []darwinPathRule
	if !cfg.DenyWrite {
		for _, p := range writePaths {
			writeRules = append(writeRules, darwinPathRule{path: filepath.Clean(expandTilde(p)), rank: darwinWriteAllow})
		}
	}
	// A private root and a denied path stay unwritable under a broader
	// writable grant and where a grant equals them (writes never win that
	// tie), and a deny-write island stays read-only even where it equals a
	// writable grant. Reads of islands stay allowed; deniedPaths below denies
	// both.
	for _, root := range writePrivateRoots(cfg) {
		for _, p := range pathAndResolved(root) {
			writeRules = append(writeRules, darwinPathRule{path: p, rank: darwinWriteMaskDeny})
		}
	}
	for _, p := range deniedRoutes {
		writeRules = append(writeRules, darwinPathRule{path: p, rank: darwinWriteMaskDeny})
	}
	for _, p := range cfg.DenyWritePaths {
		for _, rp := range pathAndResolved(expandTilde(p)) {
			writeRules = append(writeRules, darwinPathRule{path: rp, rank: darwinWriteLeafDeny})
		}
	}
	sortDarwinPathRules(writeRules)
	for _, rule := range writeRules {
		switch rule.rank {
		case darwinWriteAllow:
			sb.WriteString(fmt.Sprintf("(allow file-write* (subpath %q))\n", rule.path))
		default:
			sb.WriteString(darwinDeny(cfg, "file-write*", fmt.Sprintf("(literal %q)", rule.path)))
			sb.WriteString(darwinDeny(cfg, "file-write*", fmt.Sprintf("(subpath %q)", rule.path)))
		}
	}

	// Freeze writable/read grant routing entries and their mutable ancestors.
	// This denies only unlink/rename of the directory entries; writes beneath a
	// writable directory remain permitted.
	authorityPaths := readAuthorityPaths(cfg)
	for _, link := range cfg.grantSymlinks {
		authorityPaths = append(authorityPaths, link.path)
	}
	if !cfg.DenyWrite {
		authorityPaths = append(authorityPaths, writePaths...)
	}
	for _, path := range authorityWritePins(authorityPaths, writePaths) {
		sb.WriteString(darwinDeny(cfg, "file-write-unlink", fmt.Sprintf("(literal %q)", path)))
	}
	// Pin every mutable ancestor of a deny-write island so a process cannot
	// move an ancestor and rebuild a replacement at the guarded pathname.
	for _, ancestor := range denyWriteAncestors(cfg.DenyWritePaths, writePaths) {
		sb.WriteString(darwinDeny(cfg, "file-write-unlink", fmt.Sprintf("(literal %q)", ancestor)))
	}
	// A denied entry must not be movable to a new readable name under a broad
	// writable grant: pin the entry and its routing ancestors.
	for _, path := range authorityWritePins(deniedRoutes, writePaths) {
		sb.WriteString(darwinDeny(cfg, "file-write-unlink", fmt.Sprintf("(literal %q)", path)))
	}

	// Reads. The home directory is denied whole; denied paths are masked
	// everywhere; grants re-allow their subtrees. Resolving an allowed
	// descendant needs stat on its denied ancestors (Git resolving a linked
	// worktree's common gitdir, a shell entering a project under the home
	// directory), so each grant also allows metadata on exactly its ancestor
	// entries: never their listings, contents, or writes.
	var readRules []darwinPathRule
	for _, root := range privateRoots {
		for _, p := range pathAndResolved(root) {
			readRules = append(readRules, darwinPathRule{path: p, rank: darwinReadDeny})
		}
	}
	for _, p := range deniedRoutes {
		readRules = append(readRules, darwinPathRule{path: p, rank: darwinReadDeny})
	}
	for _, p := range pathsOutsidePrivateRoots(readAuthorityPaths(cfg), privateRoots) {
		readRules = append(readRules, darwinPathRule{path: filepath.Clean(expandTilde(p)), rank: darwinReadAllow})
	}
	for _, p := range writePaths {
		readRules = append(readRules, darwinPathRule{path: filepath.Clean(expandTilde(p)), rank: darwinReadAllow})
	}
	for _, link := range cfg.grantSymlinks {
		readRules = append(readRules, darwinPathRule{path: link.path, rank: darwinReadAllow, literal: true})
	}
	if executable != "" && isWithinAny(filepath.Clean(executable), privateRoots) {
		readRules = append(readRules, darwinPathRule{path: filepath.Clean(executable), rank: darwinReadAllow, literal: true})
	}
	// A traversable root's own entry is re-allowed over its subpath deny, so
	// opening each component of a path into a grant beneath it succeeds. The
	// allow is literal: it re-exposes the directory listing and nothing under
	// it, and sorts after the deny at equal depth.
	for _, root := range traversablePrivateRoots() {
		if DeniedBy(cfg.DenyPaths, root) {
			continue
		}
		for _, p := range pathAndResolved(root) {
			readRules = append(readRules, darwinPathRule{path: p, rank: darwinReadAllow, literal: true})
		}
	}
	sortDarwinPathRules(readRules)
	readAncestors := make(map[string]bool)
	for _, rule := range readRules {
		if rule.rank == darwinReadDeny {
			sb.WriteString(darwinDeny(cfg, "file-read*", fmt.Sprintf("(literal %q)", rule.path)))
			sb.WriteString(darwinDeny(cfg, "file-read*", fmt.Sprintf("(subpath %q)", rule.path)))
			continue
		}
		if rule.literal {
			sb.WriteString(fmt.Sprintf("(allow file-read* (literal %q))\n", rule.path))
		} else {
			sb.WriteString(fmt.Sprintf("(allow file-read* (subpath %q))\n", rule.path))
		}
		for ancestor := filepath.Dir(rule.path); !readAncestors[ancestor]; ancestor = filepath.Dir(ancestor) {
			readAncestors[ancestor] = true
			sb.WriteString(fmt.Sprintf("(allow file-read-metadata (literal %q))\n", ancestor))
		}
	}
	// A trial's command may stat the entries of statPaths the same way,
	// except a denied path's.
	for _, path := range cfg.statPaths {
		for _, p := range pathAndResolved(filepath.Clean(path)) {
			if !readAncestors[p] && !isWithinAny(p, deniedRoutes) {
				readAncestors[p] = true
				sb.WriteString(fmt.Sprintf("(allow file-read-metadata (literal %q))\n", p))
			}
		}
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
		sb.WriteString(darwinDeny(cfg, "network*", ""))
	} else {
		// Enabling TCP/UDP must not also expose host Docker, VM, agent, or
		// service Unix sockets. macOS DNS normally uses the fixed
		// mDNSResponder socket, so re-allow only that system endpoint unless
		// DNS itself was denied.
		sb.WriteString(darwinDeny(cfg, "network-outbound", "(remote unix-socket)"))
		if !cfg.DenyDNS {
			sb.WriteString("(allow network-outbound (remote unix-socket (path-literal \"/private/var/run/mDNSResponder\")))\n")
		} else {
			// Block direct DNS queries (port 53) as a fallback.
			sb.WriteString(darwinDeny(cfg, "network-outbound", `(remote udp "*:53")`))
			sb.WriteString(darwinDeny(cfg, "network-outbound", `(remote tcp "*:53")`))
		}
	}

	// Granted Unix sockets are re-allowed after both network deny variants
	// (Seatbelt is last-match-wins), so an agent socket works with networking
	// off as well. Both the frozen spelling and its ancestor-resolved form are
	// emitted: Seatbelt matches resolved vnode paths, and macOS launchd agent
	// sockets are usually reached through the /tmp -> /private/tmp alias.
	for _, grant := range effectiveUnixSocketGrants(cfg) {
		for _, path := range pathAndResolved(grant.path) {
			sb.WriteString(fmt.Sprintf("(allow network-outbound (remote unix-socket (path-literal %q)))\n", path))
		}
	}

	return sb.String()
}
