package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/openrouter"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestOpenRouterStreamingKeepsSignedAndIndexedBlocksSeparate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		deltas []string
		want   string
	}{
		{"signed", []string{`[{"type":"reasoning.text","text":"one","index":0}]`, `[{"type":"reasoning.text","signature":"first","index":0}]`, `[{"type":"reasoning.text","text":"two","index":0}]`, `[{"type":"reasoning.text","signature":"second","index":0}]`}, `[{"type":"reasoning.text","text":"one","signature":"first","index":0},{"type":"reasoning.text","text":"two","signature":"second","index":0}]`},
		{"indexed", []string{`[{"type":"reasoning.text","text":"one","index":0}]`, `[{"type":"reasoning.text","text":"two","index":1}]`}, `[{"type":"reasoning.text","text":"one","index":0},{"type":"reasoning.text","text":"two","index":1}]`},
		{"same chunk", []string{`[{"type":"reasoning.text","text":"one"},{"type":"reasoning.text","text":"two"}]`}, `[{"type":"reasoning.text","text":"one"},{"type":"reasoning.text","text":"two"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for _, delta := range tc.deltas {
					routerDelta(w, map[string]any{"reasoning_details": json.RawMessage(delta)}, "")
				}
				routerDelta(w, map[string]any{"content": "done"}, "stop")
			}))
			defer server.Close()
			final, err := routerCompletion(context.Background(), NewMultiPass(map[string]string{"openrouter": "fixture"}), &CompletionRequest{Model: "openrouter/m", BaseURL: server.URL, Capabilities: &ModelCapabilities{}})
			if err != nil || final == nil {
				t.Fatalf("response: %+v %v", final, err)
			}
			loaded := routerSQLiteReload(t, *final)
			_, replay := openrouter.Replay(loaded, server.URL, "m")
			if !equalJSON(replay, []byte(tc.want)) {
				t.Fatalf("reasoning blocks changed: %s", replay)
			}
		})
	}
}

func TestOpenRouterTruncatedReasoningCannotReplayAfterReload(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				response := map[string]any{"content": "partial answer", "reasoning": "partial thinking", "reasoning_details": json.RawMessage(`[{"type":"reasoning.text","text":"unfinished","signature":"partial"}]`)}
				if stream {
					routerDelta(w, response, "length")
				} else {
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": response, "finish_reason": "length"}}})
				}
			}))
			defer server.Close()
			final, err := routerCompletion(context.Background(), NewMultiPass(map[string]string{"openrouter": "fixture"}), &CompletionRequest{Model: "openrouter/m", BaseURL: server.URL, Stream: &stream, Capabilities: &ModelCapabilities{}})
			if err != nil || final == nil || final.StopReason != messages.StopReasonMaxTokens {
				t.Fatalf("truncated response: %+v %v", final, err)
			}
			loaded := routerSQLiteReload(t, *final)
			meta := loaded.Metadata["openrouter"].(map[string]any)
			if meta["incomplete"] != true {
				t.Fatal("truncation marker was not persisted")
			}
			for _, legacy := range []bool{false, true} {
				if legacy {
					delete(meta, "incomplete")
				}
				plain, details := openrouter.Replay(loaded, server.URL, "m")
				if plain != "" || details != nil {
					t.Fatal("truncated reasoning acquired replay authority")
				}
			}
		})
	}
}

func TestOpenRouterSavedEffortAdaptsAfterModelSwitch(t *testing.T) {
	req := &CompletionRequest{Model: "openrouter/first", ThinkingEffort: EffortLevel(LevelHigh)}
	first := ModelCapabilities{ReasoningEfforts: []string{"high"}, ReasoningEffortsComplete: true}
	if _, _, err := PrepareCapabilities(req, first, false); err != nil {
		t.Fatal(err)
	}
	req.Model = "openrouter/second"
	second := ModelCapabilities{ReasoningEfforts: []string{"low"}, ReasoningEffortsComplete: true}
	out, notes, err := PrepareCapabilities(req, second, false)
	if err != nil || ResolveOpenRouterRequestThinking(out.ThinkingEffort, second).Request != nil || len(notes) != 1 || !strings.Contains(notes[0].Message, "provider default") || req.ThinkingEffort.String() != "high" {
		t.Fatalf("saved preference blocked the new model: %+v %+v %v", out, notes, err)
	}
	if _, err := ResolveOpenRouterThinking(req.ThinkingEffort, second); err == nil {
		t.Fatal("explicit setting validation lost its unsupported-effort check")
	}
}
