package tools

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// SandboxContext describes current registry authority for model requests. It
// contains no environment values and is not an enforcement mechanism. Call it
// for each request: layers can change during a turn, while bound execution
// contexts retain their own policy. A registry with no sandbox configuration
// contributes no context; explicit unsafe mode is reported separately.
func (r *ToolRegistry) SandboxContext() (string, error) {
	if r == nil {
		return "", nil
	}
	cfg, active, err := r.SandboxReadPolicy()
	if err != nil {
		return "", fmt.Errorf("resolve sandbox context: %w", err)
	}
	if !active && !r.unsafeNoSandbox {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("<sandbox_context>\nCurrent runtime permissions; supersedes earlier sandbox summaries.\n")
	root, err := r.workRoot()
	if err != nil {
		return "", fmt.Errorf("sandbox working directory: %w", err)
	}
	fmt.Fprintf(&b, "Working directory: %s\n", strconv.Quote(root))
	if !r.HasSandbox() {
		if r.unsafeNoSandbox {
			b.WriteString("Process sandbox: disabled; commands have ambient host access.\n")
		} else {
			b.WriteString("Process sandbox: unavailable; process tools require a sandbox.\n")
		}
	}
	if active {
		if r.HasSandbox() {
			b.WriteString("Filesystem policy below covers native file tools. Bash uses this policy in an OS sandbox; shell tools may have their own overlays. Network policy is for these processes. MCP servers and other tools may have different permissions.\n")
		} else {
			b.WriteString("Filesystem policy below covers native file tools only. Bash, shell tools and local MCP servers run without a process sandbox and have ambient host access; the policy does not bind them.\n")
		}
		if r.HasSandbox() {
			network := "blocked"
			if cfg.AllowNetwork {
				network = "allowed"
				if cfg.DenyDNS {
					network += "; DNS blocked"
				}
			}
			fmt.Fprintf(&b, "Process network: %s\n", network)
		}
		if cfg.PrivateHome {
			b.WriteString("Home: private except for explicit grants.\n")
		} else {
			b.WriteString("Home: readable except for masks; home writes require explicit grants.\n")
		}
		if r.HasSandbox() {
			b.WriteString("Known credential paths and Polly runtime storage are masked except where explicitly granted; sensitive inherited environment variables are filtered.\n")
		} else {
			b.WriteString("Native file tools mask known credential paths and Polly runtime storage; commands inherit the full host environment.\n")
		}
		if cfg.DenyWrite {
			b.WriteString("Writes: all denied, including temporary files.\n")
		} else {
			if len(cfg.WritablePaths) == 0 {
				b.WriteString("Writable grants: none declared.\n")
			}
			writeSandboxContextList(&b, "Writable grants (subject to masks and write restrictions)", cfg.WritablePaths)
			switch {
			case !r.HasSandbox():
				b.WriteString("Host temp: writable by commands; there is no process sandbox.\n")
			case cfg.DenyHostTemp:
				b.WriteString("Host temp: no implicit write grant.\n")
			default:
				writeSandboxContextList(&b, "Additional temp write grants", []string{"/tmp", os.TempDir()})
			}
			if runtime.GOOS == "linux" && r.HasSandbox() {
				b.WriteString("Process temp directories are private per command; only explicit path grants expose host files there. Use explicit writable storage for files needed by later calls.\n")
			}
		}
		if sandbox.WriteAllowed(cfg, root) != nil {
			b.WriteString("Working directory: writes denied.\n")
		}
		writeSandboxContextList(&b, "Extra read grants", cfg.ReadPaths)
		writeSandboxContextList(&b, "Additional read/write masks (deeper grants may exempt them)", cfg.DenyPaths)
		writeSandboxContextList(&b, "Write restrictions", cfg.DenyWritePaths)
		gitReadOnly := cfg.DenyWrite || cfg.GitMetadataReadOnly()
		// A worktree's .git pointer file may be protected while the metadata
		// it points to permits ordinary Git writes. Only infer from directories.
		if info, err := os.Lstat(filepath.Join(root, ".git")); err == nil && info.IsDir() {
			gitReadOnly = gitReadOnly || sandbox.WriteAllowed(cfg, filepath.Join(root, ".git")) != nil
		}
		if gitReadOnly {
			b.WriteString("Git metadata: read-only; commit and other Git writes may fail.\n")
		}
		paths, env := sandbox.ExposedCredentials(cfg)
		writeSandboxContextList(&b, "Explicit credential path grants", paths)
		if r.HasSandbox() {
			writeSandboxContextList(&b, "Credential environment passthrough (names only; availability not guaranteed)", env)
			writeSandboxContextList(&b, "Environment supplied by policy (names only; reuse these bindings)", slices.Sorted(maps.Keys(cfg.Env)))
			writeSandboxContextList(&b, "Inherited environment allowlist", cfg.AllowEnv)
		} else {
			// Unsandboxed, the policy env is what bound tools export
			// themselves; passthrough and allowlists filter nothing.
			writeSandboxContextList(&b, "Environment exported to commands (names only; reuse these bindings)", slices.Sorted(maps.Keys(cfg.Env)))
		}
		writeSandboxContextList(&b, "Unix socket grants", cfg.AllowUnixSockets)
		b.WriteString("Reuse configured cache/temp locations and permitted writable storage. A permission error may be an environment limit; diagnose it and report required access instead of retrying unchanged or bypassing restrictions.\n")
	}
	b.WriteString("</sandbox_context>")
	return b.String(), nil
}

// Keep each list bounded, deterministic and unambiguous. Never shorten a path
// into a different apparent grant; omitted entries are counted explicitly.
func writeSandboxContextList(b *strings.Builder, label string, values []string) {
	if len(values) == 0 {
		return
	}
	values = slices.Compact(slices.Sorted(slices.Values(values)))
	var entries []string
	bytes := 0
	for _, value := range values {
		quoted := strconv.Quote(value)
		if len(entries) == 8 || bytes+len(quoted) > 1024 {
			continue
		}
		entries = append(entries, quoted)
		bytes += len(quoted)
	}
	if omitted := len(values) - len(entries); omitted > 0 {
		entries = append(entries, fmt.Sprintf("(%d more entries omitted)", omitted))
	}
	fmt.Fprintf(b, "%s: %s\n", label, strings.Join(entries, ", "))
}
