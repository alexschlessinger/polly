package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// Captured from the Codex backend: two similar paragraphs deliberately belong
// to different phases. The fixture contains no credentials or reasoning state.
func TestResponsesPhasesSurviveStreamStoreAndReplay(t *testing.T) {
	wire, err := os.ReadFile("testdata/commentary_final.sse")
	if err != nil {
		t.Fatal(err)
	}
	var completed json.RawMessage
	for _, line := range strings.Split(string(wire), "\n") {
		if strings.HasPrefix(line, "data: {") {
			var e struct {
				Type     string
				Response json.RawMessage
			}
			if err := json.Unmarshal([]byte(line[6:]), &e); err != nil {
				t.Fatal(err)
			}
			if e.Type == "response.completed" {
				completed = e.Response
			}
		}
	}
	for _, mode := range []contract.StreamMode{contract.Streaming, contract.Buffered} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == contract.Buffered {
					w.Write(completed)
				} else {
					w.Write(wire)
				}
			}))
			defer server.Close()
			p := NewResponsesProvider("test", server.URL)
			req := &contract.CompletionRequest{Model: "test", Messages: messages.User("continue"), Capabilities: &contract.ModelCapabilities{}, StreamMode: mode}
			var reply *messages.ChatMessage
			var answer, commentary strings.Builder
			completions := 0
			for e := range p.ChatCompletionStream(context.Background(), req, messages.NewStreamProcessor()) {
				switch e.Type {
				case messages.EventTypeContent:
					answer.WriteString(e.Content)
				case messages.EventTypeCommentary:
					commentary.WriteString(e.Content)
				case messages.EventTypeComplete:
					reply = e.Message
					completions++
				case messages.EventTypeError:
					t.Fatal(e.Error)
				}
			}
			if reply == nil || completions != 1 || len(reply.TextBlocks) != 2 {
				t.Fatalf("reply=%#v completions=%d", reply, completions)
			}
			if reply.Content != reply.TextBlocks[1].Text || answer.String() != reply.Content || commentary.String() != reply.TextBlocks[0].Text {
				t.Fatalf("phase projection: %#v", reply)
			}
			if !reply.HasTextBlocks() || reply.GetOutputTokens() != 30 {
				t.Fatalf("lost text/usage: %#v", reply)
			}
			// Exercise disk persistence through a complete close/reopen cycle.
			cfg := sessions.StoreConfig{Mode: sessions.ModeDisk, Path: filepath.Join(t.TempDir(), "session.db")}
			store, err := sessions.OpenStore(cfg)
			if err != nil {
				t.Fatal(err)
			}
			session, err := store.Acquire(context.Background(), "phase", sessions.AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err = session.AddMessages(context.Background(), []messages.ChatMessage{*reply}); err != nil {
				t.Fatal(err)
			}
			if err = session.Close(); err != nil {
				t.Fatal(err)
			}
			if err = store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = sessions.OpenStore(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			session, err = store.Acquire(context.Background(), "phase", sessions.AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			history, err := session.GetHistory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			loaded := history[len(history)-1]
			if !reflect.DeepEqual(loaded.TextBlocks, reply.TextBlocks) || loaded.Content != reply.Content {
				t.Fatalf("reload changed reply: %#v", loaded)
			}
			req.Messages = []messages.ChatMessage{loaded}
			input := BuildResponsesRequest(req).Input
			if len(input) != 2 || input[0].Phase != messages.PhaseCommentary || input[1].Phase != messages.PhaseFinalAnswer {
				t.Fatalf("replay=%#v", input)
			}
			for i, item := range input {
				if item.Content.([]ResponseOutputContent)[0].Text != reply.TextBlocks[i].Text {
					t.Fatalf("replay text %d=%#v", i, item)
				}
			}
			// A provider that lacks phases still receives every block, with boundaries.
			chat := messageToChatCompletionParam(loaded)
			if chat.Content != loaded.ModelText() {
				t.Fatalf("cross-dialect text=%#v", chat.Content)
			}
		})
	}
}

func TestResponseOutputOrderUsesCurrentCallsAndScopedReasoning(t *testing.T) {
	body := `{"status":"completed","output":[{"type":"message","id":"msg","phase":"commentary","content":[{"type":"output_text","text":"Checking."}]},{"type":"reasoning","id":"rs","encrypted_content":"opaque","summary":[]},{"type":"function_call","call_id":"call","name":"read","arguments":"{}"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer server.Close()
	req := &contract.CompletionRequest{Model: "test", Messages: messages.User("go"), Capabilities: &contract.ModelCapabilities{}, StreamMode: contract.Buffered}
	reply, err := contract.Complete(context.Background(), NewResponsesProvider("test", server.URL), req)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "" || len(reply.ToolCalls) != 1 || reply.StopReason != messages.StopReasonToolUse {
		t.Fatalf("tool reply=%#v", reply)
	}
	raw, _ := json.Marshal(reply)
	var loaded messages.ChatMessage
	if err = json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	req.Messages = []messages.ChatMessage{loaded}
	input := BuildResponsesRequest(req).Input
	if len(input) != 3 || input[0].Phase != messages.PhaseCommentary || input[1].Type != "reasoning" || input[2].CallID != "call" {
		t.Fatalf("order=%#v", input)
	}
	loaded.ToolCalls[0].Arguments = `{"path":"new"}`
	req.Messages = []messages.ChatMessage{loaded}
	input = BuildResponsesRequest(req).Input
	if input[2].Arguments != loaded.ToolCalls[0].Arguments {
		t.Fatalf("stale arguments=%#v", input)
	}
	req.Model = "other"
	input = BuildResponsesRequest(req).Input
	if len(input) != 2 || input[0].Phase != messages.PhaseCommentary || input[1].Type != "function_call" {
		t.Fatalf("model switch=%#v", input)
	}
	loaded.ToolCalls = nil
	req.Messages = []messages.ChatMessage{loaded}
	input = BuildResponsesRequest(req).Input
	if len(input) != 1 || input[0].Phase != messages.PhaseCommentary {
		t.Fatalf("removed call reappeared: %#v", input)
	}
	loaded.Content = "edited"
	req.Messages = []messages.ChatMessage{loaded}
	input = BuildResponsesRequest(req).Input
	if len(input) != 1 || input[0].Phase != "" || input[0].Content.([]ResponseOutputContent)[0].Text != "edited" {
		t.Fatalf("stale text reappeared: %#v", input)
	}
}
