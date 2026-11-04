package swarm

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

func TestAgentSandboxArguments(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, doneModel(), 1, 4)
	r.RegisterParentTools(r.config.Registry)
	spawn, _, _ := r.config.Registry.GetIfAllowed("spawn_agent")
	for _, key := range []string{"sandbox", "model", "model_host"} {
		property, ok := spawn.GetSchema().Properties()[key].(map[string]any)
		if !ok || property["type"] != "string" || slices.Contains(spawn.GetSchema().Required(), key) {
			t.Fatalf("%s is not an optional string: %+v", key, spawn.GetSchema())
		}
	}
	ctx := context.Background()
	for _, value := range []any{"", " ", "no-such-preset", false, true, nil, 1, []any{"workspace"}} {
		if _, err := spawn.Execute(ctx, map[string]any{"task_name": "inspect", "message": "inspect", "read_only": true, "sandbox": value}); err == nil {
			t.Fatalf("spawn accepted sandbox %v", value)
		}
		if _, err := decodeRequest(map[string]any{"label": "Inspect", "task": "inspect", "sandbox": value}); err == nil {
			t.Fatalf("workflow accepted sandbox %v", value)
		}
	}
	for _, spec := range []string{"", "workspace+net"} {
		args := map[string]any{"label": "Inspect", "task": "inspect", "model": "provider/model", "modelHost": "route"}
		if spec != "" {
			args["sandbox"] = spec
		}
		req, err := decodeRequest(args)
		if err != nil || req.Sandbox != spec || req.Model != "provider/model" || req.ModelHost != "route" {
			t.Fatalf("workflow request: %+v %v", req, err)
		}
	}
	if _, err := r.Agent(ctx, "", AgentRequest{Label: "Inspect", Task: "inspect", ReadOnly: true, Sandbox: "bad-preset"}); err == nil {
		t.Fatal("Go request accepted unknown sandbox")
	}
	s, err := r.State(ctx)
	if err != nil || len(s.Members) != 0 || len(s.Contexts) != 0 {
		t.Fatalf("invalid requests allocated state: %+v %v", s, err)
	}
}

func TestAgentSandboxConfigValidation(t *testing.T) {
	t.Parallel()
	for _, spec := range []string{" ", "false", "none", "bad-preset"} {
		if _, err := New(Config{AgentSandbox: spec}); err == nil || !strings.Contains(err.Error(), "agent sandbox") {
			t.Fatalf("invalid floor %q: %v", spec, err)
		}
	}
}

