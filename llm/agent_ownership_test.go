package llm

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

type ownershipLLM func(context.Context, *CompletionRequest) messages.ChatMessage

func TestAgentSharedRegistryArtifactIsolationAndClose(t *testing.T) {
	registry := tools.NewToolRegistry(nil)
	defer registry.Close()
	storeA, storeB := newTestArtifactStore(), newTestArtifactStore()
	ref, err := storeA.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("PRIVATE_A_ARTIFACT")})
	if err != nil {
		t.Fatal(err)
	}
	a := NewAgent(nil, registry, AgentConfig{ArtifactStore: storeA})
	b := NewAgent(nil, registry, AgentConfig{ArtifactStore: storeB})
	defer a.Close()
	defer b.Close()
	a.indexArtifact(ref)
	readerA, _ := a.ToolRegistry().Get("read_artifact")
	readerB, _ := b.ToolRegistry().Get("read_artifact")
	if text, err := readerA.Execute(context.Background(), map[string]any{"id": ref.ID}); err != nil || !strings.Contains(text, "PRIVATE_A_ARTIFACT") {
		t.Fatalf("agent A artifact = %q, %v", text, err)
	}
	if _, err := readerB.Execute(context.Background(), map[string]any{"id": ref.ID}); err == nil {
		t.Fatal("agent B read another conversation's artifact")
	}
	// Even a known reference cannot expose bytes from another session's store.
	b.indexArtifact(ref)
	if _, err := readerB.Execute(context.Background(), map[string]any{"id": ref.ID}); err == nil {
		t.Fatal("agent B opened another session's artifact store")
	}
	for _, name := range []string{"read_artifact", "list_artifacts", "view_image", "read_transcript"} {
		if _, ok := registry.Get(name); ok {
			t.Errorf("private tool %s leaked into caller registry", name)
		}
	}
	// Tool additions remain visible to already-created agents.
	configured := &tools.Func{Name: "configured", Run: func(context.Context, tools.Args) (string, error) { return "ok", nil }}
	registry.Register(configured)
	if got, ok := a.ToolRegistry().Get("configured"); !ok || got != configured {
		t.Fatal("later tool registration is not inherited")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if got, ok := b.ToolRegistry().Get("configured"); !ok || got != configured {
		t.Fatal("closing agent A closed caller tools")
	}
	r, err := storeA.Open(context.Background(), ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
}

func TestAgentCommitsInheritedSkillActivation(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "restricted")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: restricted\ndescription: limited tools\nallowed-tools: allowed\n---\nUse allowed.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "allowed"}, &tools.Func{Name: "blocked"}}, tools.WithUnsafeNoSandbox())
	defer registry.Close()
	if _, err := tools.NewSkillRuntime(catalog, registry); err != nil {
		t.Fatal(err)
	}
	calls := 0
	names := map[string]bool{}
	model := ownershipLLM(func(_ context.Context, req *CompletionRequest) messages.ChatMessage {
		calls++
		if calls == 1 {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "activate", Name: "activate_skill", Arguments: `{"name":"restricted"}`}}}
		}
		for _, tool := range req.Tools {
			names[tool.GetName()] = true
		}
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"}
	})
	agent := NewAgent(model, registry, AgentConfig{})
	defer agent.Close()
	if _, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("activate restricted")}, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("requests = %d, want 2", calls)
	}
	if names["blocked"] || names["bash"] || !names["allowed"] || !names["read_transcript"] {
		t.Fatalf("skill policy was not applied to the next model request: %v", names)
	}
}

func (f ownershipLLM) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	input := make(chan messages.ChatMessage, 1)
	input <- f(ctx, req)
	close(input)
	return processor.ProcessMessagesToEvents(input)
}

