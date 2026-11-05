package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/messages"
)

type reasoningStage struct {
	events []string
	want   string
}

// Each stage blocks the server until its expected reasoning has reached the
// consumer. A provider that buffers deltas until completion deadlocks at the
// first stage and fails the timeout, rather than passing on the final message.
func gatedReasoningStream(t *testing.T, stages []reasoningStage, rest []string, want string) (*messages.ChatMessage, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	advance := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, stage := range stages {
			for _, event := range stage.events {
				fmt.Fprintf(w, "data: %s\n\n", event)
			}
			w.(http.Flusher).Flush()
			select {
			case <-advance:
			case <-r.Context().Done():
				return
			}
		}
		for _, event := range rest {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	p := NewResponsesProvider("key", server.URL)
	events := p.ChatCompletionStream(ctx, &contract.CompletionRequest{Model: "model", Messages: messages.User("go")}, messages.NewStreamProcessor())
	var got string
	for i, stage := range stages {
		for got != stage.want {
			select {
			case event, ok := <-events:
				if !ok {
					t.Fatalf("stage %d: stream closed before reasoning %q (got %q)", i, stage.want, got)
				}
				switch event.Type {
				case messages.EventTypeReasoning:
					got += event.Content
					if !strings.HasPrefix(stage.want, got) {
						t.Fatalf("stage %d: reasoning %q is not a prefix of %q", i, got, stage.want)
					}
				case messages.EventTypeComplete, messages.EventTypeError:
					t.Fatalf("stage %d: terminal event arrived before gate released: %+v", i, event)
				}
			case <-ctx.Done():
				t.Fatalf("stage %d: reasoning not observable before completion: got %q, want %q", i, got, stage.want)
			}
		}
		select {
		case advance <- struct{}{}:
		case <-ctx.Done():
			t.Fatalf("stage %d: server did not reach gate", i)
		}
	}
	var reply *messages.ChatMessage
	var streamErr error
	for event := range events {
		switch event.Type {
		case messages.EventTypeReasoning:
			got += event.Content
		case messages.EventTypeComplete:
			reply = event.Message
		case messages.EventTypeError:
			streamErr = event.Error
		}
	}
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	if got != want {
		t.Fatalf("streamed reasoning = %q, want %q", got, want)
	}
	if reply != nil && reply.Reasoning != want {
		t.Fatalf("final reasoning = %q, want %q", reply.Reasoning, want)
	}
	return reply, streamErr
}

func TestResponsesReasoningStreamsImmediately(t *testing.T) {
	for _, form := range []string{"reasoning_summary_text", "reasoning_text"} {
		t.Run(form, func(t *testing.T) {
			reply, err := gatedReasoningStream(t, []reasoningStage{
				{[]string{fmt.Sprintf(`{"type":"response.%s.delta","output_index":0,"delta":"think"}`, form)}, "think"},
				{[]string{fmt.Sprintf(`{"type":"response.%s.delta","output_index":0,"delta":"ing"}`, form)}, "thinking"},
			}, []string{`{"type":"response.completed","response":{"status":"completed"}}`}, "thinking")
			if err != nil || reply == nil {
				t.Fatalf("reply = %+v, error = %v", reply, err)
			}
		})
	}
}

func TestResponsesReasoningSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
	}{
		{"summary text done", `{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"thinking"}`},
		{"summary part done", `{"type":"response.reasoning_summary_part.done","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"thinking"}}`},
		{"raw text done", `{"type":"response.reasoning_text.done","output_index":0,"content_index":0,"text":"thinking"}`},
		{"raw part done", `{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"reasoning_text","text":"thinking"}}`},
		{"item done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"opaque"}}`},
		{"terminal only", `{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"opaque"}]}}`},
		{"terminal raw empty summary", `{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":""}],"content":[{"type":"reasoning_text","text":"thinking"}],"encrypted_content":"opaque"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Even a done-only or terminal-only snapshot is delivered without
			// waiting for the server to close the response body.
			var rest []string
			if !strings.HasPrefix(tc.name, "terminal") {
				rest = []string{`{"type":"response.completed","response":{"status":"completed"}}`}
			}
			reply, err := gatedReasoningStream(t, []reasoningStage{{[]string{tc.event}, "thinking"}}, rest, "thinking")
			if err != nil || reply == nil {
				t.Fatalf("reply = %+v, error = %v", reply, err)
			}
			if strings.Contains(tc.event, `"encrypted_content"`) {
				items := ReplayReasoningItems(*reply, "model")
				if len(items) != 1 || items[0].EncryptedContent != "opaque" {
					t.Fatalf("opaque replay = %+v", items)
				}
				if switched := ReplayReasoningItems(*reply, "another-model"); len(switched) != 0 {
					t.Fatalf("replayed encrypted state on model switch: %+v", switched)
				}
			}
		})
	}
}

func TestResponsesReasoningSnapshotSuffixes(t *testing.T) {
	reply, err := gatedReasoningStream(t, []reasoningStage{
		{[]string{`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_1","delta":"A"}`}, "A"},
		{[]string{`{"type":"response.reasoning_summary_text.done","output_index":0,"item_id":"rs_1","text":"AB"}`}, "AB"},
		{[]string{`{"type":"response.reasoning_summary_part.done","output_index":0,"part":{"type":"summary_text","text":"ABC"}}`}, "ABC"},
		{[]string{`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"ABCD"}],"encrypted_content":"opaque"}}`}, "ABCD"},
		{[]string{`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"ABCDE"}],"encrypted_content":"opaque"}]}}`}, "ABCDE"},
	}, nil, "ABCDE")
	if err != nil || reply == nil {
		t.Fatalf("reply = %+v, error = %v", reply, err)
	}
	items := ReplayReasoningItems(*reply, "model")
	if len(items) != 1 || items[0].Summary == nil || (*items[0].Summary)[0].Text != "ABCDE" {
		t.Fatalf("final replay snapshot = %+v", items)
	}
}

