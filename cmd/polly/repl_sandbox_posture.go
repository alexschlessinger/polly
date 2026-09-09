package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alexschlessinger/pollytool/tools"
)

// sandboxToolSplit partitions sandbox-capable tools by whether they run
// sandboxed. Tools that can't be sandboxed at all (skill helpers, function
// tools) are excluded.
func sandboxToolSplit(reg *tools.ToolRegistry) (sandboxed, unsandboxed []string) {
	for _, t := range reg.All() {
		capable, active := tools.SandboxState(t)
		if !capable {
			continue
		}
		if active {
			sandboxed = append(sandboxed, t.GetName())
		} else {
			unsandboxed = append(unsandboxed, t.GetName())
		}
	}
	sort.Strings(sandboxed)
	sort.Strings(unsandboxed)
	return sandboxed, unsandboxed
}

type sandboxPostureState int

const (
	sandboxPostureDisabled sandboxPostureState = iota
	sandboxPostureUnavailable
	sandboxPostureActive
)

type sandboxPosture struct {
	state       sandboxPostureState
	preset      string
	denyPaths   int
	sandboxed   []string
	unsandboxed []string
	// sshAgentUnavailable notes an ssh preset without a live agent socket, so
	// the inevitable auth failures surface at startup instead of as cryptic
	// ssh errors mid-conversation.
	sshAgentUnavailable bool
}

func currentSandboxPosture(config *Config, state *conversationState) sandboxPosture {
	cfg := config
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.NoSandbox {
		return sandboxPosture{state: sandboxPostureDisabled}
	}
	var reg *tools.ToolRegistry
	if state != nil {
		reg = state.toolRegistry
	}
	if reg == nil || !reg.HasSandbox() {
		return sandboxPosture{state: sandboxPostureUnavailable}
	}
	sandboxed, unsandboxed := sandboxToolSplit(reg)
	preset := cfg.SandboxPreset
	if preset == "" {
		preset = "base"
	}
	return sandboxPosture{
		state:               sandboxPostureActive,
		preset:              preset,
		denyPaths:           len(cfg.DenyPaths),
		sandboxed:           sandboxed,
		unsandboxed:         unsandboxed,
		sshAgentUnavailable: presetSpecContains(preset, "ssh") && !sshAgentSocketLive(),
	}
}

func presetSpecContains(spec, name string) bool {
	for _, part := range strings.Split(spec, "+") {
		if strings.TrimSpace(part) == name {
			return true
		}
	}
	return false
}

func sshAgentSocketLive() bool {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return false
	}
	// Follow symlinks: the sandbox grant resolves SSH_AUTH_SOCK to its
	// canonical target, so a symlinked agent path (launchd aliases, dotfile
	// setups) is live for the sandbox and must read as live here too.
	info, err := os.Stat(sock)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

func sandboxPostureForContext(ctx *replCommandContext) sandboxPosture {
	if ctx == nil {
		return currentSandboxPosture(nil, nil)
	}
	return currentSandboxPosture(ctx.configOrDefault(), ctx.state)
}

func (p sandboxPosture) settingString() string {
	switch p.state {
	case sandboxPostureDisabled:
		return "disabled (--nosandbox)"
	case sandboxPostureUnavailable:
		return "unavailable (no backend)"
	default:
		line := fmt.Sprintf("active (preset: %s; denypaths: %d; tools: %d sandboxed, %d not", p.preset, p.denyPaths, len(p.sandboxed), len(p.unsandboxed))
		if len(p.unsandboxed) > 0 {
			line += ": " + strings.Join(p.unsandboxed, ", ")
		}
		if p.sshAgentUnavailable {
			line += "; ssh: agent unavailable"
		}
		return line + ")"
	}
}

// noticeString is the line frontends' startup notice. Only exceptional
// posture earns one: an active sandbox covering every capable tool, with its
// ssh agent reachable when the preset needs one, returns "" so callers print
// nothing. The TUI shows the posture in its masthead instead.
func (p sandboxPosture) noticeString() string {
	if p.state == sandboxPostureActive && len(p.unsandboxed) == 0 && !p.sshAgentUnavailable {
		return ""
	}
	return p.summaryLine(true)
}

