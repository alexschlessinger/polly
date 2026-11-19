package llm

import (
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestProviderHTTPAttribution(t *testing.T) {
	// Exercise OpenRouter first: its app headers must not stick to the client
	// that later providers share with it.
	providers := defaultProviders(providerDeps{httpClient: &http.Client{}})
	const chat = `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`
	for _, tc := range []struct{ provider, response string }{
		{"openrouter", chat},
		{"huggingface", chat},
		{"anthropic", `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			url, captured := newNativeRequestCaptureServer(t, tc.response)
			client, err := providers[tc.provider].new("test-key", url)
			if err != nil {
				t.Fatal(err)
			}
			h := captureNativeCompletionMessages(t, client, "test-model", messages.User("hello"), captured).header
			ua := h.Get("User-Agent")
			version, ok := strings.CutPrefix(ua, "polly/")
			version, platform := strings.CutSuffix(version, " ("+runtime.GOOS+" "+runtime.GOARCH+")")
			if !ok || !platform || version == "" {
				t.Fatalf("unexpected Polly User-Agent %q", ua)
			}
			if tc.provider != "openrouter" && (h.Values("HTTP-Referer") != nil || h.Values("X-OpenRouter-Title") != nil) {
				t.Fatal("OpenRouter app attribution leaked to another provider")
			}
		})
	}
}
