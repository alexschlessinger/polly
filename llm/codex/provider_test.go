package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

// fakeLogin is a sign-in with a settable token that rotates on refresh.
type fakeLogin struct {
	mu         sync.Mutex
	token      string
	account    string
	signedOut  bool
	refreshErr error
	refreshed  int
}

func newFakeLogin() *fakeLogin { return &fakeLogin{token: "tok-1", account: "acct_1"} }

func (l *fakeLogin) Credential(context.Context) (contract.Credential, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.signedOut {
		return contract.Credential{}, contract.ErrNotSignedIn
	}
	return contract.Credential{AccessToken: l.token, AccountID: l.account, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (l *fakeLogin) Refresh(_ context.Context, rejected string) (contract.Credential, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshed++
	if l.refreshErr != nil {
		return contract.Credential{}, l.refreshErr
	}
	if rejected == l.token {
		l.token += "-r"
	}
	return contract.Credential{AccessToken: l.token, AccountID: l.account, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (l *fakeLogin) Account() (contract.Account, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.signedOut {
		return contract.Account{}, false
	}
	return contract.Account{ID: l.account, Email: "user@example.com", Plan: "plus"}, true
}

// backendCall is one request the fake backend saw.
type backendCall struct {
	header http.Header
	body   map[string]any
	raw    []byte
}

// fakeBackend records requests to /responses and answers with whatever
// respond returns.
type fakeBackend struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []backendCall
	respond func(w http.ResponseWriter, call backendCall, n int)
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	b := &fakeBackend{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "not found", 404)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body %s: %v", raw, err)
		}
		call := backendCall{header: r.Header.Clone(), body: body, raw: raw}
		b.mu.Lock()
		b.calls = append(b.calls, call)
		n := len(b.calls)
		b.mu.Unlock()
		b.respond(w, call, n)
	}))
	t.Cleanup(b.Close)
	b.respond = func(w http.ResponseWriter, _ backendCall, _ int) { fmt.Fprint(w, okStream("ok")) }
	return b
}

func (b *fakeBackend) baseURL() string { return b.URL + "/backend-api/codex" }

func (b *fakeBackend) call(i int) backendCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[i]
}

func (b *fakeBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

func responsesStream(events ...string) string {
	var sb strings.Builder
	for _, event := range events {
		fmt.Fprintf(&sb, "data: %s\n\n", event)
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

func okStream(text string) string {
	response := `{"id":"resp_1","model":"gpt-5.5","status":"completed","output":[{"type":"message","id":"msg_1","status":"completed","content":[{"type":"output_text","text":"` + text + `"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`
	return responsesStream(
		`{"type":"response.output_text.delta","output_index":0,"delta":"`+text+`"}`,
		`{"type":"response.completed","response":`+response+`}`,
	)
}

func newTestProvider(t *testing.T, backend *fakeBackend, login contract.Login) *Provider {
	t.Helper()
	return NewProvider(login, backend.baseURL(), WithHTTPClient(backend.Client()))
}

func userRequest(model, text string) *contract.CompletionRequest {
	return &contract.CompletionRequest{Model: model, Capabilities: &contract.ModelCapabilities{}, Messages: messages.User(text)}
}

// echoTool is a tool the request advertises; it never runs here.
var echoTool tools.Tool = &tools.Func{Name: "echo", Run: func(context.Context, tools.Args) (string, error) { return "", nil }}

func TestRequestGoldenBodyAndHeaders(t *testing.T) {
	backend := newFakeBackend(t)
	login := newFakeLogin()
	p := newTestProvider(t, backend, login)
	temp := float32(1)
	req := &contract.CompletionRequest{
		Model:          "gpt-5.5",
		Temperature:    &temp,
		MaxTokens:      64000,
		PromptCacheKey: "prefix-1",
		CacheSessionID: "sess-1",
		ThinkingEffort: contract.EffortLevel(contract.LevelHigh),
		Capabilities:   &contract.ModelCapabilities{},
		Tools:          []tools.Tool{echoTool},
		Messages:       append([]messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "Be brief."}}, messages.User("hi")...),
	}
	reply, err := contract.Complete(context.Background(), p, req)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "ok" || backend.count() != 1 {
		t.Fatalf("reply = %+v after %d calls", reply, backend.count())
	}
	call := backend.call(0)
	body := call.body
	for _, absent := range []string{"temperature", "max_output_tokens", "metadata", "previous_response_id"} {
		if _, present := body[absent]; present {
			t.Errorf("body carries %s: %v", absent, body[absent])
		}
	}
	if body["model"] != "gpt-5.5" || body["instructions"] != "Be brief." || body["store"] != false || body["stream"] != true || body["prompt_cache_key"] != "prefix-1" || body["tool_choice"] != "auto" || body["parallel_tool_calls"] != true {
		t.Errorf("body = %s", call.raw)
	}
	if fmt.Sprint(body["include"]) != "[reasoning.encrypted_content]" {
		t.Errorf("include = %v", body["include"])
	}
	if reasoning, _ := body["reasoning"].(map[string]any); reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Errorf("reasoning = %v", body["reasoning"])
	}
	if tools, _ := body["tools"].([]any); len(tools) != 1 || tools[0].(map[string]any)["name"] != "echo" {
		t.Errorf("tools = %v", body["tools"])
	}
	h := call.header
	if h.Get("Authorization") != "Bearer tok-1" || h.Get("chatgpt-account-id") != "acct_1" || h.Get("originator") != "polly" || h.Get("session-id") != "sess-1" || h.Get("Accept") != "text/event-stream" {
		t.Errorf("headers = %v", h)
	}
	if ua := h.Get("User-Agent"); !strings.HasPrefix(ua, "polly/") || !strings.Contains(ua, runtime.GOOS+" "+runtime.GOARCH) {
		t.Errorf("user agent = %q", ua)
	}
}

