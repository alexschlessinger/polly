package llm

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestCompactionAccountingMatchesRequestProjection(t *testing.T) {
	for _, kind := range []string{"unselected", "referenced", "inline base64", "working tool image", "duplicate artifact", "summary", "omitted images", "flattened tools", "flattened without images"} {
		t.Run(kind, func(t *testing.T) {
			store := newTestArtifactStore()
			ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Reference: "[image #1]", Data: []byte("png")})
			part := imageArtifactPart(ref)
			if kind == "inline base64" {
				part = messages.ContentPart{Type: "image_base64", ImageData: "cG5n", Reference: "[image #1]", MimeType: "image/png"}
			}
			history := []messages.ChatMessage{
				{Role: messages.MessageRoleSystem, Content: "rules"},
				{Role: messages.MessageRoleUser, Content: "old request", Parts: []messages.ContentPart{part}},
				reply("old answer", 0),
				{Role: messages.MessageRoleUser, Content: "continue"},
			}
			if kind != "unselected" {
				history[3].Content += " with [image #1]"
			}
			if kind == "duplicate artifact" {
				history[3].Parts = []messages.ContentPart{part}
			}
			if kind == "working tool image" || kind == "duplicate artifact" || kind == "summary" || strings.HasPrefix(kind, "flattened") {
				call := callTool("current", "fetch", 0)
				call.ToolCalls[0].Arguments = `{"text":"` + strings.Repeat("x", 1_000) + `"}`
				history = append(history, call, messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "current", ToolName: "fetch", Content: "result", Parts: []messages.ContentPart{part}})
			}
			if kind == "summary" {
				history = append(history, messages.Compaction{Summary: "current task"}.Message())
			}
			caps := &ModelCapabilities{}
			if kind == "omitted images" || kind == "flattened without images" {
				caps.InputModalities = []string{"text"}
			}
			if strings.HasPrefix(kind, "flattened") {
				no := false
				caps.Tools = &no
			}
			view := RequestView(history, nil)
			original := cloneMessages(view)
			req := &CompletionRequest{Model: "openai/model", Messages: view, Capabilities: caps}
			prepared, _, err := PrepareCapabilities(req, *caps, false)
			if err != nil {
				t.Fatal(err)
			}
			state := &runState{images: &imageCache{omit: omitsImages(*caps)}}
			actual, stats, err := projectCompletionRequest(context.Background(), prepared, store, state)
			if err != nil {
				t.Fatal(err)
			}
			accounted, err := projectedCompactionView(prepared, view)
			if err != nil {
				t.Fatal(err)
			}
			if accounted.total != stats.EstimatedTokens || len(accounted.messages) != len(actual) || projectedText(accounted.messages) != projectedText(actual) {
				t.Fatalf("accounting differs from actual projection: counted=%d actual=%d messages=%d/%d", accounted.total, stats.EstimatedTokens, len(accounted.messages), len(actual))
			}
			total := 0
			for _, tokens := range accounted.textTokens {
				total += tokens
			}
			for _, tokens := range accounted.images {
				total += tokens
			}
			if total != accounted.total || !reflect.DeepEqual(view, original) {
				t.Fatalf("attribution total=%d want=%d, or history was mutated", total, accounted.total)
			}
		})
	}
}
