package tools

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPResultOutputSeparatesTextStructuredAndBinaryMedia(t *testing.T) {
	image := []byte("image payload")
	audio := []byte("audio payload")
	embedded := []byte("embedded payload")
	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: "plain text"},
			&mcp.ImageContent{Data: image, MIMEType: "image/png"},
			&mcp.AudioContent{Data: audio, MIMEType: "audio/wav"},
			&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
				URI: "file:///result.bin", MIMEType: "application/octet-stream", Blob: embedded,
			}},
		},
		StructuredContent: map[string]any{"count": 3, "ok": true},
	}

	output, err := mcpResultOutput(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.Text, "plain text") || !strings.Contains(output.Text, `"count":3`) || !strings.Contains(output.Text, `"ok":true`) {
		t.Fatalf("text/structured output = %q", output.Text)
	}
	for _, payload := range [][]byte{image, audio, embedded} {
		if encoded := base64.StdEncoding.EncodeToString(payload); strings.Contains(output.Text, encoded) {
			t.Fatalf("binary base64 %q leaked into textual MCP output %q", encoded, output.Text)
		}
	}
	if len(output.Media) != 3 {
		t.Fatalf("typed media = %#v, want 3 entries", output.Media)
	}
	if string(output.Media[0].Data) != string(image) || output.Media[0].MIMEType != "image/png" {
		t.Fatalf("image media = %#v", output.Media[0])
	}
	if string(output.Media[1].Data) != string(audio) || output.Media[1].MIMEType != "audio/wav" {
		t.Fatalf("audio media = %#v", output.Media[1])
	}
	if string(output.Media[2].Data) != string(embedded) || output.Media[2].Reference != "file:///result.bin" {
		t.Fatalf("embedded media = %#v", output.Media[2])
	}
}

func TestMCPResultOutputRecursesNestedToolResultWithoutBinaryJSON(t *testing.T) {
	image := []byte("nested image")
	result := &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ToolResultContent{
			ToolUseID: "nested",
			Content: []mcp.Content{
				&mcp.TextContent{Text: "nested text"},
				&mcp.ImageContent{Data: image, MIMEType: "image/jpeg"},
			},
			StructuredContent: map[string]any{"nested": true},
		},
	}}

	output, err := mcpResultOutput(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.Text, "nested text") || !strings.Contains(output.Text, `"nested":true`) {
		t.Fatalf("nested output text = %q", output.Text)
	}
	if strings.Contains(output.Text, base64.StdEncoding.EncodeToString(image)) {
		t.Fatalf("nested image leaked into text: %q", output.Text)
	}
	if len(output.Media) != 1 || string(output.Media[0].Data) != string(image) {
		t.Fatalf("nested media = %#v", output.Media)
	}
}

func TestNamespacedToolPreservesRichOutputInterface(t *testing.T) {
	wrapped := &NamespacedTool{Tool: richOutputStub{}, namespacedName: "server__rich"}
	output, err := wrapped.ExecuteOutput(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if output.Text != "rich" || len(output.Media) != 1 || string(output.Media[0].Data) != "bytes" {
		t.Fatalf("namespaced rich output = %#v", output)
	}
}

func TestMCPErrorDescriptionDoesNotSerializeImageBase64(t *testing.T) {
	image := []byte("error image bytes")
	description, err := mcpErrorDescription(&mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{
			&mcp.TextContent{Text: "failed"},
			&mcp.ImageContent{Data: image, MIMEType: "image/png"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(description, "failed") || !strings.Contains(description, "image/png binary media") {
		t.Fatalf("safe MCP error description = %q", description)
	}
	if strings.Contains(description, base64.StdEncoding.EncodeToString(image)) {
		t.Fatalf("MCP error image leaked as base64: %q", description)
	}
}

type richOutputStub struct{ NativeTool }

func (richOutputStub) GetName() string { return "rich" }

func (richOutputStub) GetSchema() *schema.ToolSchema {
	return schema.Tool("rich", "rich test", nil)
}

func (richOutputStub) Execute(context.Context, map[string]any) (string, error) {
	return "legacy", nil
}

func (richOutputStub) ExecuteOutput(context.Context, map[string]any) (ToolOutput, error) {
	return ToolOutput{Text: "rich", Media: []ToolMedia{{Data: []byte("bytes"), MIMEType: "image/png"}}}, nil
}
