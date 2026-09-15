package llm

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

// memFileTool reads from an in-memory tree: an independently implemented
// file tool that never touches the filesystem or the native file tools.
type memFileTool struct {
	files map[string]string
	calls *int
}

func (t *memFileTool) GetName() string   { return "read_mem" }
func (t *memFileTool) GetType() string   { return "memory" }
func (t *memFileTool) GetSource() string { return "independent" }
func (t *memFileTool) GetSchema() *schema.ToolSchema {
	return schema.Tool("read_mem", "Read a file from the in-memory tree", schema.Params{"path": schema.S("path")}, "path")
}
func (t *memFileTool) Execute(_ context.Context, args map[string]any) (string, error) {
	*t.calls++
	content, ok := t.files[tools.Args(args).String("path")]
	if !ok {
		return "", tools.NewToolError("no such file", "not_found")
	}
	return content, nil
}

// memImageTool is an independent view_image: it returns bytes it holds
// itself as media, so the agent's image path is exercised with no file or
// network access anywhere.
type memImageTool struct {
	data  []byte
	calls *int
}

func (t *memImageTool) GetName() string   { return "view_image" }
func (t *memImageTool) GetType() string   { return "memory" }
func (t *memImageTool) GetSource() string { return "independent" }
func (t *memImageTool) GetSchema() *schema.ToolSchema {
	return schema.Tool("view_image", "Attach an image", schema.Params{"source": schema.S("source")}, "source")
}
func (t *memImageTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	out, err := t.ExecuteOutput(ctx, args)
	return out.Text, err
}
func (t *memImageTool) ExecuteOutput(context.Context, map[string]any) (tools.ToolOutput, error) {
	*t.calls++
	return tools.ToolOutput{Text: "Attached image", Media: []tools.ToolMedia{{Data: t.data, MIMEType: "image/png", Name: "mem.png"}}}, nil
}

func independentPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.NRGBA{G: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// independentRegistry is a generic registry holding only the independent
// tools, with the image tool marked built-in the way native setup marks
// view_image.
func independentRegistry(t *testing.T, fileCalls, imageCalls *int) *tools.ToolRegistry {
	t.Helper()
	registry := tools.NewToolRegistry([]tools.Tool{
		&memFileTool{files: map[string]string{"notes.txt": "IN-MEMORY NOTES"}, calls: fileCalls},
		&memImageTool{data: independentPNG(t), calls: imageCalls},
	})
	registry.MarkBuiltin("view_image")
	t.Cleanup(func() { registry.Close() })
	return registry
}

func independentModel(calls *int, final string, batch ...messages.ChatMessageToolCall) ownershipLLM {
	return func(context.Context, *CompletionRequest) messages.ChatMessage {
		*calls++
		if *calls == 1 {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: batch}
		}
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: final, StopReason: messages.StopReasonEndTurn}
	}
}

func hasImagePart(msg messages.ChatMessage) bool {
	for _, part := range msg.Parts {
		if part.MimeType == "image/png" {
			return true
		}
	}
	return false
}

func toolResult(t *testing.T, msgs []messages.ChatMessage, id string) messages.ChatMessage {
	t.Helper()
	for _, msg := range msgs {
		if msg.Role == messages.MessageRoleTool && msg.ToolCallID == id {
			return msg
		}
	}
	t.Fatalf("no tool result for %s", id)
	return messages.ChatMessage{}
}

