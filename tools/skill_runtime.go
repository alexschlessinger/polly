package tools

import (
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/skills"
)

// ErrSkillRuntimeUnavailable is returned when activation or restore is attempted without discovered skills.
var ErrSkillRuntimeUnavailable = errors.New("skill runtime is unavailable")

// SkillRuntime exposes a public API for skill activation, restore, and state export.
type SkillRuntime struct {
	catalog      *skills.Catalog
	registry     *ToolRegistry
	activateTool *SkillActivateTool
}

// NewSkillRuntime registers the built-in skill tools on the registry and returns a runtime facade.
func NewSkillRuntime(catalog *skills.Catalog, registry *ToolRegistry) (*SkillRuntime, error) {
	if registry == nil {
		return nil, fmt.Errorf("tool registry is nil")
	}

	runtime := &SkillRuntime{
		catalog:  catalog,
		registry: registry,
	}
	if catalog == nil || catalog.IsEmpty() {
		return runtime, nil
	}

	runtime.activateTool = NewSkillActivateTool(catalog, registry)
	readFileTool := NewSkillReadFileTool(catalog)
	newSkillBash := func() (*BashTool, error) {
		bt := newBashTool("")
		bt.siblingLoaded = registry.hasVisibleTool
		if registry.HasSandbox() {
			// Fail closed: the skill bash tool must inherit the registry's base
			// policy and must not fall back to an independently weaker config.
			sb, effectiveCfg, err := registry.newSandboxFor("skill bash", nil)
			if err != nil {
				return nil, fmt.Errorf("sandbox for skill bash tool: %w", err)
			}
			bt = bt.withSandboxConfig(sb, effectiveCfg)
		} else if err := registry.requireProcessSandbox("skill bash tool"); err != nil {
			return nil, err
		}
		return bt, nil
	}

	var bashCandidate *BashTool
	if _, ok := registry.registeredTool("bash"); !ok {
		var err error
		bashCandidate, err = newSkillBash()
		if err != nil {
			return nil, err
		}
	}

	// The initial bash snapshot may change while its sandbox is being prepared.
	// The atomic commit returns false without mutation only when that snapshot had
	// bash but the tool was removed before commit, so construct a candidate and
	// retry. A non-nil candidate makes the second commit unconditional.
	if !registry.registerSkillRuntimeTools(runtime.activateTool, readFileTool, bashCandidate) {
		var err error
		bashCandidate, err = newSkillBash()
		if err != nil {
			return nil, err
		}
		_ = registry.registerSkillRuntimeTools(runtime.activateTool, readFileTool, bashCandidate)
	}

	return runtime, nil
}

// Catalog returns the discovered skill catalog backing the runtime.
func (r *SkillRuntime) Catalog() *skills.Catalog {
	if r == nil {
		return nil
	}
	return r.catalog
}

// Enabled reports whether discovered skills are available for activation.
func (r *SkillRuntime) Enabled() bool {
	return r != nil && r.activateTool != nil
}

// Activate activates a skill immediately and commits the resulting tools and policy into the registry.
func (r *SkillRuntime) Activate(name string) (string, error) {
	if !r.Enabled() {
		return "", ErrSkillRuntimeUnavailable
	}

	result, err := r.activateTool.activate(name)
	if err != nil {
		return "", err
	}
	r.registry.CommitPendingChanges()
	return result, nil
}

// Restore reactivates previously active skills and commits their tools and policy into the registry.
func (r *SkillRuntime) Restore(names []string) error {
	if len(names) == 0 {
		return nil
	}
	if !r.Enabled() {
		return ErrSkillRuntimeUnavailable
	}

	for _, name := range names {
		if _, err := r.activateTool.activate(name); err != nil {
			return err
		}
	}
	r.registry.CommitPendingChanges()
	return nil
}

// ActivatedSkills returns the currently activated skill names in stable order.
func (r *SkillRuntime) ActivatedSkills() []string {
	if !r.Enabled() {
		return nil
	}
	return r.activateTool.ActivatedSkills()
}
