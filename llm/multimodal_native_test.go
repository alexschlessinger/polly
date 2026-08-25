package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestMultimodalImageSurvivesJSONReloadIntoNativeRequests(t *testing.T) {
	imageBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	imageBase64 := base64.StdEncoding.EncodeToString(imageBytes)
	original := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{
			{Type: "text", Text: "describe this image"},
			{
				Type:      "image_base64",
				ImageData: imageBase64,
				MimeType:  "image/png",
				FileName:  "pixel.png",
			},
		},
	}

	encodedMessage, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal ChatMessage: %v", err)
	}
	var reloaded messages.ChatMessage
	if err := json.Unmarshal(encodedMessage, &reloaded); err != nil {
		t.Fatalf("unmarshal ChatMessage: %v", err)
	}
	if !reflect.DeepEqual(reloaded, original) {
		t.Fatalf("reloaded ChatMessage = %#v, want %#v", reloaded, original)
	}

	t.Run("openai responses input_image", func(t *testing.T) {
		serverURL, captured := newNativeRequestCaptureServer(t, `{}`)
		client := NewOpenAIClient("test-key", "")
		// Keep the wrapper in native Responses mode while pointing its native
		// transport at the mock endpoint. Passing a non-empty wrapper base URL
		// would intentionally select the Chat Completions compatibility path.
		client.client = openai.NewClient("test-key", serverURL+"/v1")
		got := captureNativeCompletionRequest(t, client, "gpt-5.4", reloaded, captured)
		if got.path != "/v1/responses" {
			t.Fatalf("request path = %q, want /v1/responses", got.path)
		}
		wantURL := "data:image/png;base64," + imageBase64
		requireCapturedNativeJSONContains(t, got.body,
			`"type":"input_image"`,
			`"detail":"auto"`,
			`"image_url":"`+wantURL+`"`,
		)
	})

	t.Run("anthropic base64 source", func(t *testing.T) {
		serverURL, captured := newNativeRequestCaptureServer(t, `{}`)
		routeDefaultTransportTo(t, serverURL)
		client := NewAnthropicClient("test-key")
		got := captureNativeCompletionRequest(t, client, "claude-sonnet-4-6", reloaded, captured)
		if got.path != "/v1/messages" {
			t.Fatalf("request path = %q, want /v1/messages", got.path)
		}
		requireCapturedNativeJSONContains(t, got.body,
			`"type":"image"`,
			`"source":{"type":"base64","media_type":"image/png","data":"`+imageBase64+`"}`,
		)
	})

	t.Run("gemini inlineData", func(t *testing.T) {
		serverURL, captured := newNativeRequestCaptureServer(t, `{}`)
		routeDefaultTransportTo(t, serverURL)
		client, err := NewGeminiClient("test-key")
		if err != nil {
			t.Fatalf("NewGeminiClient: %v", err)
		}
		got := captureNativeCompletionRequest(t, client, "gemini-2.5-flash", reloaded, captured)
		if got.path != "/v1beta/models/gemini-2.5-flash:generateContent" {
			t.Fatalf("request path = %q, want Gemini generateContent path", got.path)
		}
		requireCapturedNativeJSONContains(t, got.body,
			`"inlineData":{"mimeType":"image/png","data":"`+imageBase64+`"}`,
		)
	})

	t.Run("ollama images", func(t *testing.T) {
		serverURL, captured := newNativeRequestCaptureServer(t, `{"done":true}`)
		client := NewOllamaClient(serverURL, "")
		got := captureNativeCompletionRequest(t, client, "llava", reloaded, captured)
		if got.path != "/api/chat" {
			t.Fatalf("request path = %q, want /api/chat", got.path)
		}
		requireCapturedNativeJSONContains(t, got.body,
			`"images":["`+imageBase64+`"]`,
		)
	})
}

type capturedNativeRequest struct {
	method string
	path   string
	body   []byte
	err    error
}

func newNativeRequestCaptureServer(t *testing.T, responseBody string) (string, <-chan capturedNativeRequest) {
	t.Helper()
	captured := make(chan capturedNativeRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		captured <- capturedNativeRequest{
			method: r.Method,
			path:   r.URL.Path,
			body:   body,
			err:    err,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	}))
	t.Cleanup(server.Close)
	return server.URL, captured
}

// Anthropic and Gemini calls select their native public API endpoints. Their
// HTTP clients use http.DefaultTransport, so rerouting it in this sequential
// test exercises the real clients while keeping traffic local.
func routeDefaultTransportTo(t *testing.T, serverURL string) {
	t.Helper()
	target, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	previous := http.DefaultTransport
	http.DefaultTransport = nativeRoundTripper(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = target.Scheme
		clone.URL.Host = target.Host
		clone.Host = target.Host
		return previous.RoundTrip(clone)
	})
	t.Cleanup(func() {
		http.DefaultTransport = previous
	})
}

type nativeRoundTripper func(*http.Request) (*http.Response, error)

func (f nativeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func captureNativeCompletionRequest(
	t *testing.T,
	client LLM,
	model string,
	message messages.ChatMessage,
	captured <-chan capturedNativeRequest,
) capturedNativeRequest {
	t.Helper()
	stream := false
	events := client.ChatCompletionStream(context.Background(), &CompletionRequest{
		Model:     model,
		MaxTokens: 128,
		Messages:  []messages.ChatMessage{message},
		Stream:    &stream,
		Timeout:   5 * time.Second,
	}, &SimpleProcessor{})
	for event := range events {
		if event != nil && event.Type == messages.EventTypeError {
			t.Fatalf("native completion failed: %v", event.Error)
		}
	}

	select {
	case got := <-captured:
		if got.err != nil {
			t.Fatalf("read captured native request: %v", got.err)
		}
		if got.method != http.MethodPost {
			t.Fatalf("request method = %q, want POST", got.method)
		}
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("native client did not reach the capture server")
		return capturedNativeRequest{}
	}
}

func requireCapturedNativeJSONContains(t *testing.T, body []byte, fragments ...string) {
	t.Helper()
	if !json.Valid(body) {
		t.Fatalf("captured native request is not valid JSON: %q", body)
	}
	for _, fragment := range fragments {
		if !strings.Contains(string(body), fragment) {
			t.Errorf("captured native request JSON %s does not contain %s", body, fragment)
		}
	}
}