func TestRequestDefaultsInstructionsAndReasoning(t *testing.T) {
	backend := newFakeBackend(t)
	p := newTestProvider(t, backend, newFakeLogin())
	req := userRequest("gpt-5.5", "hi")
	req.ThinkingEffort = contract.EffortOff()
	if _, err := contract.Complete(context.Background(), p, req); err != nil {
		t.Fatal(err)
	}
	body := backend.call(0).body
	if body["instructions"] != defaultInstructions {
		t.Errorf("instructions = %v", body["instructions"])
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if _, hasEffort := reasoning["effort"]; hasEffort || reasoning["summary"] != "auto" {
		t.Errorf("reasoning = %v", body["reasoning"])
	}
	for _, absent := range []string{"tool_choice", "parallel_tool_calls", "tools"} {
		if _, present := body[absent]; present {
			t.Errorf("body carries %s without tools", absent)
		}
	}
	if _, present := backend.call(0).header["Session-Id"]; present {
		t.Error("session-id sent without a session")
	}
}

func TestStructuredOutputRidesInTheTextBlock(t *testing.T) {
	backend := newFakeBackend(t)
	p := newTestProvider(t, backend, newFakeLogin())
	req := userRequest("gpt-5.5", "hi")
	req.ResponseSchema = schema.MustSchemaFromJSON(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`)
	if _, err := contract.Complete(context.Background(), p, req); err != nil {
		t.Fatal(err)
	}
	text, _ := backend.call(0).body["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "response" {
		t.Fatalf("text = %v", backend.call(0).body["text"])
	}
}

func TestBufferedRequestsStillStream(t *testing.T) {
	backend := newFakeBackend(t)
	p := newTestProvider(t, backend, newFakeLogin())
	req := userRequest("gpt-5.5", "hi")
	req.StreamMode = contract.Buffered
	reply, err := contract.Complete(context.Background(), p, req)
	if err != nil || reply.Content != "ok" {
		t.Fatalf("reply = %+v, err = %v", reply, err)
	}
	if backend.call(0).body["stream"] != true {
		t.Fatal("buffered request was not streamed")
	}
}

func TestRejectedTokenIsRefreshedOnce(t *testing.T) {
	backend := newFakeBackend(t)
	backend.respond = func(w http.ResponseWriter, call backendCall, n int) {
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"token expired","type":"invalid_token"}}`)
			return
		}
		fmt.Fprint(w, okStream("ok"))
	}
	login := newFakeLogin()
	p := newTestProvider(t, backend, login)
	reply, err := contract.Complete(context.Background(), p, userRequest("gpt-5.5", "hi"))
	if err != nil || reply.Content != "ok" {
		t.Fatalf("reply = %+v, err = %v", reply, err)
	}
	if backend.count() != 2 || login.refreshed != 1 {
		t.Fatalf("calls = %d, refreshes = %d", backend.count(), login.refreshed)
	}
	second := backend.call(1)
	if second.header.Get("Authorization") != "Bearer tok-1-r" || second.body["model"] != "gpt-5.5" {
		t.Fatalf("retry = %v %s", second.header, second.raw)
	}
}

