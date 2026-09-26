package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/messages"
)

// TestReasoningSummaryPartsAreParagraphs pins that a summary's parts, which
// the API streams as separate texts, reach the reply separated by blank
// lines rather than run together, over the stream and the whole response
// alike, with nothing trailing the last part.
func TestReasoningSummaryPartsAreParagraphs(t *testing.T) {
	final := `{"id":"resp_1","status":"completed","output":[` +
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"**Drafting spec**"},{"type":"summary_text","text":"**Setting up skeleton**"}]},` +
		`{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"**Writing files**"}]},` +
		`{"type":"message","id":"msg_1","status":"completed","content":[{"type":"output_text","text":"done"}]}],` +
		`"usage":{"input_tokens":3,"output_tokens":2}}`
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"**Drafting"}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":" spec**"}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"**Drafting spec**"}`,
		`{"type":"response.reasoning_summary_part.done","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"**Drafting spec**"}}`,
		`{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":1,"part":{"type":"summary_text","text":""}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":1,"delta":"**Setting up skeleton**"}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":1,"text":"**Setting up skeleton**"}`,
		`{"type":"response.reasoning_summary_part.done","output_index":0,"summary_index":1,"part":{"type":"summary_text","text":"**Setting up skeleton**"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"rs_2","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":1,"summary_index":0,"delta":"**Writing files**"}`,
		`{"type":"response.reasoning_summary_text.done","output_index":1,"summary_index":0,"text":"**Writing files**"}`,
		`{"type":"response.output_text.delta","output_index":2,"delta":"done"}`,
		`{"type":"response.completed","response":` + final + `}`,
	}
	want := "**Drafting spec**\n\n**Setting up skeleton**\n\n**Writing files**"
	for _, mode := range []contract.StreamMode{contract.Streaming, contract.Buffered} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mode == contract.Buffered {
				fmt.Fprint(w, final)
				return
			}
			for _, event := range events {
				fmt.Fprintf(w, "data: %s\n\n", event)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		t.Cleanup(server.Close)
		p := NewResponsesProvider("key", server.URL)
		reply, err := contract.Complete(context.Background(), p, &contract.CompletionRequest{Model: "gpt-5", StreamMode: mode, Capabilities: &contract.ModelCapabilities{}, Messages: messages.User("go")})
		if err != nil {
			t.Fatalf("mode %v: %v", mode, err)
		}
		if reply.Reasoning != want || reply.Content != "done" {
			t.Fatalf("mode %v: reasoning = %q, content = %q", mode, reply.Reasoning, reply.Content)
		}
		if strings.HasSuffix(reply.Reasoning, "\n") {
			t.Fatalf("mode %v: trailing break in %q", mode, reply.Reasoning)
		}
	}
}