// TestAgentRunsIndependentToolsWithoutNativeSetup is the runtime-backends
// proof for the direct agent: file and image calls run through the shared
// registry and loop against tools implemented independently, while native
// construction, filesystem, process, and network access would all fail.
func TestAgentRunsIndependentToolsWithoutNativeSetup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var fileCalls, imageCalls, modelCalls int
	registry := independentRegistry(t, &fileCalls, &imageCalls)
	model := independentModel(&modelCalls, "described",
		messages.ChatMessageToolCall{ID: "file", Name: "read_mem", Arguments: `{"path":"notes.txt"}`},
		messages.ChatMessageToolCall{ID: "image", Name: "view_image", Arguments: `{"source":"anything"}`},
		messages.ChatMessageToolCall{ID: "shell", Name: "bash", Arguments: `{"command":"true"}`},
	)
	agent := NewAgent(model, registry, AgentConfig{MaxIterations: 3})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("go")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.GetContent() != "described" || fileCalls != 1 || imageCalls != 1 {
		t.Fatalf("reply %q, file calls %d, image calls %d", response.Message.GetContent(), fileCalls, imageCalls)
	}
	if got := toolResult(t, response.AllMessages, "file").Content; !strings.Contains(got, "IN-MEMORY NOTES") {
		t.Fatalf("read_mem result = %q", got)
	}
	if got := toolResult(t, response.AllMessages, "image"); !hasImagePart(got) {
		t.Fatalf("view_image result carries no image: %+v", got)
	}
	if got := toolResult(t, response.AllMessages, "shell").Content; !strings.Contains(got, "Tool not found: bash") {
		t.Fatalf("bash result = %q, want not found", got)
	}
	// Nothing native was constructed or derived along the way.
	for _, r := range []*tools.ToolRegistry{registry, agent.ToolRegistry()} {
		if r.HasNativeTool("bash") || r.HasNativeTool("read_file") || r.HasSandbox() {
			t.Fatal("native setup leaked into the independent toolset")
		}
	}
	viewer, ok := agent.ToolRegistry().Get("view_image")
	if !ok {
		t.Fatal("the agent lost the independent view_image")
	}
	if _, independent := viewer.(*memImageTool); !independent {
		t.Fatalf("the agent replaced view_image with %T", viewer)
	}
}

func TestAgentIndependentToolsWithPrivateHelpersAndDisabledTools(t *testing.T) {
	var fileCalls, imageCalls, modelCalls int
	registry := independentRegistry(t, &fileCalls, &imageCalls)
	store := newTestArtifactStore()
	model := independentModel(&modelCalls, "stored",
		messages.ChatMessageToolCall{ID: "image", Name: "view_image", Arguments: `{"source":"anything"}`},
	)
	agent := NewAgent(model, registry, AgentConfig{MaxIterations: 3, ArtifactStore: store})
	defer agent.Close()
	for _, name := range []string{"read_artifact", "list_artifacts", "read_transcript", "view_image", "read_mem"} {
		if _, ok := agent.ToolRegistry().Get(name); !ok {
			t.Fatalf("agent registry lacks %s", name)
		}
	}
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("go")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if imageCalls != 1 || len(store.blobs) == 0 {
		t.Fatalf("image calls %d, stored artifacts %d: the independent media did not reach the store", imageCalls, len(store.blobs))
	}
	got := toolResult(t, response.AllMessages, "image")
	stored := false
	for _, part := range got.Parts {
		if part.MimeType == "image/png" && part.Artifact != nil {
			stored = true
		}
	}
	if !stored {
		t.Fatalf("view_image result carries no stored image: %+v", got)
	}

	modelCalls = 0
	disabled := NewAgent(independentModel(&modelCalls, "never",
		messages.ChatMessageToolCall{ID: "file", Name: "read_mem", Arguments: `{"path":"notes.txt"}`},
	), registry, AgentConfig{MaxIterations: 3, DisableTools: true})
	defer disabled.Close()
	// DisableTools is an execution bound: the model is offered no tools and
	// a call it makes anyway is refused before any tool runs.
	if _, err := disabled.Run(context.Background(), &CompletionRequest{Messages: messages.User("go")}, nil); err == nil || !strings.Contains(err.Error(), "tool execution is disabled") || fileCalls != 0 {
		t.Fatalf("disabled agent ran tools: %v (file calls %d)", err, fileCalls)
	}
}