func TestAgentSharedRegistryTranscriptIsolation(t *testing.T) {
	registry := tools.NewToolRegistry(nil)
	calls := 0
	client := ownershipLLM(func(ctx context.Context, _ *CompletionRequest) messages.ChatMessage {
		calls++
		if calls == 1 {
			// Run B while A is waiting for its first model response. A must
			// still read its own transcript when its tool call arrives.
			otherClient := ownershipLLM(func(context.Context, *CompletionRequest) messages.ChatMessage {
				return messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "noted", StopReason: messages.StopReasonEndTurn}
			})
			otherAgent := NewAgent(otherClient, registry, AgentConfig{})
			defer otherAgent.Close()
			if _, err := otherAgent.Run(ctx, &CompletionRequest{Messages: messages.User("PRIVATE_B_TRANSCRIPT")}, nil); err != nil {
				t.Fatal(err)
			}
			return messages.ChatMessage{
				Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
				ToolCalls: []messages.ChatMessageToolCall{{ID: "recall", Name: "read_transcript", Arguments: "{}"}},
			}
		}
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}
	})
	agent := NewAgent(client, registry, AgentConfig{})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("PRIVATE_A_TRANSCRIPT")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool && msg.ToolName == "read_transcript" {
			found = true
			if !strings.Contains(msg.Content, "PRIVATE_A_TRANSCRIPT") || strings.Contains(msg.Content, "PRIVATE_B_TRANSCRIPT") {
				t.Fatalf("agent A read the wrong transcript: %s", msg.Content)
			}
		}
	}
	if !found {
		t.Fatal("agent A did not receive its transcript")
	}
	for _, name := range []string{"read_transcript", "view_image"} {
		if _, ok := registry.Get(name); ok {
			t.Errorf("agent added %s to the shared registry", name)
		}
	}
}

func TestAgentSharedRegistryPreservesImageReadPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.png")
	if err := os.WriteFile(path, []byte("private file"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(sandbox.New, sandbox.Config{DenyPaths: []string{path}}))
	args, err := json.Marshal(map[string]string{"source": path})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := ownershipLLM(func(context.Context, *CompletionRequest) messages.ChatMessage {
		calls++
		if calls == 1 {
			return messages.ChatMessage{
				Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
				ToolCalls: []messages.ChatMessageToolCall{{ID: "image", Name: "view_image", Arguments: string(args)}},
			}
		}
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}
	})
	agent := NewAgent(client, registry, AgentConfig{})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("view the image")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool && msg.ToolName == "view_image" {
			if !strings.Contains(msg.Content, "blocked from reads by the sandbox policy") {
				t.Fatalf("view_image did not enforce the read policy: %s", msg.Content)
			}
			return
		}
	}
	t.Fatal("agent did not execute view_image")
}

func TestAgentCloseLeavesSharedMCPConnectionAlive(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "ownership-test", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	path := filepath.Join(t.TempDir(), "mcp.json")
	data, err := json.Marshal(tools.MCPServersConfig{MCPServers: map[string]tools.MCPConfig{"shared": {Transport: "streamable", URL: httpServer.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewToolRegistry(nil)
	defer registry.Close()
	if _, err := registry.LoadMCPServer(path); err != nil {
		t.Fatal(err)
	}
	a, b := NewAgent(nil, registry, AgentConfig{}), NewAgent(nil, registry, AgentConfig{})
	defer b.Close()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	tool, ok := b.ToolRegistry().Get("shared__ping")
	if !ok {
		t.Fatalf("shared MCP tool disappeared: %v", b.ToolRegistry().All())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if reply, err := tool.Execute(ctx, map[string]any{}); err != nil || !strings.Contains(reply, "pong") {
		t.Fatalf("MCP call after sibling close = %q, %v", reply, err)
	}
}

func TestBuiltinToolNamesMatchTheAgentRegistry(t *testing.T) {
	registry := tools.NewToolRegistry(nil)
	defer registry.Close()
	agent := NewAgent(nil, registry, AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	var got []string
	for _, tool := range agent.ToolRegistry().All() {
		got = append(got, tool.GetName())
	}
	slices.Sort(got)
	if want := BuiltinToolNames(); !slices.Equal(got, want) {
		t.Fatalf("agent built-ins = %v, BuiltinToolNames() = %v", got, want)
	}
}
