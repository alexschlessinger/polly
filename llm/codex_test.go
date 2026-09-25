package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/codex"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/messages"
)

// testLogin is a sign-in the tests can switch off.
type testLogin struct {
	mu        sync.Mutex
	token     string
	signedOut bool
}

func (l *testLogin) Credential(context.Context) (Credential, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.signedOut {
		return Credential{}, ErrNotSignedIn
	}
	return Credential{AccessToken: l.token, AccountID: "acct_1"}, nil
}

func (l *testLogin) Refresh(context.Context, string) (Credential, error) {
	return l.Credential(context.Background())
}

func (l *testLogin) Account() (Account, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.signedOut {
		return Account{}, false
	}
	return Account{ID: "acct_1", Email: "user@example.com", Plan: "plus"}, true
}

type recordingTransport struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []map[string]any
	respond  func(*http.Request) *http.Response
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	t.mu.Lock()
	t.requests = append(t.requests, req)
	t.bodies = append(t.bodies, body)
	t.mu.Unlock()
	return t.respond(req), nil
}

func sseResponse(events ...string) *http.Response {
	var b strings.Builder
	for _, event := range events {
		b.WriteString("data: " + event + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(b.String()))}
}

func TestCodexConfiguration(t *testing.T) {
	spec := defaultProviders(providerDeps{})["codex"]
	if spec.defaultBaseURL != codex.DefaultBaseURL || !spec.nativeEndpoint || !spec.signIn || !spec.keylessCatalog || spec.keyless == nil || spec.metadata == nil {
		t.Fatal("bad provider configuration")
	}
	if ProviderRequiresKey("codex/m", "") || !ProviderRequiresLogin("codex/m") || ProviderRequiresLogin("openai/m") || ProviderRequiresLogin("m") {
		t.Fatal("bad credential rules")
	}
	m := NewMultiPass(nil)
	if envVar, missing := m.MissingAPIKey("codex/m", ""); missing || envVar != "" {
		t.Fatalf("MissingAPIKey = %q, %v", envVar, missing)
	}
	if !m.LoginRequired("codex/m") || m.LoginRequired("openai/m") || m.LoginRequired("m") {
		t.Fatal("LoginRequired without a sign-in")
	}
	if _, ok := m.Account("codex"); ok {
		t.Fatal("account without a sign-in")
	}
	_, err := routerCompletion(context.Background(), m, &CompletionRequest{Model: "codex/m"})
	if err == nil || !strings.Contains(err.Error(), "needs a sign-in") || !strings.Contains(err.Error(), ErrNotSignedIn.Error()) {
		t.Fatalf("request without a sign-in: %v", err)
	}

	login := &testLogin{token: "tok"}
	m = NewMultiPass(nil, WithLogin("Codex", login))
	if m.LoginRequired("codex/m") {
		t.Fatal("LoginRequired with a sign-in")
	}
	if acct, ok := m.Account("codex"); !ok || acct.ID != "acct_1" || acct.Plan != "plus" {
		t.Fatalf("account = %+v (%v)", acct, ok)
	}
	login.signedOut = true
	if !m.LoginRequired("codex/m") {
		t.Fatal("LoginRequired after signing out")
	}
	if _, ok := m.Account("codex"); ok {
		t.Fatal("account after signing out")
	}
}

