package tools

import "context"

// The helpers in this file let a registry mirror tools served elsewhere,
// such as polly's in-container helper: staged additions, skill allowances,
// a selection bound onto the registry's own tools, and the metadata a
// proxy must reproduce.

// StageTools stages tools for the next CommitPendingChanges, the way a skill
// activation stages the tools it loaded. They own no MCP client.
func (r *ToolRegistry) StageTools(staged ...Tool) {
	records := make([]stagedToolRecord, 0, len(staged))
	for _, tool := range staged {
		if name := registeredName(tool); name != "" {
			records = append(records, stagedToolRecord{name: name, tool: tool})
		}
	}
	r.stagePreparedTools(records)
}

// StageSkillAllowance stages a skill's allowed-tool patterns and the tools it
// loaded, applied by the next CommitPendingChanges.
func (r *ToolRegistry) StageSkillAllowance(patterns, autoAllowed []string) {
	r.stageSkillAllowance(patterns, autoAllowed)
}

// RestrictView narrows the registry to the tools matching allow, its own
// registrations included, and bounds tools it stages later. A nil allow
// leaves the registry as it is; built-ins always pass.
func (r *ToolRegistry) RestrictView(allow []string) {
	if allow == nil {
		return
	}
	patterns := append([]string(nil), allow...)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.viewAllowed = func(name string) bool {
		return !contextPrivateTool(name) && matchesAnyToolPattern(patterns, name)
	}
}

// AlwaysAllowed reports whether name is exempt from skill allow-lists.
func (r *ToolRegistry) AlwaysAllowed(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.alwaysAllowedTools[name]
}

// Builtin reports whether name is one of the registry's built-ins.
func (r *ToolRegistry) Builtin(name string) bool { return r.isBuiltin(name) }

// ParseAllowedToolPatterns splits a skill's allowed-tools declaration into
// patterns.
func ParseAllowedToolPatterns(value string) []string { return parseAllowedToolPatterns(value) }

// Pipefail reports whether WithPipefail marked ctx.
func Pipefail(ctx context.Context) bool {
	pipefail, _ := ctx.Value(pipefailKey{}).(bool)
	return pipefail
}