func TestRefreshFailureSurfacesTheSignIn(t *testing.T) {
	backend := newFakeBackend(t)
	backend.respond = func(w http.ResponseWriter, _ backendCall, _ int) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"token expired"}}`)
	}
	login := newFakeLogin()
	login.refreshErr = fmt.Errorf("codex: the sign-in has expired; sign in again: %w", contract.ErrNotSignedIn)
	p := newTestProvider(t, backend, login)
	// Stream errors reach the caller as text, so the wording carries the
	// verdict.
	_, err := contract.Complete(context.Background(), p, userRequest("gpt-5.5", "hi"))
	if err == nil || !strings.Contains(err.Error(), "sign in again") || !strings.Contains(err.Error(), contract.ErrNotSignedIn.Error()) {
		t.Fatalf("err = %v", err)
	}
	if backend.count() != 1 || login.refreshed != 1 {
		t.Fatalf("calls = %d, refreshes = %d", backend.count(), login.refreshed)
	}

	// A 401 the refresh could not explain still names the sign-in.
	login.refreshErr = errors.New("authority unreachable")
	_, err = contract.Complete(context.Background(), p, userRequest("gpt-5.5", "hi"))
	if err == nil || !strings.Contains(err.Error(), "authority unreachable") {
		t.Fatalf("err = %v", err)
	}
}

func TestUsageLimitIsTerminalAndDescribed(t *testing.T) {
	backend := newFakeBackend(t)
	resets := time.Now().Add(90 * time.Minute).Unix()
	backend.respond = func(w http.ResponseWriter, _ backendCall, _ int) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `{"error":{"type":"usage_limit_reached","message":"You have hit your usage limit.","plan_type":"plus","resets_at":%d}}`, resets)
	}
	p := newTestProvider(t, backend, newFakeLogin())
	done := make(chan error, 1)
	go func() {
		_, err := contract.Complete(context.Background(), p, userRequest("gpt-5.5", "hi"))
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("usage limit waited out Retry-After")
	}
	if err == nil || !strings.Contains(err.Error(), "usage limit is reached (plus plan)") || !strings.Contains(err.Error(), "resets in about 1h") {
		t.Fatalf("err = %v", err)
	}
	if backend.count() != 1 {
		t.Fatalf("terminal refusal retried: %d calls", backend.count())
	}

	// A plan without Codex, and an ordinary rate limit that is retried.
	backend.respond = func(w http.ResponseWriter, _ backendCall, n int) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":"usage_not_included","message":"Upgrade to Plus."}}`)
	}
	_, err = contract.Complete(context.Background(), p, userRequest("gpt-5.5", "hi"))
	if err == nil || !strings.Contains(err.Error(), "does not include Codex") || !strings.Contains(err.Error(), "Upgrade to Plus.") {
		t.Fatalf("err = %v", err)
	}
	before := backend.count()
	backend.respond = func(w http.ResponseWriter, _ backendCall, n int) {
		if n == before+1 {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`)
			return
		}
		fmt.Fprint(w, okStream("ok"))
	}
	if reply, err := contract.Complete(context.Background(), p, userRequest("gpt-5.5", "hi")); err != nil || reply.Content != "ok" || backend.count() != before+2 {
		t.Fatalf("rate limit not retried: %+v, %v, %d calls", reply, err, backend.count())
	}
}

func TestUsageMetersLandInTheReply(t *testing.T) {
	backend := newFakeBackend(t)
	reset := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	backend.respond = func(w http.ResponseWriter, _ backendCall, _ int) {
		w.Header().Set("x-codex-primary-used-percent", "42.5")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		w.Header().Set("x-codex-primary-reset-at", fmt.Sprint(reset.Unix()))
		w.Header().Set("x-codex-secondary-used-percent", "7")
		w.Header().Set("x-codex-secondary-window-minutes", "10080")
		fmt.Fprint(w, okStream("ok"))
	}
	p := newTestProvider(t, backend, newFakeLogin())
	reply, err := contract.Complete(context.Background(), p, userRequest("gpt-5.5", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	usage, ok := UsageFrom(*reply)
	if !ok || usage.Primary.UsedPercent != 42.5 || usage.Primary.WindowMinutes != 300 || !usage.Primary.ResetAt.Equal(reset) || usage.Secondary.UsedPercent != 7 || usage.Secondary.WindowMinutes != 10080 || !usage.Secondary.ResetAt.IsZero() {
		t.Fatalf("usage = %+v (%v) from %v", usage, ok, reply.Metadata)
	}
	raw, _ := json.Marshal(reply)
	var loaded messages.ChatMessage
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if reloaded, ok := UsageFrom(loaded); !ok || reloaded != usage {
		t.Fatalf("reloaded usage = %+v (%v)", reloaded, ok)
	}
	if _, ok := UsageFrom(messages.ChatMessage{}); ok {
		t.Fatal("usage read from a reply without any")
	}
}

func TestReasoningReplaysOnlyThroughThisProvider(t *testing.T) {
	backend := newFakeBackend(t)
	item := `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"opaque-1"}`
	backend.respond = func(w http.ResponseWriter, _ backendCall, _ int) {
		response := `{"id":"resp_1","status":"completed","output":[` + item + `,{"type":"message","id":"msg_1","status":"completed","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`
		fmt.Fprint(w, responsesStream(
			`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
			`{"type":"response.output_text.delta","output_index":1,"delta":"answer"}`,
			`{"type":"response.completed","response":`+response+`}`,
		))
	}
	p := newTestProvider(t, backend, newFakeLogin())
	first, err := contract.Complete(context.Background(), p, userRequest("gpt-5.5", "why?"))
	if err != nil || first.Content != "answer" {
		t.Fatalf("reply = %+v, err = %v", first, err)
	}
	if first.Metadata[openai.ResponsesReasoningModelKey] != "codex/gpt-5.5" {
		t.Fatalf("reasoning scope = %v", first.Metadata[openai.ResponsesReasoningModelKey])
	}
	raw, _ := json.Marshal(first)
	var loaded messages.ChatMessage
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if items := openai.ReplayReasoningItems(loaded, "gpt-5.5"); len(items) != 0 {
		t.Fatalf("api.openai.com would replay codex reasoning: %+v", items)
	}
	history := append(messages.User("why?"), loaded)
	history = append(history, messages.User("and?")...)
	req := userRequest("gpt-5.5", "")
	req.Messages = history
	if _, err := contract.Complete(context.Background(), p, req); err != nil {
		t.Fatal(err)
	}
	input, _ := backend.call(1).body["input"].([]any)
	var replayed bool
	for _, entry := range input {
		m, _ := entry.(map[string]any)
		if m["type"] == "reasoning" && m["encrypted_content"] == "opaque-1" && m["id"] == "rs_1" {
			replayed = true
		}
	}
	if !replayed {
		t.Fatalf("reasoning not replayed: %s", backend.call(1).raw)
	}
	// After a model switch the items stay home.
	req.Model = "gpt-6-sol"
	if _, err := contract.Complete(context.Background(), p, req); err != nil {
		t.Fatal(err)
	}
	for _, entry := range mustArray(backend.call(2).body["input"]) {
		if m, _ := entry.(map[string]any); m["type"] == "reasoning" {
			t.Fatalf("reasoning replayed to another model: %s", backend.call(2).raw)
		}
	}
}

func mustArray(v any) []any { a, _ := v.([]any); return a }

func TestSignedOutMakesNoRequest(t *testing.T) {
	backend := newFakeBackend(t)
	login := newFakeLogin()
	login.signedOut = true
	p := newTestProvider(t, backend, login)
	_, err := contract.Complete(context.Background(), p, userRequest("gpt-5.5", "hi"))
	if err == nil || !strings.Contains(err.Error(), contract.ErrNotSignedIn.Error()) || backend.count() != 0 {
		t.Fatalf("err = %v after %d calls", err, backend.count())
	}
	if _, err := contract.Complete(context.Background(), NewProvider(nil, backend.baseURL()), userRequest("gpt-5.5", "hi")); err == nil || !strings.Contains(err.Error(), "no sign-in") {
		t.Fatalf("nil login: %v", err)
	}
}

func TestStreamErrorEventSurfaces(t *testing.T) {
	backend := newFakeBackend(t)
	backend.respond = func(w http.ResponseWriter, _ backendCall, _ int) {
		fmt.Fprint(w, responsesStream(`{"type":"error","code":"usage_limit_reached","message":"limit reached"}`))
	}
	p := newTestProvider(t, backend, newFakeLogin())
	reply, err := contract.Complete(context.Background(), p, userRequest("gpt-5.5", "hi"))
	switch {
	case err != nil:
		if !strings.Contains(err.Error(), "usage_limit_reached") {
			t.Fatalf("err = %v", err)
		}
	case reply == nil || !reply.IsError() || !strings.Contains(reply.GetError().Error(), "usage_limit_reached"):
		t.Fatalf("reply = %+v", reply)
	}
}

func TestCallerClientIsNotMutated(t *testing.T) {
	backend := newFakeBackend(t)
	var calls int
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return http.DefaultTransport.RoundTrip(req)
	})
	client := &http.Client{Transport: transport}
	p := NewProvider(newFakeLogin(), backend.baseURL(), WithHTTPClient(client))
	if _, err := contract.Complete(context.Background(), p, userRequest("gpt-5.5", "hi")); err != nil {
		t.Fatal(err)
	}
	if _, ok := client.Transport.(roundTripFunc); !ok || calls != 1 {
		t.Fatalf("caller transport replaced (%T) or bypassed (%d calls)", client.Transport, calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestDescribeKeepsTypedErrors pins what the stream flattens to text: the
// refusal with its plan and reset, and the sign-in verdict.
func TestDescribeKeepsTypedErrors(t *testing.T) {
	resets := time.Now().Add(time.Hour)
	call := &callState{}
	call.setTerminal(&UsageError{Code: "usage_limit_reached", Plan: "pro", ResetsAt: resets})
	err := describe(&openai.APIError{StatusCode: 429, Type: "usage_limit_reached"}, call)
	var usageErr *UsageError
	if !errors.As(err, &usageErr) || usageErr.Plan != "pro" || !usageErr.ResetsAt.Equal(resets) {
		t.Fatalf("err = %v", err)
	}
	if err := describe(&openai.APIError{StatusCode: 429, Code: "insufficient_quota", Message: "none left"}, &callState{}); !errors.As(err, &usageErr) || usageErr.Code != "insufficient_quota" || !strings.Contains(err.Error(), "none left") {
		t.Fatalf("err = %v", err)
	}
	if err := describe(&openai.APIError{StatusCode: 401}, &callState{}); !errors.Is(err, contract.ErrNotSignedIn) {
		t.Fatalf("401 = %v", err)
	}
	call = &callState{}
	call.setRefreshError(errors.New("authority unreachable"))
	if err := describe(&openai.APIError{StatusCode: 401}, call); err == nil || err.Error() != "authority unreachable" {
		t.Fatalf("401 after a failed refresh = %v", err)
	}
	plain := errors.New("boom")
	if err := describe(plain, &callState{}); err != plain {
		t.Fatalf("plain error = %v", err)
	}
	if describe(nil, &callState{}) != nil {
		t.Fatal("nil error described")
	}
}

func TestParseUsageAndTimestamps(t *testing.T) {
	h := http.Header{}
	if _, ok := parseUsage(h); ok {
		t.Fatal("usage read from empty headers")
	}
	h.Set("x-codex-secondary-used-percent", " 12.25 ")
	h.Set("x-codex-secondary-reset-at", "2026-09-30T12:00:00Z")
	u, ok := parseUsage(h)
	if !ok || u.Secondary.UsedPercent != 12.25 || u.Secondary.ResetAt.Format(time.RFC3339) != "2026-09-30T12:00:00Z" || u.Primary.UsedPercent != 0 {
		t.Fatalf("usage = %+v (%v)", u, ok)
	}
	if !parseTimestamp("garbage").IsZero() || parseTimestamp("1790000000").Unix() != 1790000000 {
		t.Fatal("timestamp parsing")
	}
	if e := parseUsageError([]byte(`{"error":{"type":"rate_limit_exceeded"}}`)); e != nil {
		t.Fatalf("ordinary rate limit parsed as terminal: %+v", e)
	}
	e := parseUsageError([]byte(`{"error":{"code":"usage_limit_reached","resets_in_seconds":120}}`))
	if e == nil || e.Code != "usage_limit_reached" || time.Until(e.ResetsAt) < time.Minute {
		t.Fatalf("refusal = %+v", e)
	}
}