func TestCodexRoundTripThroughTheRouter(t *testing.T) {
	item := `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque"}`
	response := `{"id":"resp_1","status":"completed","output":[` + item + `,{"type":"message","id":"msg_1","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`
	transport := &recordingTransport{respond: func(*http.Request) *http.Response {
		return sseResponse(
			`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
			`{"type":"response.output_text.delta","output_index":1,"delta":"ok"}`,
			`{"type":"response.completed","response":`+response+`}`,
		)
	}}
	client := &http.Client{Transport: transport}
	login := &testLogin{token: "tok"}
	m := NewMultiPass(map[string]string{"openai": "key"}, WithLogin("codex", login), WithHTTPClient(client))
	req := &CompletionRequest{Model: "codex/gpt-5.5", BaseURL: "https://example.invalid/v1", StreamMode: Buffered, Capabilities: &ModelCapabilities{}, CacheSessionID: "sess", Messages: messages.User("why?")}
	first, err := routerCompletion(context.Background(), m, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Content != "ok" || first.GetInputTokens() != 3 || first.Metadata[openai.ResponsesReasoningModelKey] != "codex/gpt-5.5" {
		t.Fatalf("reply = %+v", first)
	}
	sent := transport.requests[0]
	if sent.URL.String() != codex.DefaultBaseURL+"/responses" {
		t.Fatalf("a global base URL redirected the native endpoint: %s", sent.URL)
	}
	if sent.Header.Get("Authorization") != "Bearer tok" || sent.Header.Get("chatgpt-account-id") != "acct_1" || sent.Header.Get("originator") != "polly" || sent.Header.Get("session-id") != "sess" {
		t.Fatalf("headers = %v", sent.Header)
	}
	if body := transport.bodies[0]; body["model"] != "gpt-5.5" || body["store"] != false {
		t.Fatalf("body = %v", body)
	}
	if req.Model != "codex/gpt-5.5" || req.BaseURL != "https://example.invalid/v1" {
		t.Fatal("mutated caller request")
	}

	// The reply survives a session reload and replays only through codex.
	raw, _ := json.Marshal(first)
	var loaded messages.ChatMessage
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	history := append(messages.User("why?"), loaded)
	history = append(history, messages.User("and?")...)
	req.Messages = history
	if _, err := routerCompletion(context.Background(), m, req); err != nil {
		t.Fatal(err)
	}
	if !replaysReasoning(transport.bodies[1]) {
		t.Fatalf("codex did not replay its reasoning: %v", transport.bodies[1]["input"])
	}
	transport.respond = func(*http.Request) *http.Response {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(response))}
	}
	req.Model, req.BaseURL = "openai/gpt-5.5", ""
	if _, err := routerCompletion(context.Background(), m, req); err != nil {
		t.Fatal(err)
	}
	if replaysReasoning(transport.bodies[2]) {
		t.Fatalf("api.openai.com replayed codex reasoning: %v", transport.bodies[2]["input"])
	}
}

func replaysReasoning(body map[string]any) bool {
	input, _ := body["input"].([]any)
	for _, entry := range input {
		if m, _ := entry.(map[string]any); m["type"] == "reasoning" && m["encrypted_content"] == "opaque" {
			return true
		}
	}
	return false
}

func TestCodexCatalogIsScopedByTheAccount(t *testing.T) {
	first := NewMultiPass(nil, WithLogin("codex", &testLogin{token: "tok-a"}))
	second := NewMultiPass(nil, WithLogin("codex", &testLogin{token: "tok-b"}))
	targetA, specA, err := first.metadataTarget(ModelTarget{Provider: "codex"})
	if err != nil || !specA.signIn || targetA.APIKey != "acct_1" || targetA.BaseURL != codex.DefaultBaseURL {
		t.Fatalf("target = %+v, err = %v", targetA, err)
	}
	targetB, _, err := second.metadataTarget(ModelTarget{Provider: "codex"})
	if err != nil || metadataKey(targetA, "") != metadataKey(targetB, "") {
		t.Fatalf("catalog scope follows the token, not the account: %v", err)
	}
	login := &testLogin{token: "tok", signedOut: true}
	if _, _, err := NewMultiPass(nil, WithLogin("codex", login)).metadataTarget(ModelTarget{Provider: "codex"}); !errors.Is(err, ErrModelMetadataUnknown) {
		t.Fatalf("signed out: %v", err)
	}
	if _, _, err := NewMultiPass(nil).metadataTarget(ModelTarget{Provider: "codex"}); !errors.Is(err, ErrModelMetadataUnknown) {
		t.Fatalf("no sign-in: %v", err)
	}
}