func TestAgentSandboxScopeForwarding(t *testing.T) {
	t.Parallel()
	for _, floor := range []string{"", "workspace"} {
		t.Run("floor="+floor, func(t *testing.T) {
			r := rebuildRuntime(t, runtimeTest(t, doneModel(), 1, 8), func(c *Config) { c.AgentSandbox = floor })
			rec := &openRecorder{}
			r = runtimeWithOpen(t, r, rec)
			suspendAutoRelease(t, r)
			ctx := context.Background()
			for _, spec := range []string{"", "workspace+net"} {
				for _, entry := range []string{"spawn", "subagent", "workflow"} {
					args := map[string]any{"label": "Inspect", "model": "provider/model"}
					if spec != "" {
						args["sandbox"] = spec
					}
					var err error
					switch entry {
					case "spawn":
						r.RegisterParentTools(r.config.Registry)
						spawn, _, _ := r.config.Registry.GetIfAllowed("spawn_agent")
						args["task_name"] = "inspect"
						if spec != "" {
							args["task_name"] = "restricted"
						}
						args["message"], args["read_only"], args["model_host"] = "inspect", true, "route"
						_, err = spawn.Execute(ctx, args)
						awaitIdle(t, r, ctx)
					case "subagent":
						_, err = r.Spawn(ctx, subagent.Request{Label: "Inspect", Task: "inspect", ReadOnly: true, Model: "provider/model", ModelHost: "route", Sandbox: spec})
					case "workflow":
						args["task"], args["readOnly"], args["modelHost"] = "inspect", true, "route"
						h := &workflowHost{runtime: r, controller: "workflow"}
						_, err = h.Call(ctx, workflow.Operation{Kind: "agent", Args: args})
						h.close()
					}
					if err != nil {
						t.Fatalf("%s: %v", entry, err)
					}
					want := sandboxPresets(nil, floor, spec)
					if got := rec.scopes[len(rec.scopes)-1].Grant.SandboxPresets; !slices.Equal(got, want) {
						t.Fatalf("%s scope presets %v, want %v", entry, got, want)
					}
				}
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range s.Members {
				c := s.Contexts[m.Context]
				if c == nil || !slices.Equal(c.SandboxPresets, m.SandboxPresets) || m.Model != "provider/model" || m.ModelHost != "route" {
					t.Fatalf("member/context authority: %+v %+v", m, c)
				}
			}
		})
	}
}

func TestAgentSandboxFloorDoesNotRestrictWorkflowContext(t *testing.T) {
	t.Parallel()
	r := rebuildRuntime(t, runtimeTest(t, doneModel(), 1, 2), func(c *Config) { c.AgentSandbox = "workspace" })
	rec := &openRecorder{}
	r = runtimeWithOpen(t, r, rec)
	suspendAutoRelease(t, r)
	ctx := context.Background()
	h := &workflowHost{runtime: r, controller: "workflow"}
	defer h.close()
	value, err := h.Call(ctx, workflow.Operation{Kind: "context", Args: map[string]any{"readOnly": true}})
	if err != nil {
		t.Fatal(err)
	}
	id := value.(string)
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.registry(ctx, s, s.Contexts[id]); err != nil {
		t.Fatal(err)
	}
	if len(s.Contexts[id].SandboxPresets) != 0 || len(rec.scopes[0].Grant.SandboxPresets) != 0 {
		t.Fatal("agent default restricted the parent's workflow context")
	}
	if _, err := h.Call(ctx, workflow.Operation{Kind: "agent", Args: map[string]any{"label": "Inspect", "task": "inspect", "readOnly": true, "context": id}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.scopes[1].Grant.SandboxPresets, []string{"workspace"}) {
		t.Fatalf("agent seeded from workflow context lost floor: %+v", rec.scopes[1])
	}
}

func TestAgentSandboxSurvivesRestoreReleaseAndRefresh(t *testing.T) {
	t.Parallel()
	r := rebuildRuntime(t, runtimeTest(t, doneModel(), 1, 8), func(c *Config) {
		c.AgentSandbox = "workspace+net"
		c.Request = llm.CompletionRequest{Model: "provider/model", ModelHost: "route"}
	})
	rec := &openRecorder{}
	r = runtimeWithOpen(t, r, rec)
	suspendAutoRelease(t, r)
	ctx := context.Background()
	first, err := r.Agent(ctx, "", AgentRequest{Label: "Inspect", Task: "inspect", ReadOnly: true, Sandbox: "workspace", Tools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	want := []string{"workspace+net", "workspace"}
	check := func() *State {
		t.Helper()
		s, err := r.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m := s.Members[first.Session]
		if !slices.Equal(m.SandboxPresets, want) || m.Model != "provider/model" || m.ModelHost != "route" || m.Tools == nil {
			t.Fatalf("member lost immutable authority: %+v", m)
		}
		if c := s.Contexts[m.Context]; c != nil && !slices.Equal(c.SandboxPresets, want) {
			t.Fatalf("context lost sandbox: %+v", c)
		}
		return s
	}
	check()
	// Recover without New's automatic workspace reclamation, so both a bound
	// context and later a released member exercise floor accumulation.
	restore := func(floor string) {
		t.Helper()
		config := r.config
		config.AgentSandbox = floor
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		var state *State
		r, state, err = newRuntime(config)
		if err != nil {
			t.Fatal(err)
		}
		current := r
		t.Cleanup(func() { current.Close() })
		suspendAutoRelease(t, r)
		if !slices.Equal(state.Members[first.Session].SandboxPresets, want) {
			t.Fatalf("restore returned stale constraints: %+v", state.Members[first.Session])
		}
		check()
	}
	want = append(want, "private-home")
	restore("private-home")
	restore("") // removing the host default cannot remove the saved floor
	if _, err := r.Agent(ctx, "", AgentRequest{Session: first.Session, Task: "loosen", Sandbox: "workspace+net"}); err == nil || !strings.Contains(err.Error(), "inherits") {
		t.Fatalf("continuation replaced sandbox: %v", err)
	}
	if _, err := r.releaseWorkspace(ctx, first.Context); err != nil {
		t.Fatal(err)
	}
	if s := check(); s.Members[first.Session].Context != "" {
		t.Fatal("workspace was not released")
	}
	want = append(want, "workspace+git")
	restore("workspace+git")
	for n, refresh := range []bool{false, true} {
		_, _, err := followupTool(t, r, []string{"ordinary", "refresh"}[n], map[string]any{"target": first.Session, "message": "inspect again", "refresh": refresh})
		if err != nil {
			t.Fatal(err)
		}
		awaitIdle(t, r, ctx)
		check()
		if got := rec.scopes[len(rec.scopes)-1].Grant.SandboxPresets; !slices.Equal(got, want) {
			t.Fatalf("followup refresh=%t granted %v, want %v", refresh, got, want)
		}
		admitParent(t, r)
	}
	view, err := r.inspectAgents(ctx, r.ID, tools.Args{"details": true})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(view)
	if err != nil || !strings.Contains(string(data), `"sandboxPresets":["workspace+net","workspace","private-home","workspace+git"]`) {
		t.Fatalf("inspection omitted policy: %s %v", data, err)
	}
}
