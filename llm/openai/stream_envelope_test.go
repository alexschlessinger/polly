package openai

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestStreamChatEnvelopeErrorCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		apiError      bool
	}{
		{"escaped error key", `{"\u0065rror":{"message":"failed"},"choices":"ignored on error"}`, true},
		{"case folded error key", `{"ERROR":{"message":"failed"}}`, true},
		{"malformed error field", `{"error":"not an error object","choices":[]}`, false},
		{"null error", `{"error":null,"choices":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("data: " + tc.payload + "\n\ndata: [DONE]\n\n"))
			})
			count := 0
			for chunk, err := range client.StreamChatCompletion(context.Background(), &ChatCompletionRequest{}) {
				count++
				var apiErr *APIError
				if tc.apiError {
					if !errors.As(err, &apiErr) || apiErr.Message != "failed" || chunk != nil {
						t.Fatalf("chunk=%+v err=%v, want API error", chunk, err)
					}
				} else if err != nil || chunk == nil {
					t.Fatalf("chunk=%+v err=%v, want normal chunk", chunk, err)
				}
			}
			if count != 1 {
				t.Fatalf("received %d events, want one", count)
			}
		})
	}
}
