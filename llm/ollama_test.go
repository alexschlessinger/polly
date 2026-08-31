package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// TestOllamaDefaultsToStreaming: a nil CompletionRequest.Stream means
// streaming, per the interface contract; the outgoing Ollama request must say
// stream:true and the stream must still complete with the full content.
func TestOllamaDefaultsToStreaming(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Write([]byte(
			`{"message":{"role":"assistant","content":"hi"},"done":false}` + "\n" +
				`{"message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":1,"eval_count":2}` + "\n"))
	}))
	t.Cleanup(server.Close)

	client := NewOllamaClient(server.URL, "")
	events := client.ChatCompletionStream(context.Background(), &CompletionRequest{
		Model:     "test-model",
		Messages:  messages.User("hello"),
		MaxTokens: 16,
	}, &SimpleProcessor{})

	var complete *messages.ChatMessage
	for event := range events {
		switch event.Type {
		case messages.EventTypeError:
			t.Fatalf("stream error: %v", event.Error)
		case messages.EventTypeComplete:
			complete = event.Message
		}
	}

	if gotBody["stream"] != true {
		t.Errorf("request stream = %v, want true", gotBody["stream"])
	}
	if complete == nil || complete.Content != "hi" {
		t.Fatalf("complete = %#v, want content %q", complete, "hi")
	}
}