// summaryLine is the sandbox posture in sentence case: the state, the
// preset's parts, optionally how many tools the sandbox covers, and anything
// exceptional a user should know at a glance. The masthead shows it without
// the count; the line frontends' notice includes it.
func (p sandboxPosture) summaryLine(withCount bool) string {
	switch p.state {
	case sandboxPostureDisabled:
		return "Sandbox disabled (--nosandbox)"
	case sandboxPostureUnavailable:
		return "Sandbox unavailable"
	default:
		parts := []string{"Sandbox active", strings.ReplaceAll(p.preset, "+", ", ")}
		if withCount {
			parts = append(parts, fmt.Sprintf("%d tools sandboxed", len(p.sandboxed)))
		}
		if len(p.unsandboxed) > 0 {
			parts = append(parts, "not sandboxed: "+strings.Join(p.unsandboxed, ", "))
		}
		if p.sshAgentUnavailable {
			parts = append(parts, "ssh agent unavailable")
		}
		return strings.Join(parts, " · ")
	}
}

func sandboxListBadge(info tools.SandboxInfo) string {
	if !info.Capable {
		return ""
	}
	if !info.Active {
		if info.OptedOut {
			return "[not sandboxed: opted out]"
		}
		return "[not sandboxed]"
	}
	if info.Config == nil {
		return "[sandboxed]"
	}
	return "[sandboxed: " + sandboxCompactSummary(info) + "]"
}

func sandboxCompactSummary(info tools.SandboxInfo) string {
	cfg := info.Config
	if cfg == nil {
		return ""
	}
	parts := []string{"net off"}
	if cfg.AllowNetwork {
		parts[0] = "net on"
		if cfg.DenyDNS {
			parts = append(parts, "dns off")
		}
	}
	switch {
	case cfg.DenyWrite:
		parts = append(parts, "read-only")
	case hasCustomWritablePaths(cfg.WritablePaths):
		parts = append(parts, "temp+custom writes")
	default:
		parts = append(parts, "temp writes")
	}
	switch {
	case len(cfg.AllowEnv) > 0:
		parts = append(parts, "env allowlist")
	case len(cfg.PassEnv) > 0:
		parts = append(parts, "env filtered+pass")
	default:
		parts = append(parts, "env filtered")
	}
	if len(cfg.AllowUnixSockets) > 0 {
		parts = append(parts, fmt.Sprintf("%d unix socket(s)", len(cfg.AllowUnixSockets)))
	}
	return strings.Join(parts, ", ")
}

func sandboxShowDetail(info tools.SandboxInfo) string {
	if !info.Capable {
		return ""
	}
	if !info.Active {
		if info.OptedOut {
			return "not sandboxed (opted out)"
		}
		return "not sandboxed"
	}
	cfg := info.Config
	if cfg == nil {
		return "active (details unavailable)"
	}
	parts := []string{"network off"}
	if cfg.AllowNetwork {
		parts[0] = "network on"
		if cfg.DenyDNS {
			parts = append(parts, "DNS off")
		}
	}
	switch {
	case cfg.DenyWrite:
		parts = append(parts, "writes denied")
	case hasCustomWritablePaths(cfg.WritablePaths):
		parts = append(parts, "writes limited to temp and custom paths")
	default:
		parts = append(parts, "writes limited to temp")
	}
	switch {
	case len(cfg.AllowEnv) > 0:
		parts = append(parts, "env allowlist active")
	case len(cfg.PassEnv) > 0:
		parts = append(parts, "env filters credential-like variables (passing: "+strings.Join(cfg.PassEnv, ", ")+")")
	default:
		parts = append(parts, "env filters credential-like variables")
	}
	if len(cfg.AllowUnixSockets) > 0 {
		parts = append(parts, fmt.Sprintf("Unix sockets: %d granted", len(cfg.AllowUnixSockets)))
	}
	return strings.Join(parts, "; ")
}

func hasCustomWritablePaths(paths []string) bool {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if !isTempWritablePath(p) {
			return true
		}
	}
	return false
}

func isTempWritablePath(path string) bool {
	clean := filepath.Clean(path)
	for _, candidate := range []string{os.TempDir(), "/tmp", "/private/tmp"} {
		if candidate == "" {
			continue
		}
		candidate = filepath.Clean(candidate)
		if clean == candidate {
			return true
		}
		// Effective sandbox configs are prepared before tools are loaded, which
		// canonicalizes writable grants. Match the canonical form of known temp
		// roots too (notably /var/... -> /private/var/... on macOS) without
		// resolving arbitrary writable paths during display.
		if real, err := filepath.EvalSymlinks(candidate); err == nil && clean == filepath.Clean(real) {
			return true
		}
	}
	return false
}
