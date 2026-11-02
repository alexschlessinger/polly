package llm

import (
	"context"
	"encoding/base64"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestCoveringSummaryRetainsReferencedHistoricalImages(t *testing.T) {
	for _, kind := range []string{"image_url", "image_base64", "image_artifact"} {
		t.Run(kind, func(t *testing.T) {
			store := newTestArtifactStore()
			token := fmt.Sprintf("[image #%d]", 1)
			part := messages.ContentPart{Type: kind, Reference: token, FileName: "layout.png"}
			references := []string{token, part.FileName}
			switch kind {
			case "image_url":
				part.ImageURL = "https://example.org/layout.png"
			case "image_base64":
				part.ImageData, part.MimeType = base64.StdEncoding.EncodeToString([]byte("layout")), "image/png"
			case "image_artifact":
				ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: part.FileName, Reference: token, Data: []byte("layout")})
				part = imageArtifactPart(ref)
				references = append(references, ref.ID)
			}
			for _, reference := range references {
				history := []messages.ChatMessage{
					{Role: messages.MessageRoleUser, Content: "earlier attachment", Parts: []messages.ContentPart{part}},
					reply("seen", 0),
					{Role: messages.MessageRoleUser, Content: "unrelated attachment", Parts: []messages.ContentPart{{Type: "image_url", ImageURL: "https://example.org/unused.png", Reference: fmt.Sprintf("[image #%d]", 2)}}},
					reply("seen", 0),
					{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "fix " + reference + strings.Repeat(" layout details", 3_000)}}},
					callTool("c1", "read_file", 0), resultOf("c1", "read_file", "css"),
				}
				original := cloneMessages(history)
				before, stats, err := projectMessages(context.Background(), history, store, true)
				if err != nil || stats.HydratedImages != 1 {
					t.Fatalf("before summary (%s): images=%d error=%v", reference, stats.HydratedImages, err)
				}
				for n := 0; n < 2; n++ {
					history = append(history, messages.Compaction{Summary: "the user wants a layout fix"}.Message())
					projected, stats, err := projectMessages(context.Background(), history, store, true)
					if err != nil || stats.HydratedImages != 1 || !reflect.DeepEqual(projectedImageParts(projected), projectedImageParts(before)) {
						t.Fatalf("summary %d (%s): images=%d error=%v", n+1, reference, stats.HydratedImages, err)
					}
				}
				if !reflect.DeepEqual(original, history[:len(original)]) {
					t.Fatal("projection changed durable history")
				}
				finished := append(slices.Clone(history), reply("done", 0), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "thanks"})
				_, stats, err = projectMessages(context.Background(), finished, store, true)
				if err != nil || stats.HydratedImages != 0 {
					t.Fatalf("unreferenced images retained in next turn: images=%d error=%v", stats.HydratedImages, err)
				}
				finished = append(finished, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "compare " + token + " and " + fmt.Sprintf("[image #%d]", 2)})
				_, stats, err = projectMessages(context.Background(), finished, store, true)
				if err != nil || stats.HydratedImages != 2 {
					t.Fatalf("summarized images no longer referable: images=%d error=%v", stats.HydratedImages, err)
				}
			}
		})
	}
}

func TestSummaryNeverPromotesInternalImages(t *testing.T) {
	token := fmt.Sprintf("[image #%d]", 99)
	for _, keepsTurn := range []bool{false, true} {
		history := []messages.ChatMessage{
			{Role: messages.MessageRoleInternal, Parts: []messages.ContentPart{{Type: "image_url", ImageURL: "https://private.example/secret.png", Reference: token}}},
			{Role: messages.MessageRoleUser, Content: "continue"},
			callTool("c1", "read_file", 0), resultOf("c1", "read_file", "output"),
			messages.Compaction{Summary: "public summary", KeepsTurn: keepsTurn}.Message(),
		}
		if parts := projectedImageParts(RequestView(history, nil)); len(parts) != 0 {
			t.Fatalf("keepsTurn=%v: internal image promoted into summary: %+v", keepsTurn, parts)
		}
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "show " + token})
		_, _, err := projectMessages(context.Background(), history, nil, true)
		if err == nil || !strings.Contains(err.Error(), "not available") {
			t.Fatalf("keepsTurn=%v: private image became referable: %v", keepsTurn, err)
		}
	}
}

func TestAgentCoveringSummaryKeepsHistoricalWorkingImage(t *testing.T) {
	token := fmt.Sprintf("[image #%d]", 1)
	summaries, continuations := 0, 0
	model := &compactionLLM{reply: func(req *CompletionRequest, _ int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			summaries++
			return reply("fix the historical screenshot", 0), nil
		}
		continuations++
		if images := projectedImageParts(req.Messages); len(images) != 1 || images[0].ImageURL != "https://example.org/layout.png" {
			t.Errorf("continuation lost selected image: %+v", images)
		}
		return reply("done", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{DisableTools: true})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "attachment", Parts: []messages.ContentPart{{Type: "image_url", ImageURL: "https://example.org/layout.png", Reference: token}}},
		reply("seen", 0),
		{Role: messages.MessageRoleUser, Content: "fix " + token + strings.Repeat(" detailed requirements", 2_000)},
		callTool("c1", "read_file", 0), resultOf("c1", "read_file", "css"),
	}
	response, err := agent.Run(context.Background(), &CompletionRequest{Model: "openai/model", Messages: history, MaxContextTokens: 10_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summaries != 1 || continuations != 1 || response.Projection.HydratedImages != 1 {
		t.Fatalf("summaries=%d continuations=%d projection=%+v", summaries, continuations, response.Projection)
	}
	for _, msg := range response.AllMessages {
		if c, ok := msg.Compaction(); ok && c.Summary != "" && c.KeepsTurn {
			t.Fatal("fixture did not cover the current turn")
		}
	}
}
