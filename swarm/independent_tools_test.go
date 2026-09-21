package swarm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

const (
	independentRepoGuidance = "INDEPENDENT REPOSITORY GUIDANCE"
	independentToolGuidance = "INDEPENDENT TOOL GUIDANCE"
)

// independentToolset is an OpenTools that supplies its own tools: an
// in-memory reader and a writer that edits the scope's root directly, so
// the coordinator's Git capture sees the edit. It enforces the scope's
// selection and read-only grant itself and never touches native tools.
type independentToolset struct {
	mu     sync.Mutex
	wrote  []string
	scopes []tools.ToolScope
}

func (s *independentToolset) open(_ context.Context, scope tools.ToolScope) (tools.ToolBinding, error) {
	s.mu.Lock()
	s.scopes = append(s.scopes, scope)
	s.mu.Unlock()
	candidates := []tools.Tool{
		&tools.Func{Name: "read_mem", Desc: "Read the in-memory tree", Params: schema.Params{"path": schema.S("path")}, Required: []string{"path"}, Run: func(context.Context, tools.Args) (string, error) {
			return "IN-MEMORY NOTES", nil
		}},
		&tools.Func{Name: "write_root", Desc: "Write a file into the workspace", Params: schema.Params{"name": schema.S("name"), "content": schema.S("content")}, Required: []string{"name", "content"}, Run: func(_ context.Context, a tools.Args) (string, error) {
			if scope.Grant.ReadOnly {
				return "", tools.NewToolError("workspace is read-only", "denied")
			}
			path := filepath.Join(scope.Root, a.String("name"))
			if err := os.WriteFile(path, []byte(a.String("content")), 0o600); err != nil {
				return "", err
			}
			s.mu.Lock()
			s.wrote = append(s.wrote, path)
			s.mu.Unlock()
			return "written", nil
		}},
	}
	registry := tools.NewToolRegistry(nil)
	var omitted []string
	for _, tool := range candidates {
		name := tool.GetName()
		if scope.AllowedTools != nil && !matchesAny(scope.AllowedTools, name) {
			omitted = append(omitted, name)
			continue
		}
		registry.Register(tool)
	}
	return tools.ToolBinding{Registry: registry, Instructions: independentRepoGuidance, ToolInstructions: independentToolGuidance, Omitted: omitted, Close: registry.Close}, nil
}

func matchesAny(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if tools.MatchesToolPattern(pattern, name) {
			return true
		}
	}
	return false
}

func runtimeWithToolset(t *testing.T, r *Runtime, set *independentToolset, configure func(*Config)) *Runtime {
	t.Helper()
	return rebuildRuntime(t, r, func(c *Config) {
		c.OpenTools = set.open
		if configure != nil {
			configure(c)
		}
	})
}

func toolNames(req *llm.CompletionRequest) []string {
	var names []string
	for _, tool := range req.Tools {
		names = append(names, tool.GetName())
	}
	return names
}

