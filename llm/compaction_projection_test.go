package llm

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestCompactionCalibratesOnlyPreparedProjectedInput(t *testing.T) {
	for _, kind := range []string{"plain", "unselected URL", "unselected base64", "unselected artifact", "text-only image", "flattened tools"} {
		for _, manual := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/automatic", true: "/manual"}[manual], func(t *testing.T) {
				model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
					if call == 0 {
						n := 0
						for _, msg := range req.Messages {
							n += EstimateMessageTokens(msg)
							if slices.ContainsFunc(msg.Parts, isImagePart) {
								t.Error("the measured request contains an unselected image")
							}
						}
						if kind != "text-only image" && kind != "flattened tools" && n != 3_021 {
							t.Errorf("initial projected estimate = %d, want 3021", n)
						}
						return reply("answer", 2*n), nil
					}
					if !manual || !isSummaryRequest(req) {
						t.Error("an automatic request with oversized retained system overhead reached the provider")
					}
					return reply("a short summary", 100), nil
				}}
				agent := NewAgent(model, nil, AgentConfig{DisableTools: true})
				defer agent.Close()
				old := messages.User(strings.Repeat("x", 8_000))[0]
				var caps *ModelCapabilities
				switch kind {
				case "unselected URL", "text-only image":
					old.Parts = []messages.ContentPart{{Type: "image_url", ImageURL: "https://example.org/old.png"}}
					if kind == "text-only image" {
						caps = &ModelCapabilities{InputModalities: []string{"text"}}
					}
				case "unselected base64":
					old.Parts = []messages.ContentPart{{Type: "image_base64", ImageData: "cG5n", MimeType: "image/png"}}
				case "unselected artifact":
					old.Parts = []messages.ContentPart{imageArtifactPart(artifacts.Ref{ID: "sha256:" + strings.Repeat("a", 64), Kind: artifacts.KindImage, MIMEType: "image/png", Bytes: 3})}
				}
				history := []messages.ChatMessage{
					{Role: messages.MessageRoleSystem, Content: strings.Repeat("x", 4_000)}, old, reply("old answer", 0),
				}
				if kind == "flattened tools" {
					no := false
					caps = &ModelCapabilities{Tools: &no}
					call := callTool("old", "fetch", 0)
					call.ToolCalls[0].Arguments = `{"text":"` + strings.Repeat("x", 10_000) + `"}`
					history = append(history, call, resultOf("old", "fetch", "ok"))
				}
				history = append(history, messages.User("continue")...)
				req := &CompletionRequest{Model: "openai/model", MaxContextTokens: 20_000, Messages: history, Capabilities: caps}
				first, err := agent.Run(context.Background(), req, nil)
				if err != nil {
					t.Fatal(err)
				}
				history = append(history, first.AllMessages...)
				encoded, err := json.Marshal(history)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(encoded, &history); err != nil {
					t.Fatal(err)
				}
				original := cloneMessages(history)
				req.Messages, req.MaxContextTokens = history, 1_600
				var response *AgentResponse
				if manual {
					response, err = agent.Compact(context.Background(), req, nil)
				} else {
					req.Messages = append(slices.Clone(history), messages.User("continue")...)
					response, err = agent.Run(context.Background(), req, nil)
				}
				var limit *ContextLimitError
				if !errors.As(err, &limit) || limit.EstimatedTokens <= limit.Limit {
					t.Fatalf("lost retained system calibration: error=%v projection=%+v", err, response.Projection)
				}
				wantCalls := 1
				if manual {
					wantCalls = 2
					assertCompactNoMarker(t, response)
					if kind != "text-only image" && limit.EstimatedTokens != 2_035 {
						t.Fatalf("compacted size = %d, want 2035 including measured system overhead", limit.EstimatedTokens)
					}
				}
				if len(model.requests) != wantCalls || !reflect.DeepEqual(history, original) {
					t.Fatalf("calls=%d, want %d, or durable history changed", len(model.requests), wantCalls)
				}
			})
		}
	}
}

