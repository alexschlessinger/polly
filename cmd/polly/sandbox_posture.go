package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
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
	readGrants  int
	privateHome bool
	sandboxed   []string
	unsandboxed []string
	// sshAgentUnavailable notes an ssh preset without a live agent socket, so
	// the inevitable auth failures surface at startup instead of as cryptic
	// ssh errors mid-conversation.
	sshAgentUnavailable bool
	// credentials names what the policy commands run under, its layers
	// included, exposes of the credential deny list: grants at or inside a
	// masked path and credential-shaped variables passed through. Exposure is
	// allowed when chosen; it is never silent.
	credentials []string
	// profile summarizes the workspace's sandbox profile, "" without one.
	profile string
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
	readGrants := 0
	privateHome := false
	var credentials []string
	if policy, active, err := reg.SandboxReadPolicy(); err == nil && active {
		readGrants = len(policy.ReadPaths)
		privateHome = policy.PrivateHome
		credentials = exposedCredentialNames(policy)
	}
	var profile string
	if state != nil {
		profile = state.sandboxProfile.summary()
	}
	return sandboxPosture{
		state:               sandboxPostureActive,
		preset:              preset,
		denyPaths:           len(cfg.DenyPaths),
		readGrants:          readGrants,
		privateHome:         privateHome,
		sandboxed:           sandboxed,
		unsandboxed:         unsandboxed,
		sshAgentUnavailable: presetSpecContains(preset, "ssh") && !sshAgentSocketLive(),
		credentials:         credentials,
		profile:             profile,
	}
}

// exposedCredentialNames lists a policy's exposed credential paths, spelled
// from the home directory, followed by its passed-through credential-shaped
// variables.
func exposedCredentialNames(cfg sandbox.Config) []string {
	paths, env := sandbox.ExposedCredentials(cfg)
	names := make([]string, 0, len(paths)+len(env))
	for _, path := range paths {
		names = append(names, homeRelativePath(path))
	}
	return append(names, env...)
}

// homeRelativePath spells a path under the home directory with a leading ~.
func homeRelativePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	homes := []string{filepath.Clean(home)}
	if real, err := filepath.EvalSymlinks(home); err == nil && filepath.Clean(real) != homes[0] {
		homes = append(homes, filepath.Clean(real))
	}
	for _, home := range homes {
		if rel, err := filepath.Rel(home, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if rel == "." {
				return "~"
			}
			return filepath.Join("~", rel)
		}
	}
	return path
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
		home := "readable, known credentials masked"
		if p.privateHome {
			home = "private"
		}
		line := fmt.Sprintf("active (preset: %s; home: %s, %d read grants; denypaths: %d; tools: %d sandboxed, %d not", p.preset, home, p.readGrants, p.denyPaths, len(p.sandboxed), len(p.unsandboxed))
		if len(p.unsandboxed) > 0 {
			line += ": " + strings.Join(p.unsandboxed, ", ")
		}
		if p.sshAgentUnavailable {
			line += "; ssh: agent unavailable"
		}
		if p.profile != "" {
			line += "; " + p.profile
		}
		if len(p.credentials) > 0 {
			line += "; credentials: " + strings.Join(p.credentials, ", ")
		}
		return line + ")"
	}
}

// noticeString is the line frontends' startup notice. Only exceptional
// posture earns one: an active sandbox covering every capable tool, with its
// ssh agent reachable when the preset needs one and no credential exposed,
// returns "" so callers print nothing. The TUI shows the posture in its
// masthead instead.
func (p sandboxPosture) noticeString() string {
	if p.state == sandboxPostureActive && len(p.unsandboxed) == 0 && !p.sshAgentUnavailable && len(p.credentials) == 0 {
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
		if p.profile != "" {
			parts = append(parts, p.profile)
		}
		if withCount {
			parts = append(parts, fmt.Sprintf("%d tools sandboxed", len(p.sandboxed)))
		}
		if len(p.unsandboxed) > 0 {
			parts = append(parts, "not sandboxed: "+strings.Join(p.unsandboxed, ", "))
		}
		if p.sshAgentUnavailable {
			parts = append(parts, "ssh agent unavailable")
		}
		if len(p.credentials) > 0 {
			parts = append(parts, "credentials: "+strings.Join(p.credentials, ", "))
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

// sandboxFacet is one aspect of a tool's sandbox config in two wordings:
// the list badge's and the /tools show detail's.
type sandboxFacet struct{ short, long string }

// sandboxFacets describes a config facet by facet: network and DNS, the
// write policy, the environment policy, and granted unix sockets.
func sandboxFacets(info tools.SandboxInfo) []sandboxFacet {
	cfg := info.Config
	if cfg == nil {
		return nil
	}
	facets := []sandboxFacet{{"net off", "network off"}}
	if cfg.AllowNetwork {
		facets[0] = sandboxFacet{"net on", "network on"}
		if cfg.DenyDNS {
			facets = append(facets, sandboxFacet{"dns off", "DNS off"})
		}
	}
	switch {
	case cfg.DenyWrite:
		facets = append(facets, sandboxFacet{"read-only", "writes denied"})
	case hasCustomWritablePaths(cfg.WritablePaths):
		facets = append(facets, sandboxFacet{"temp+custom writes", "writes limited to temp and custom paths"})
	default:
		facets = append(facets, sandboxFacet{"temp writes", "writes limited to temp"})
	}
	switch {
	case len(cfg.AllowEnv) > 0:
		facets = append(facets, sandboxFacet{"env allowlist", "env allowlist active"})
	case len(cfg.PassEnv) > 0:
		facets = append(facets, sandboxFacet{"env filtered+pass", "env filters credential-like variables (passing: " + strings.Join(cfg.PassEnv, ", ") + ")"})
	default:
		facets = append(facets, sandboxFacet{"env filtered", "env filters credential-like variables"})
	}
	if n := len(cfg.AllowUnixSockets); n > 0 {
		facets = append(facets, sandboxFacet{fmt.Sprintf("%d unix socket(s)", n), fmt.Sprintf("Unix sockets: %d granted", n)})
	}
	if names := exposedCredentialNames(*cfg); len(names) > 0 {
		facets = append(facets, sandboxFacet{"credentials exposed", "credentials exposed: " + strings.Join(names, ", ")})
	}
	return facets
}

func sandboxCompactSummary(info tools.SandboxInfo) string {
	var parts []string
	for _, f := range sandboxFacets(info) {
		parts = append(parts, f.short)
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
	if info.Config == nil {
		return "active (details unavailable)"
	}
	var parts []string
	for _, f := range sandboxFacets(info) {
		parts = append(parts, f.long)
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

// sandboxNoticeLine reports exceptional sandbox posture at REPL startup.
// An active sandbox with no issues needs no notice.
func sandboxNoticeLine(config *Config, state *conversationState) string {
	return currentSandboxPosture(config, state).noticeString()
}
