package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestChildrenShareActiveSkillMCP(t *testing.T) {
	var starts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if len(request.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			starts.Add(1)
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "review", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "probe", "description": "probe", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "shared client is live"}}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer server.Close()
	root := t.TempDir()
	dir := filepath.Join(root, "review-skill")
	if err := os.MkdirAll(filepath.Join(dir, "mcp"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: review-skill\ndescription: Review fixture\n---\nUse the probe.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"probe": map[string]any{"url": server.URL, "transport": "streamable"}}})
	if err := os.WriteFile(filepath.Join(dir, "mcp", "probe.json"), config, 0644); err != nil {
		t.Fatal(err)
	}
	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	hits := 0
	client := &scriptedStreamLLM{}
	parent, _ := newSpawnTestParent(t, client, &hits)
	defer parent.toolRegistry.Close()
	parent.toolRegistry.Register(&tools.Func{Name: "bash", Desc: "unused fixture"})
	parent.skillCatalog = catalog
	parent.skillRuntime, err = tools.NewSkillRuntime(catalog, parent.toolRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parent.skillRuntime.Activate("review-skill"); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatalf("parent starts = %d", starts.Load())
	}
	for range 4 {
		child, err := openChildState(context.Background(), client, parent, subagent.Request{Task: "probe"})
		if err != nil {
			t.Fatal(err)
		}
		if got := child.skillRuntime.ActivatedSkills(); len(got) != 1 || got[0] != "review-skill" {
			t.Fatalf("child skills = %v", got)
		}
		if err := child.Close(); err != nil {
			t.Fatal(err)
		}
		// Closing a child cannot close the parent's shared MCP client.
		shared, _, ok := parent.toolRegistry.GetIfAllowed("review-skill-probe__probe")
		if !ok {
			t.Fatal("parent lost its shared tool")
		}
		if text, err := shared.Execute(context.Background(), nil); err != nil || !strings.Contains(text, "shared client is live") {
			t.Fatalf("parent tool after child close: %q, %v", text, err)
		}
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("MCP initializations = %d", got)
	}

}

type childCacheRoundTripper func(*http.Request) (*http.Response, error)

func (f childCacheRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestChildrenInheritContextWindowCaches(t *testing.T) {
	var requests atomic.Int32
	old := http.DefaultTransport
	http.DefaultTransport = childCacheRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"review-model","max_input_tokens":200000}`)), Request: r}, nil
	})
	defer func() { http.DefaultTransport = old }()
	hits := 0
	client := llm.NewMultiPass(nil)
	parent, _ := newSpawnTestParent(t, client, &hits)
	parent.settings.Model = "anthropic/review-model"
	parent.settings.MaxHistoryTokens = 256000
	parent.contextWindows = map[string]int{parent.settings.Model: 200000, "anthropic/unknown": 0}
	ctx := context.Background()
	md, err := parent.session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	md.ContextWindows = map[string]int{parent.settings.Model: 200000}
	if err := parent.session.SetMetadata(ctx, md); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		child, err := openChildState(ctx, client, parent, subagent.Request{Task: "look"})
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := child.contextWindows["anthropic/unknown"]; !ok || value != 0 {
			t.Fatal("failed discovery was not inherited")
		}
		child.contextWindows["anthropic/unknown"] = 1
		if parent.contextWindows["anthropic/unknown"] != 0 {
			t.Fatal("child cache aliases the parent")
		}
		child.contextWindows = nil // A later process relies on persisted metadata.
		if got := resolveContextBudget(ctx, child); got <= 0 {
			t.Fatal(got)
		}
		if err := child.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("requests = %d", got)
	}

}