func TestCompactionProjectsAdmittedImageSelection(t *testing.T) {
	for _, referenced := range []bool{false, true} {
		t.Run(map[bool]string{false: "dropped image", true: "retained image"}[referenced], func(t *testing.T) {
			model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
				if call == 0 {
					n := 0
					for _, msg := range req.Messages {
						n += EstimateMessageTokens(msg)
					}
					return reply("answer", 2*n), nil
				}
				if referenced {
					t.Error("an oversized admitted request reached the provider")
				}
				if !isSummaryRequest(req) && len(projectedImageParts(req.Messages)) != 0 {
					t.Error("the continuation retained an unselected image")
				}
				return reply("a short summary", 100), nil
			}}
			agent := NewAgent(model, nil, AgentConfig{DisableTools: true})
			defer agent.Close()
			req := &CompletionRequest{Model: "openai/model", MaxContextTokens: 20_000, Messages: []messages.ChatMessage{
				{Role: messages.MessageRoleSystem, Content: strings.Repeat("x", 4_000)},
				{Role: messages.MessageRoleUser, Content: strings.Repeat("x", 8_000)}, reply("old answer", 0),
				{Role: messages.MessageRoleUser, Content: "look", Parts: []messages.ContentPart{{Type: "image_url", Reference: "[image #1]", ImageURL: "https://example.org/current.png"}}},
			}}
			first, err := agent.Run(context.Background(), req, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Messages = append(req.Messages, first.AllMessages...)
			req.MaxContextTokens = 3_000
			prompt := "continue"
			if referenced {
				prompt += " with [image #1]"
			}
			response, err := agent.Run(context.Background(), req, &AgentCallbacks{
				AdmitInput: func(context.Context) ([]messages.ChatMessage, error) { return messages.User(prompt), nil },
			})
			if referenced {
				var limit *ContextLimitError
				if !errors.As(err, &limit) || len(model.requests) != 1 {
					t.Fatalf("retained image: error=%v calls=%d", err, len(model.requests))
				}
			} else if err != nil || len(model.requests) != 3 || response.Projection.CountedTokens > req.MaxContextTokens {
				t.Fatalf("dropped image: error=%v calls=%d projection=%+v", err, len(model.requests), response.Projection)
			}
		})
	}
}

func TestCompactionProjectionFailureDoesNotGrantRoom(t *testing.T) {
	for _, reported := range []int{500, 2_000} {
		history := messages.User("look at [image #1]") // its referenced image was lost from durable history
		response := reply("answer", reported)
		response.SetRequestEstimate(1_000)
		history = append(history, response, messages.Compaction{Summary: "summary"}.Message())
		counted, ok := countedTokens(history, &CompletionRequest{}, 100)
		want := 100 + max(0, reported-1_000)
		if counted != want || ok != (reported > 1_000) {
			t.Fatalf("input %d: counted=%d, %v; want conservative %d", reported, counted, ok, want)
		}
	}
}

func TestCompactionRetainsMeasuredWorkingImages(t *testing.T) {
	for _, kind := range []string{"image_url", "image_base64", "image_artifact"} {
		for _, manual := range []bool{false, true} {
			for _, budget := range []int{4_500, 7_000} {
				t.Run(kind+map[bool]string{false: "/automatic/", true: "/manual/"}[manual]+groupDigits(budget), func(t *testing.T) {
					store := newTestArtifactStore()
					part := messages.ContentPart{Type: kind, Reference: "[image #1]"}
					switch kind {
					case "image_url":
						part.ImageURL = "https://example.org/current.png"
					case "image_base64":
						part.ImageData, part.MimeType = "cG5n", "image/png"
					case "image_artifact":
						ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Reference: "[image #1]", Data: []byte("png")})
						part = imageArtifactPart(ref)
					}
					model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
						if call == 0 {
							n := 0
							for _, msg := range req.Messages {
								n += EstimateMessageTokens(msg)
							}
							if len(projectedImageParts(req.Messages)) != 1 {
								t.Error("the fixture did not measure its working image")
							}
							return reply("answer", 2*n), nil
						}
						if budget == 4_500 && (!manual || !isSummaryRequest(req)) {
							t.Error("an oversized continuation lost its retained image calibration")
						}
						if !isSummaryRequest(req) && len(projectedImageParts(req.Messages)) != 1 {
							t.Error("the continuation lost its working image")
						}
						return reply("a short summary", 100), nil
					}}
					agent := NewAgent(model, nil, AgentConfig{DisableTools: true, ArtifactStore: store})
					defer agent.Close()
					req := &CompletionRequest{Model: "openai/model", MaxContextTokens: 20_000, Messages: []messages.ChatMessage{
						{Role: messages.MessageRoleSystem, Content: strings.Repeat("x", 4_000)},
						{Role: messages.MessageRoleUser, Content: strings.Repeat("x", 8_000)}, reply("old answer", 0),
						{Role: messages.MessageRoleUser, Content: "look at [image #1]", Parts: []messages.ContentPart{part}},
					}}
					first, err := agent.Run(context.Background(), req, nil)
					if err != nil {
						t.Fatal(err)
					}
					req.Messages = append(req.Messages, first.AllMessages...)
					req.MaxContextTokens = budget
					var response *AgentResponse
					if manual {
						response, err = agent.Compact(context.Background(), req, nil)
					} else {
						req.Messages = append(req.Messages, messages.User("continue with [image #1]")...)
						response, err = agent.Run(context.Background(), req, nil)
					}
					if budget == 4_500 {
						var limit *ContextLimitError
						if !errors.As(err, &limit) || limit.EstimatedTokens <= req.MaxContextTokens {
							t.Fatalf("working image lost calibration: error=%v projection=%+v", err, response.Projection)
						}
						if manual {
							assertCompactNoMarker(t, response)
						}
					} else if stats := response.Projection; err != nil || !stats.Counted || stats.CountedTokens < 6_000 || stats.CountedTokens > budget {
						t.Fatalf("fitting working image: error=%v projection=%+v", err, stats)
					}
				})
			}
		}
	}
}
