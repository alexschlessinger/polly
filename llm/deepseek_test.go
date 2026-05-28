package llm

import (
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	openai "github.com/openai/openai-go/v3"
)

func TestBuildDeepSeekReasoningReplayOptions(t *testing.T) {
	tests := []struct {
		name     string
		msgs     []messages.ChatMessage
		wantOpts int
	}{
		{
			name:     "empty",
			msgs:     nil,
			wantOpts: 0,
		},
		{
			name: "user message with reasoning is ignored",
			msgs: []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "hi", Reasoning: "should not be sent"},
			},
			wantOpts: 0,
		},
		{
			name: "assistant message without reasoning is ignored",
			msgs: []messages.ChatMessage{
				{Role: messages.MessageRoleAssistant, Content: "hello"},
			},
			wantOpts: 0,
		},
		{
			name: "single assistant message with reasoning produces one option",
			msgs: []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "hi"},
				{Role: messages.MessageRoleAssistant, Content: "hello", Reasoning: "user greeted me"},
			},
			wantOpts: 1,
		},
		{
			name: "multiple assistant messages produce one option each",
			msgs: []messages.ChatMessage{
				{Role: messages.MessageRoleSystem, Content: "sys"},
				{Role: messages.MessageRoleUser, Content: "q1"},
				{Role: messages.MessageRoleAssistant, Content: "a1", Reasoning: "r1"},
				{Role: messages.MessageRoleUser, Content: "q2"},
				{Role: messages.MessageRoleAssistant, Content: "a2", Reasoning: "r2"},
			},
			wantOpts: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := buildDeepSeekReasoningReplayOptions(tc.msgs)
			if len(opts) != tc.wantOpts {
				t.Fatalf("got %d options, want %d", len(opts), tc.wantOpts)
			}
		})
	}
}

func TestExtractReasoningContentDelta(t *testing.T) {
	// The openai-go SDK stuffs unknown fields like `reasoning_content` into the
	// per-chunk delta by unmarshaling the raw stream chunk. Decode a raw chunk
	// so the ExtraFields metadata is populated the same way it is in production.
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
			var delta openai.ChatCompletionChunkChoiceDelta
			if err := delta.UnmarshalJSON([]byte(tc.raw)); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := extractReasoningContentDelta(delta)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
