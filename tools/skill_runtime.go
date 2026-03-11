package tools

import (
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
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
	registry.Register(runtime.activateTool)
	registry.Register(NewSkillReadFileTool(catalog))
	bt := NewBashTool("")
	cfg := sandbox.DefaultConfig()
	cfg.AllowNetwork = true
	if registry.HasSandbox() {
		if sb, err := registry.NewSandboxDirect(cfg); err == nil {
			bt = bt.WithSandbox(sb)
		}
	}
	runtime.activateTool.writablePaths = cfg.WritablePaths
	registry.Register(bt)
	registry.MarkAlwaysAllowed("activate_skill")
	registry.MarkAlwaysAllowed("read_skill_file")

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