func TestMemberRunsAnIndependentToolset(t *testing.T) {
	var calls atomic.Int32
	var seen []string
	var system string
	model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			seen = toolNames(req)
			system = req.Messages[0].Content
			return iterationTool("read", "read_mem", `{"path":"notes.txt"}`)
		}
		if !strings.Contains(req.Messages[len(req.Messages)-1].Content, "IN-MEMORY NOTES") {
			t.Error("independent tool result missing")
		}
		return answer("done")
	})
	set := &independentToolset{}
	r := runtimeWithToolset(t, runtimeTest(t, model, 1, 1), set, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "read", ReadOnly: true, Tools: []string{"read_mem"}})
	if err != nil || result.Text != "done" {
		t.Fatalf("%+v %v", result, err)
	}
	for _, want := range []string{"read_mem", "send_message", "swarm_publish"} {
		if !slices.Contains(seen, want) {
			t.Fatalf("member tools %v lack %s", seen, want)
		}
	}
	if slices.Contains(seen, "write_root") || slices.Contains(seen, "bash") || slices.Contains(seen, "read_file") {
		t.Fatalf("member tools %v exceed the independent selection", seen)
	}
	for _, want := range []string{independentRepoGuidance, independentToolGuidance} {
		if !strings.Contains(system, want) {
			t.Fatalf("system prompt lacks %q", want)
		}
	}
	// Repository guidance persists with the member's history; tool guidance
	// is ephemeral, rendered per request.
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session, err := r.config.Store.Acquire(ctx, s.Members[result.Session].Name, sessions.AcquireOptions{ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	history, err := session.GetHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 || history[0].Role != messages.MessageRoleSystem {
		t.Fatalf("history = %+v", history)
	}
	if !strings.Contains(history[0].Content, independentRepoGuidance) || strings.Contains(history[0].Content, independentToolGuidance) {
		t.Fatalf("persisted system prompt = %q", history[0].Content)
	}
	if len(set.scopes) != 1 || set.scopes[0].Root == "" || !set.scopes[0].Grant.ReadOnly {
		t.Fatalf("scopes = %+v", set.scopes)
	}
}

func TestMemberSelectionIsValidatedAgainstEveryRegisteredTool(t *testing.T) {
	var offered atomic.Int32
	model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		offered.Store(int32(len(req.Tools)))
		return answer("done")
	})
	set := &independentToolset{}
	r := runtimeWithToolset(t, runtimeTest(t, model, 1, 8), set, func(c *Config) {
		c.PrepareMember = func(_ context.Context, _ sessions.Session, registry *tools.ToolRegistry) (string, error) {
			if registry != nil {
				registry.Register(&tools.Func{Name: "host_tool", Desc: "host", Run: func(context.Context, tools.Args) (string, error) { return "host", nil }})
			}
			return "", nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, selection := range [][]string{{"send_message"}, {"host_tool"}, {"read_transcript"}, {"read_*"}} {
		if _, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "run", ReadOnly: true, Tools: selection}); err != nil {
			t.Fatalf("selection %v refused: %v", selection, err)
		}
	}
	if _, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "run", ReadOnly: true, Tools: []string{"nope"}}); err == nil || !strings.Contains(err.Error(), `required tool "nope" cannot honor execution context`) {
		t.Fatalf("unknown selection = %v", err)
	}
	if _, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "run", ReadOnly: true, Tools: []string{}}); err != nil {
		t.Fatalf("disabled tools refused: %v", err)
	}
	if offered.Load() != 0 {
		t.Fatalf("a member with tools disabled was offered %d tools", offered.Load())
	}
	if _, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "typed", ReadOnly: true, Schema: boolResultSchema, Tools: []string{completionToolName}}); err != nil && strings.Contains(err.Error(), "cannot honor") {
		t.Fatalf("typed completion selection refused: %v", err)
	}
}

func TestIndependentEditsAreCapturedIntoSnapshots(t *testing.T) {
	var calls atomic.Int32
	model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return iterationTool("write", "write_root", `{"name":"made.txt","content":"made by an independent tool\n"}`)
		}
		return answer("done")
	})
	set := &independentToolset{}
	r := runtimeWithToolset(t, scratchRuntime(t, model, true), set, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "edit"}); err != nil {
		t.Fatal(err)
	}
	if directory := canonicalPath(t, r.config.Directory); len(set.wrote) != 1 || !strings.HasPrefix(set.wrote[0], directory) {
		t.Fatalf("wrote %v, want a file under the member's checkout in %s", set.wrote, directory)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	captured := false
	for _, snapshot := range s.Snapshots {
		cmd := exec.Command("git", "ls-tree", "-r", "--name-only", snapshot.Tree)
		cmd.Dir = r.config.Root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git ls-tree: %s %v", out, err)
		}
		if strings.Contains(string(out), "made.txt") {
			captured = true
		}
	}
	if !captured {
		t.Fatalf("no snapshot captured the independent edit: %+v", s.Snapshots)
	}
}