func TestResponsesReasoningPerItemRepresentationAndParts(t *testing.T) {
	const final = `{"type":"response.completed","response":{"status":"completed","output":[` +
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"alternate summary"}],"content":[{"type":"reasoning_text","text":"raw one"},{"type":"reasoning_text","text":"raw two"}]},` +
		`{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"summary one"},{"type":"summary_text","text":""},{"type":"summary_text","text":"summary two"}],"content":[{"type":"reasoning_text","text":"alternate raw"}]}]}}`
	want := "raw one\n\nraw two\n\nsummary one\n\nsummary two"
	reply, err := gatedReasoningStream(t, []reasoningStage{
		{[]string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
			`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":""}`,
			`{"type":"response.reasoning_text.delta","output_index":0,"content_index":0,"delta":"raw one"}`,
		}, "raw one"},
		{[]string{
			`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"alternate summary"}`,
			`{"type":"response.reasoning_text.done","output_index":0,"content_index":0,"text":"raw one"}`,
			`{"type":"response.reasoning_text.delta","output_index":0,"content_index":1,"delta":"raw two"}`,
		}, "raw one\n\nraw two"},
		{[]string{
			`{"type":"response.reasoning_summary_text.delta","output_index":1,"item_id":"rs_2","summary_index":0,"delta":"summary one"}`,
		}, "raw one\n\nraw two\n\nsummary one"},
		{[]string{
			`{"type":"response.reasoning_text.delta","output_index":1,"delta":"alternate raw"}`,
			`{"type":"response.reasoning_summary_part.done","output_index":1,"summary_index":1,"part":{"type":"summary_text","text":""}}`,
			`{"type":"response.reasoning_summary_text.delta","output_index":1,"summary_index":2,"delta":"summary two"}`,
		}, want},
	}, []string{final}, want)
	if err != nil || reply == nil {
		t.Fatalf("reply = %+v, error = %v", reply, err)
	}
}

func TestResponsesReasoningRawSnapshotIdentityAndMismatch(t *testing.T) {
	reply, err := gatedReasoningStream(t, []reasoningStage{
		{[]string{`{"type":"response.reasoning_text.delta","output_index":4,"item_id":"rs_1","delta":"raw"}`}, "raw"},
		{[]string{`{"type":"response.reasoning_text.done","output_index":4,"item_id":"rs_1","text":"raw text"}`}, "raw text"},
	}, []string{
		`{"type":"response.reasoning_text.done","output_index":4,"text":"raw"}`,
		`{"type":"response.content_part.done","output_index":4,"part":{"type":"reasoning_text","text":"incompatible rewrite"}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"raw text!"}]}]}}`,
	}, "raw text!")
	if err != nil || reply == nil {
		t.Fatalf("reply = %+v, error = %v", reply, err)
	}
}

func TestResponsesReasoningLateSuffixKeepsPartBoundary(t *testing.T) {
	reply, err := gatedReasoningStream(t, []reasoningStage{
		{[]string{`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_1","delta":"first"}`}, "first"},
		{[]string{`{"type":"response.reasoning_summary_text.delta","output_index":1,"item_id":"rs_2","delta":"second"}`}, "first\n\nsecond"},
	}, []string{
		`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"first!"}]},{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"second"}]}]}}`,
	}, "first\n\nsecond\n\n!")
	if err != nil || reply == nil {
		t.Fatalf("reply = %+v, error = %v", reply, err)
	}
}

func TestResponsesReasoningPartialStreams(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rest    []string
		want    string
		wantErr string
		stop    messages.StopReason
	}{
		{"incomplete", []string{`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"partial more"}]}]}}`}, "partial more", "", messages.StopReasonMaxTokens},
		{"failed snapshot", []string{`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"failed"},"output":[{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"partial more"}]}]}}`}, "partial more", "failed", ""},
		{"error event", []string{`{"type":"error","code":"server_error","message":"failed"}`}, "partial", "failed", ""},
		{"truncated", nil, "partial", "stream ended", ""},
		{"invalid event", []string{`{"type":`}, "partial", "invalid stream event", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := gatedReasoningStream(t, []reasoningStage{{[]string{
				`{"type":"response.reasoning_text.delta","output_index":0,"item_id":"rs_1","delta":"partial"}`,
			}, "partial"}}, tc.rest, tc.want)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || reply == nil || reply.StopReason != tc.stop {
				t.Fatalf("reply = %+v, error = %v, want stop %q", reply, err, tc.stop)
			}
		})
	}
}

func TestCompleteResponsesEmptySummaryUsesRawReasoning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":""}],"content":[{"type":"reasoning_text","text":"one"},{"type":"reasoning_text","text":"two"}]}]}`)
	}))
	defer server.Close()
	reply, err := contract.Complete(context.Background(), NewResponsesProvider("key", server.URL), &contract.CompletionRequest{Model: "model", StreamMode: contract.Buffered})
	if err != nil || reply == nil || reply.Reasoning != "one\n\ntwo" {
		t.Fatalf("reply = %+v, error = %v", reply, err)
	}
}
