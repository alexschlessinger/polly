package llm

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
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

func routerCompletion(ctx context.Context, client LLM, req *CompletionRequest) (*messages.ChatMessage, error) {
	var final *messages.ChatMessage
	var err error
	for event := range client.ChatCompletionStream(ctx, req, messages.NewStreamProcessor()) {
		if event.Type == messages.EventTypeError {
			err = event.Error
		}
		if event.Type == messages.EventTypeComplete {
			final = event.Message
		}
	}
	return final, err
}

func routerSSE(w http.ResponseWriter, value any) {
	data, _ := json.Marshal(value)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func routerDelta(w http.ResponseWriter, delta any, finish string) {
	routerSSE(w, map[string]any{"choices": []any{map[string]any{"delta": delta, "finish_reason": finish}}})
}

// Both modes use the production MultiPass OpenRouter Chat Completions route,
// then replay the persisted response in a second HTTP request.
func TestOpenRouterReasoningRoundTrip(t *testing.T) {
	for _, stream := range []bool{true, false} {
		for _, form := range []string{"plain", "details", "dual", "empty", "unattributed"} {
			t.Run(fmt.Sprintf("%t/%s", stream, form), func(t *testing.T) {
				details := json.RawMessage(`[{"type":"reasoning.text","text":"think","signature":"signed","opaque":{"keep":true}},{"type":"reasoning.encrypted","data":"ciphertext"}]`)
				if form == "empty" {
					details = json.RawMessage(`[]`)
				}
				requests := make(chan openai.ChatCompletionRequest, 4)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req openai.ChatCompletionRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						return
					}
					requests <- req
					response := map[string]any{"content": "answer", "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"x":1}`}}}}
					if form != "details" {
						response["reasoning"] = "think"
					}
					if form == "details" || form == "dual" || form == "empty" {
						response["reasoning_details"] = details
					}
					if req.Stream {
						routerSSE(w, map[string]any{"id": "first-id", "model": "returned-model", "choices": []any{}})
						routerDelta(w, response, "tool_calls")
						routerSSE(w, map[string]any{"provider": "actual-provider", "choices": []any{}})
						fmt.Fprint(w, "data: [DONE]\n\n")
					} else {
						json.NewEncoder(w).Encode(map[string]any{"id": "first-id", "model": "returned-model", "provider": "actual-provider", "choices": []any{map[string]any{"message": response, "finish_reason": "tool_calls"}}})
					}
				}))
				defer server.Close()
				client := NewMultiPass(map[string]string{"openrouter": "fixture"})
				req := &CompletionRequest{Model: "openrouter/org/model", BaseURL: server.URL, Stream: &stream, Capabilities: &ModelCapabilities{}, Messages: []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "test"}}}
				final, err := routerCompletion(context.Background(), client, req)
				if err != nil || final == nil {
					t.Fatalf("completion: %v, %v", final, err)
				}
				<-requests
				if final.Reasoning != "think" || final.Content != "answer" || len(final.ToolCalls) != 1 {
					t.Fatalf("channels: %+v", final)
				}
				meta := final.Metadata["openrouter"].(map[string]any)
				for key, want := range map[string]string{"response_id": "first-id", "model": "returned-model", "provider": "actual-provider", "requested_model": "org/model", "endpoint": server.URL} {
					if meta[key] != want {
						t.Errorf("%s = %v", key, meta[key])
					}
				}
				if form == "unattributed" {
					delete(final.Metadata, "openrouter")
				}
				loaded := routerSQLiteReload(t, *final)
				req.Messages = append(req.Messages, loaded, messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "c1", Content: "failed", Metadata: map[string]any{"tool_succeeded": false}})
				if _, err = routerCompletion(context.Background(), client, req); err != nil {
					t.Fatal(err)
				}
				wire := <-requests
				assistant := wire.Messages[1]
				if form == "plain" && assistant.Reasoning != "think" {
					t.Fatal("plaintext was not replayed")
				}
				if form == "details" || form == "dual" || form == "empty" {
					if assistant.Reasoning != "" || !equalJSON(assistant.ReasoningDetails, details) {
						t.Fatalf("details replay: %+v", assistant)
					}
				}
				if form == "unattributed" && (assistant.Reasoning != "" || assistant.ReasoningDetails != nil) {
					t.Fatal("replayed legacy reasoning")
				}
				if loaded.Reasoning != "think" {
					t.Fatal("modified historical display")
				}
				// Changing requested model or endpoint excludes replay without
				// editing history. Upstream provider changes do not matter.
				for _, origin := range [][2]string{{server.URL, "different-model"}, {server.URL + "/other", "org/model"}} {
					p, d := openai.OpenRouterReplay(loaded, origin[0], origin[1])
					if p != "" || d != nil {
						t.Fatal("cross-origin replay")
					}
				}
			})
		}
	}
}

func equalJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func routerSQLiteReload(t *testing.T, msg messages.ChatMessage) messages.ChatMessage {
	t.Helper()
	cfg := sessions.StoreConfig{Mode: sessions.ModeDisk, Path: filepath.Join(t.TempDir(), "session.db")}
	store, err := sessions.OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Acquire(context.Background(), "replay", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AddMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sessions.OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session, err = store.Acquire(context.Background(), "replay", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	history, err := session.GetHistory(context.Background())
	if err != nil || len(history) == 0 {
		t.Fatalf("reload: %v", err)
	}
	return history[len(history)-1]
}

func TestOpenRouterRepeatedIndicesAndOpaqueBlocks(t *testing.T) {
	fragments := []string{
		`[{"type":"reasoning.text","index":0,"text":"one"}]`,
		`[{"type":"reasoning.text","index":0,"text":" two"}]`,
		`[{"type":"reasoning.text","index":0,"signature":"late","format":"v1","opaque":{"nested":[1,true]}}]`,
		`[{"type":"reasoning.summary","index":0,"summary":"summary"}]`,
		`[{"type":"reasoning.summary","index":0,"summary":" continued"}]`,
		`[{"type":"reasoning.encrypted","index":0,"data":"A"},{"type":"reasoning.encrypted","index":0,"data":"B"}]`,
		`[{"type":"reasoning.text","index":0,"text":"three"}]`,
		`[{"type":"future.opaque","index":0,"extension":[{"untouched":true}]}]`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, f := range fragments {
			routerDelta(w, map[string]any{"reasoning_details": json.RawMessage(f)}, "")
		}
		routerDelta(w, map[string]any{"content": "done"}, "stop")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	req := &CompletionRequest{Model: "openrouter/m", BaseURL: server.URL, Capabilities: &ModelCapabilities{}}
	final, err := routerCompletion(context.Background(), NewMultiPass(map[string]string{"openrouter": "x"}), req)
	if err != nil || final == nil {
		t.Fatalf("%v %v", final, err)
	}
	if final.Reasoning != "one twosummary continuedthree" {
		t.Fatalf("display: %q", final.Reasoning)
	}
	_, got := openai.OpenRouterReplay(*final, server.URL, "m")
	want := `[{"type":"reasoning.text","index":0,"text":"one two","signature":"late","format":"v1","opaque":{"nested":[1,true]}},{"type":"reasoning.summary","index":0,"summary":"summary continued"},{"type":"reasoning.encrypted","index":0,"data":"A"},{"type":"reasoning.encrypted","index":0,"data":"B"},{"type":"reasoning.text","index":0,"text":"three"},{"type":"future.opaque","index":0,"extension":[{"untouched":true}]}]`
	if !equalJSON(got, []byte(want)) {
		t.Fatalf("blocks: %s", got)
	}
}

func TestOpenRouterCompletedBlocksAndOpaqueNumbers(t *testing.T) {
	// Non-streaming arrays already have logical boundaries, even when adjacent
	// text blocks have no IDs. Null/missing fields and large opaque numbers must
	// survive the wire, SQLite, and a second outgoing request unchanged.
	details := json.RawMessage(`[{"type":"reasoning.text","text":"one","index":0,"opaque":9007199254740993},{"type":"reasoning.text","text":"two","index":1},{"type":"reasoning.text","signature":"only-signature"},{"type":"reasoning.summary","summary":null}]`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"done","reasoning_details":%s},"finish_reason":"stop"}]}`, details)
	}))
	defer server.Close()
	stream := false
	final, err := routerCompletion(context.Background(), NewMultiPass(map[string]string{"openrouter": "x"}), &CompletionRequest{Model: "openrouter/m", BaseURL: server.URL, Capabilities: &ModelCapabilities{}, Stream: &stream})
	if err != nil || final == nil {
		t.Fatalf("response: %v", err)
	}
	loaded := routerSQLiteReload(t, *final)
	_, replay := openai.OpenRouterReplay(loaded, server.URL, "m")
	// Compare normalized bytes, not floats: float64 comparisons would conceal
	// loss of opaque integer precision after database decoding.
	var want, got []map[string]json.RawMessage
	json.Unmarshal(details, &want)
	json.Unmarshal(replay, &got)
	wantRaw, _ := json.Marshal(want)
	gotRaw, _ := json.Marshal(got)
	if string(wantRaw) != string(gotRaw) || loaded.Reasoning != "onetwo" {
		t.Fatalf("completed details changed: %s", replay)
	}
}

func TestOpenRouterMalformedArgumentsAndConcurrentStreams(t *testing.T) {
	var fixtures []struct {
		Sequence  int
		Call      messages.ChatMessageToolCall
		Succeeded bool
	}
	data, err := os.ReadFile("testdata/openrouter_lucid_walrus_arguments.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 8 {
		t.Fatal("missing lucid-walrus examples")
	}
	calls := make([]messages.ChatMessageToolCall, len(fixtures))
	for i, f := range fixtures {
		calls[i] = f.Call
		if f.Succeeded {
			t.Fatal("failure fixture changed")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openai.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		id := req.Messages[0].Content.(string)
		for _, msg := range req.Messages {
			if msg.Role != "assistant" {
				continue
			}
			if len(msg.ToolCalls) != len(calls) {
				t.Error("lost replay calls")
				continue
			}
			for i, call := range msg.ToolCalls {
				if call.Function.Arguments != calls[i].Arguments {
					t.Error("malformed replay changed arguments")
				}
			}
		}
		if !req.Stream {
			wireCalls := make([]openai.ChatToolCall, len(calls))
			for i, c := range calls {
				wireCalls[i] = openai.ChatToolCall{ID: c.ID, Type: "function", Function: openai.ChatToolCallFunc{Name: c.Name, Arguments: c.Arguments}}
			}
			json.NewEncoder(w).Encode(openai.ChatCompletion{ID: id, Provider: id, Choices: []openai.ChatChoice{{Message: openai.ChatResponseMessage{Reasoning: "separate <arg_key>reasoning", Content: "content", ToolCalls: wireCalls}, FinishReason: "tool_calls"}}})
			return
		}
		routerSSE(w, map[string]any{"id": id, "choices": []any{}})
		routerDelta(w, map[string]any{"reasoning": "separate <arg_key>reasoning", "content": "content"}, "")
		for offset := 0; ; offset += 17 {
			active := false
			for i, c := range calls {
				runes := []rune(c.Arguments)
				if offset >= len(runes) {
					continue
				}
				active = true
				fn := map[string]any{"arguments": string(runes[offset:min(offset+17, len(runes))])}
				delta := map[string]any{"index": i, "function": fn}
				if offset == 0 {
					delta["id"], delta["type"], fn["name"] = c.ID, "function", c.Name
				}
				routerDelta(w, map[string]any{"tool_calls": []any{delta}}, "")
			}
			if !active {
				break
			}
		}
		routerDelta(w, map[string]any{}, "tool_calls")
		routerSSE(w, map[string]any{"provider": id, "choices": []any{}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	client := NewMultiPass(map[string]string{"openrouter": "x"})
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			stream := i != 0
			id := fmt.Sprintf("response-%d", i)
			req := &CompletionRequest{Model: "openrouter/m", BaseURL: server.URL, Capabilities: &ModelCapabilities{}, Stream: &stream, Messages: []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: id}}}
			final, err := routerCompletion(context.Background(), client, req)
			if err != nil || final == nil {
				t.Errorf("completion: %v", err)
				return
			}
			if !reflect.DeepEqual(final.ToolCalls, calls) {
				t.Error("malformed arguments were changed")
			}
			if final.Reasoning != "separate <arg_key>reasoning" || final.Content != "content" {
				t.Error("channel mixing")
			}
			meta := final.Metadata["openrouter"].(map[string]any)
			if meta["response_id"] != id || meta["provider"] != id || meta["model"] != nil {
				t.Errorf("cross-response attribution: %v", meta)
			}
			if i == 0 {
				loaded := routerSQLiteReload(t, *final)
				if !reflect.DeepEqual(loaded.ToolCalls, calls) {
					t.Error("SQLite changed malformed arguments")
				}
				req.Messages = append(req.Messages, loaded)
				for _, call := range calls {
					req.Messages = append(req.Messages, messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, Content: "recorded failure", Metadata: map[string]any{"tool_succeeded": false}})
				}
				if _, err := routerCompletion(context.Background(), client, req); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
}

func TestOpenRouterAgentFollowupAndNoticeOnce(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var requests []openai.ChatCompletionRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req openai.ChatCompletionRequest
				json.NewDecoder(r.Body).Decode(&req)
				requests = append(requests, req)
				if req.Reasoning == nil || req.Reasoning.Effort != "low" || req.ReasoningEffort != "" {
					t.Errorf("effective setting: %+v", req.Reasoning)
				}
				response := map[string]any{"content": "done"}
				finish := "stop"
				if len(requests) <= 2 {
					finish = "tool_calls"
					response = map[string]any{"reasoning": "keep me", "tool_calls": []any{map[string]any{"id": fmt.Sprintf("c%d", len(requests)), "index": 0, "type": "function", "function": map[string]any{"name": "reject", "arguments": `{"bad</arg_value>":true}`}}}}
				}
				if req.Stream {
					routerDelta(w, response, finish)
					fmt.Fprint(w, "data: [DONE]\n\n")
				} else {
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": response, "finish_reason": finish}}})
				}
			}))
			defer server.Close()
			tool := &tools.Func{Name: "reject", Run: func(context.Context, tools.Args) (string, error) { return "", fmt.Errorf("validation rejected") }}
			agent := NewAgent(NewMultiPass(map[string]string{"openrouter": "x"}), tools.NewToolRegistry([]tools.Tool{tool}), AgentConfig{MaxIterations: 3})
			defer agent.Close()
			caps := ModelCapabilities{ReasoningMandatory: truth(true), ReasoningEfforts: []string{"max", "high", "low"}, ReasoningEffortsComplete: true}
			req := &CompletionRequest{Model: "openrouter/m", BaseURL: server.URL, Capabilities: &caps, Stream: &stream, Tools: []tools.Tool{tool}, Messages: []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "test"}}}
			var notes []RequestAdaptation
			result, err := agent.Run(context.Background(), req, &AgentCallbacks{OnAdaptation: func(n RequestAdaptation) { notes = append(notes, n) }})
			if err != nil {
				t.Fatal(err)
			}
			if len(notes) != 1 || req.ThinkingEffort != EffortOff() {
				t.Fatalf("notices/preference: %v %+v", notes, req.ThinkingEffort)
			}
			if len(requests) != 3 || requests[1].Messages[1].Reasoning != "keep me" {
				t.Fatal("lost reasoning through tool iteration")
			}
			failures := 0
			for _, m := range result.AllMessages {
				if m.Role == messages.MessageRoleTool {
					failures++
					if m.Metadata["tool_succeeded"] != false {
						t.Fatalf("tool outcome: %v", m.Metadata)
					}
				}
			}
			if failures != 2 {
				t.Fatal("lost tool failures")
			}
		})
	}
}

func TestOpenRouterMissingAttributionEOFAndCancellation(t *testing.T) {
	for _, mode := range []string{"missing", "eof", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				routerDelta(w, map[string]any{"reasoning": "partial"}, "")
				if mode == "cancel" {
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				if mode == "eof" {
					return
				}
				routerDelta(w, map[string]any{"content": "done"}, "stop")
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			final, err := routerCompletion(ctx, NewMultiPass(map[string]string{"openrouter": "x"}), &CompletionRequest{Model: "openrouter/m", BaseURL: server.URL, Capabilities: &ModelCapabilities{}})
			if mode != "missing" {
				// The common processor can expose an unattributed partial
				// message after caller cancellation; Agent.Run checks ctx.Err.
				// It must never receive terminal metadata or replay authority.
				if final != nil && (final.StopReason != "" || final.Metadata["openrouter"] != nil) {
					t.Fatal("partial response acquired completion/replay authority")
				}
				if mode == "eof" && err == nil {
					t.Fatal("premature EOF succeeded")
				}
				return
			}
			if err != nil || final == nil {
				t.Fatalf("%v", err)
			}
			meta := final.Metadata["openrouter"].(map[string]any)
			for _, k := range []string{"response_id", "provider", "model"} {
				if _, ok := meta[k]; ok {
					t.Errorf("invented %s", k)
				}
			}
		})
	}
	endpoint := openai.OpenRouterEndpoint(" HTTPS://user:password@EXAMPLE.COM/api/v1/?token=secret#secret ")
	if endpoint != "https://example.com/api/v1" || strings.Contains(endpoint, "secret") {
		t.Fatalf("credential identity: %q", endpoint)
	}
}
