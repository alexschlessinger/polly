package ollama

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestChatEnvelopeErrorCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		apiError      bool
	}{
		{"escaped error key", `{"\u0065rror":"failed","message":"ignored on error"}`, true},
		{"case folded error key", `{"ERROR":"failed"}`, true},
		{"malformed error field", `{"error":42,"done":true}`, false},
		{"null error", `{"error":null,"done":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tc.payload + "\n"))
			})
			count := 0
			err := client.Chat(context.Background(), &ChatRequest{}, func(ChatResponse) error {
				count++
				return nil
			})
			var apiErr StatusError
			if tc.apiError {
				if !errors.As(err, &apiErr) || apiErr.ErrorMessage != "failed" || count != 0 {
					t.Fatalf("count=%d err=%v, want API error", count, err)
				}
			} else if err != nil || count != 1 {
				t.Fatalf("count=%d err=%v, want normal chunk", count, err)
			}
		})
	}
}
