package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestApplyDeepSeekReasoningReplay(t *testing.T) {
	tests := []struct {
		name         string
		msgs         []messages.ChatMessage
		wantReplayed int
	}{
		{
			name:         "empty",
			msgs:         nil,
			wantReplayed: 0,
		},
		{
			name: "user message with reasoning is ignored",
			msgs: []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "hi", Reasoning: "should not be sent"},
			},
			wantReplayed: 0,
		},
		{
			name: "assistant message without reasoning is ignored",
			msgs: []messages.ChatMessage{
				{Role: messages.MessageRoleAssistant, Content: "hello"},
			},
			wantReplayed: 0,
		},
		{
			name: "single assistant message with reasoning is annotated",
			msgs: []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "hi"},
				{Role: messages.MessageRoleAssistant, Content: "hello", Reasoning: "user greeted me"},
			},
			wantReplayed: 1,
		},
		{
			name: "multiple assistant messages are each annotated",
			msgs: []messages.ChatMessage{
				{Role: messages.MessageRoleSystem, Content: "sys"},
				{Role: messages.MessageRoleUser, Content: "q1"},
				{Role: messages.MessageRoleAssistant, Content: "a1", Reasoning: "r1"},
				{Role: messages.MessageRoleUser, Content: "q2"},
				{Role: messages.MessageRoleAssistant, Content: "a2", Reasoning: "r2"},
			},
			wantReplayed: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := buildChatCompletionRequestParams(&CompletionRequest{
				Model:    "deepseek-reasoner",
				Messages: tc.msgs,
			})
			got := applyDeepSeekReasoningReplay(params, tc.msgs)
			if got != tc.wantReplayed {
				t.Fatalf("replayed %d messages, want %d", got, tc.wantReplayed)
			}
			for i, msg := range tc.msgs {
				want := ""
				if msg.Role == messages.MessageRoleAssistant {
					want = msg.Reasoning
				}
				if params.Messages[i].ReasoningContent != want {
					t.Fatalf("message %d reasoning_content = %q, want %q", i, params.Messages[i].ReasoningContent, want)
				}
			}
		})
	}
}

func TestChatDeltaReasoningContent(t *testing.T) {
	// DeepSeek streams a non-standard `reasoning_content` field in deltas;
	// the wire type must decode all the shapes servers produce.
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "absent",
			raw:  `{"role":"assistant","content":null}`,
			want: "",
		},
		{
			name: "non-empty reasoning_content",
			raw:  `{"content":null,"reasoning_content":"thinking out loud"}`,
			want: "thinking out loud",
		},
		{
			name: "escaped reasoning_content",
			raw:  `{"content":null,"reasoning_content":"line1\nline2"}`,
			want: "line1\nline2",
		},
		{
			name: "empty reasoning_content",
			raw:  `{"content":null,"reasoning_content":""}`,
			want: "",
		},
		{
			name: "null reasoning_content",
			raw:  `{"content":null,"reasoning_content":null}`,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var delta openai.ChatDelta
			if err := json.Unmarshal([]byte(tc.raw), &delta); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if delta.ReasoningContent != tc.want {
				t.Fatalf("got %q, want %q", delta.ReasoningContent, tc.want)
			}
		})
	}
}

// OpenAI-compatible gateways split on the field name: DeepSeek's own API sends
// reasoning_content, OpenRouter and most others send reasoning. Both must
// decode and resolve to the same text, or reasoning is silently dropped for
// whichever convention is missing.
func TestChatDeltaResolvesEitherReasoningField(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "openrouter reasoning field",
			raw:  `{"content":"","role":"assistant","reasoning":"First, the user asked"}`,
			want: "First, the user asked",
		},
		{
			// The structured twin carries the same text; ignoring it must not
			// stop the plain field from being read.
			name: "reasoning alongside reasoning_details",
			raw: `{"content":"","reasoning":"weighing it up",` +
				`"reasoning_details":[{"type":"reasoning.text","text":"weighing it up","index":0}]}`,
			want: "weighing it up",
		},
		{
			name: "deepseek reasoning_content still wins",
			raw:  `{"content":null,"reasoning_content":"native deepseek"}`,
			want: "native deepseek",
		},
		{
			name: "neither field",
			raw:  `{"content":"just an answer"}`,
			want: "",
		},
		{
			name: "null reasoning",
			raw:  `{"content":null,"reasoning":null}`,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var delta openai.ChatDelta
			if err := json.Unmarshal([]byte(tc.raw), &delta); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := delta.ReasoningText(); got != tc.want {
				t.Fatalf("delta.ReasoningText() = %q, want %q", got, tc.want)
			}

			// The non-streaming shape carries the same two fields.
			var msg openai.ChatResponseMessage
			if err := json.Unmarshal([]byte(tc.raw), &msg); err != nil {
				t.Fatalf("unmarshal message: %v", err)
			}
			if got := msg.ReasoningText(); got != tc.want {
				t.Fatalf("message.ReasoningText() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeepSeekStreamEmitsReasoningBeforeContent: when one delta carries both
// reasoning_content and content, reasoning must be emitted first — it
// precedes the answer it produced.
func TestDeepSeekStreamEmitsReasoningBeforeContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"why\",\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n"))
	}))
	t.Cleanup(server.Close)

	client := NewDeepSeekClient("test-key", server.URL)
	events := client.ChatCompletionStream(context.Background(), &CompletionRequest{
		Model:    "deepseek-reasoner",
		Messages: messages.User("hi"),
	}, messages.NewStreamProcessor())

	var order []messages.StreamEventType
	for event := range events {
		switch event.Type {
		case messages.EventTypeError:
			t.Fatalf("stream error: %v", event.Error)
		case messages.EventTypeReasoning, messages.EventTypeContent:
			order = append(order, event.Type)
		}
	}
	want := []messages.StreamEventType{messages.EventTypeReasoning, messages.EventTypeContent}
	if len(order) != 2 || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("emission order = %v, want %v", order, want)
	}
}
