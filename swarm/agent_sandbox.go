package swarm

import (
	"fmt"
	"slices"

	"github.com/alexschlessinger/pollytool/tools"
)

// agentSandboxArgument distinguishes omission from an explicit empty value or
// a non-string. Neither null nor false may disable an inherited restriction.
func agentSandboxArgument(args map[string]any) (string, error) {
	value, present := args["sandbox"]
	if !present {
		return "", nil
	}
	spec, ok := value.(string)
	if !ok {
		return "", fail("invalid_args", "sandbox must be a preset string; omit it to inherit")
	}
	if err := tools.ValidateAgentSandboxPreset(spec); err != nil {
		return "", fail("invalid_args", "sandbox: "+err.Error())
	}
	return spec, nil
}

// sandboxPresets copies and accumulates restrictive policies. The tools host
// intersects them, rather than allowing a later request to replace a floor.
func sandboxPresets(saved []string, added ...string) []string {
	out := slices.Clone(saved)
	for _, spec := range added {
		if spec != "" && !slices.Contains(out, spec) {
			out = append(out, spec)
		}
	}
	return out
}

func validateSavedSandbox(kind, id string, presets []string) error {
	for _, spec := range presets {
		if err := tools.ValidateAgentSandboxPreset(spec); err != nil {
			return fmt.Errorf("%s %s sandbox: %w", kind, id, err)
		}
	}
	return nil
}

func (r *Runtime) restoredAgentSandbox(m *Member, c *ExecutionContext) ([]string, error) {
	presets := m.SandboxPresets
	if c != nil {
		presets = sandboxPresets(presets, c.SandboxPresets...)
	}
	presets = sandboxPresets(presets, r.config.AgentSandbox)
	return presets, validateSavedSandbox("member", m.ID, presets)
}
