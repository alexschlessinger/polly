package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/alexschlessinger/pollytool/internal/envstorage"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

type sandboxPreparation struct {
	Allocations []envstorage.Allocation `json:"allocations"`
	Env         map[string]string       `json:"env,omitempty"`
	Links       []envstorage.Link       `json:"links,omitempty"`
}

func (s *sandboxInit) registerPrepare(registry *tools.ToolRegistry) {
	registry.Register(&tools.Func{
		Name: sandboxPrepareTool, Exclusive: true, LongRunning: true,
		Desc: "Prepare and persist isolated build state during /init, without a permission review. Allocate named cache, state and config directories owned by this workspace; bind environment variables to @cache/<name>, @state/<name> or @config/<name>, optionally with a subpath. Existing explicit settings win. No host reads, credentials, sockets, shell hooks or installation commands. Recipes are in the sandbox-setup skill. Returns resolved paths; use ordinary sandboxed tools to write configuration and restore dependencies.",
		Params: schema.Params{
			"allocations": schema.Array("Storage declarations. Shared is allowed only for a cache whose recipe supports concurrent writers.", map[string]any{"type": "object", "properties": map[string]any{"name": schema.S("Stable lowercase name: letters, digits and hyphens."), "kind": schema.Enum("Cleanup category.", "cache", "state", "config"), "purpose": schema.S("What the allocation holds."), "recipe": schema.S("Recipe name and version, or custom."), "shared": schema.Bool("Whether writable worktrees may share this disposable cache.")}, "required": []string{"name", "kind", "purpose"}, "additionalProperties": false}),
			"env":         map[string]any{"type": "object", "description": "Environment variable names mapped to declared allocation paths; values are paths only.", "additionalProperties": map[string]any{"type": "string"}},
			"links":       schema.Array("Configuration links from a child of @state to @config. Targets may be missing files; directory targets are created.", map[string]any{"type": "object", "properties": map[string]any{"path": schema.S("Link location under a declared @state allocation."), "target": schema.S("Target under a declared @config allocation."), "directory": schema.Bool("Create the target as a directory.")}, "required": []string{"path", "target"}, "additionalProperties": false}),
		}, Required: []string{"allocations"}, Run: s.runPrepare,
	})
	registry.MarkAlwaysAllowed(sandboxPrepareTool)
}

func (s *sandboxInit) runPrepare(ctx context.Context, args tools.Args) (string, error) {
	if err := s.active(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	if len(data) > 32<<10 {
		return "", tools.NewToolError("preparation exceeds 32 KiB", sandboxInitBadItem)
	}
	var request sandboxPreparation
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return "", tools.NewToolError(err.Error(), sandboxInitBadItem)
	}
	s.trial.Lock()
	defer s.trial.Unlock()
	profile := s.state.sandboxProfile
	if profile == nil || profile.off != "" {
		return "", tools.NewToolError("workspace profiles must be enabled for automatic preparation", sandboxInitBadItem)
	}
	base, on, err := s.state.toolRegistry.BaseSandboxPolicy()
	if err != nil {
		return "", err
	}
	if !on || base.DenyWrite {
		return "", tools.NewToolError("this sandbox does not allow environment preparation", sandboxInitBadItem)
	}
	if len(request.Env) > 64 {
		return "", tools.NewToolError("at most 64 environment bindings are allowed", sandboxInitBadItem)
	}
	var conflicts []string
	err = profile.updateProfileContext(ctx, s.state.toolRegistry, func(file *sandboxProfile) error {
		if file.ConfigurationCheckout == "" {
			file.ConfigurationCheckout = profile.ws.checkoutKey
		}
		for _, a := range request.Allocations {
			if a.Disabled {
				return errors.New("preparation cannot declare disabled storage")
			}
			at := slices.IndexFunc(file.Storage.Allocations, func(old envstorage.Allocation) bool { return old.Key() == a.Key() })
			if at < 0 {
				file.Storage.Allocations = append(file.Storage.Allocations, a)
			} else {
				old := file.Storage.Allocations[at]
				old.Disabled = false
				if old != a {
					return fmt.Errorf("allocation %s already exists with different settings; reuse its declaration", a.Key())
				}
				file.Storage.Allocations[at] = a
			}
		}
		for _, link := range request.Links {
			at := slices.IndexFunc(file.Storage.Links, func(old envstorage.Link) bool { return old.Path == link.Path })
			if at < 0 {
				file.Storage.Links = append(file.Storage.Links, link)
			} else if file.Storage.Links[at] != link {
				return fmt.Errorf("configuration link %s already has another target", link.Path)
			}
		}
		if err := file.Storage.Validate(); err != nil {
			return err
		}
		for _, name := range slices.Sorted(maps.Keys(request.Env)) {
			value := request.Env[name]
			if err := checkProfileEnvName(name); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if _, _, err := file.Storage.Lookup(value); err != nil {
				return err
			}
			if _, exists := base.Env[name]; exists {
				conflicts = append(conflicts, name+" keeps its explicit base setting")
				continue
			}
			at := slices.IndexFunc(file.Items, func(item sandboxProfileItem) bool { return item.Kind == profileEnv && item.Name == name })
			if at >= 0 && !file.Items[at].Automatic {
				conflicts = append(conflicts, name+" keeps its explicit setting")
				continue
			}
			item := sandboxProfileItem{Kind: profileEnv, Name: name, Value: value, Automatic: true}
			if at < 0 {
				file.Items = append(file.Items, item)
			} else {
				file.Items[at] = item
			}
			if slices.ContainsFunc(profile.session, func(item sandboxProfileItem) bool { return item.Kind == profileEnv && item.Name == name }) {
				conflicts = append(conflicts, name+" is overridden for this session")
			}
		}
		return ctx.Err()
	}, nil, true)
	if err != nil {
		return "", tools.NewToolError(err.Error(), sandboxInitBadItem)
	}
	paths := map[string]string{}
	for _, a := range profile.profile.Storage.Allocations {
		paths[a.Key()] = profile.ws.storageRoots().Path(a)
	}
	cfg, active, err := s.state.toolRegistry.SandboxReadPolicy()
	if err != nil {
		return "", err
	}
	if !active {
		return "", errors.New("sandbox disappeared during preparation")
	}
	effective := map[string]string{}
	for name := range request.Env {
		if value, ok := cfg.Env[name]; ok {
			effective[name] = value
		}
	}
	return marshalSandboxInitReport(map[string]any{"saved": true, "storage": paths, "env": effective, "conflicts": conflicts, "note": "Prepared settings apply now and on reopen. Run dependency bootstrap and final build/test commands through ordinary sandboxed bash; preparation itself is not verification."})
}
