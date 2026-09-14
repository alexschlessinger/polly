package llm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

// Explicitly opt in: at most three 4096-token generations per mode, using only
// an in-process echo tool. Never execute model-provided commands in this test.
func TestOpenRouterLiveToolRoundTrip(t *testing.T) {
	if os.Getenv("POLLYTOOL_OPENROUTER_LIVE_TEST") != "1" {
		t.Skip("set POLLYTOOL_OPENROUTER_LIVE_TEST=1 for the bounded GLM smoke test")
	}
	key := os.Getenv("POLLYTOOL_OPENROUTERKEY")
	if key == "" {
		t.Skip("POLLYTOOL_OPENROUTERKEY is unavailable")
	}
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			tool := &tools.Func{Name: "echo_probe", Desc: "Return the probe value unchanged.", Params: schema.Params{"value": schema.S("Probe value")}, Required: []string{"value"}, Run: func(_ context.Context, args tools.Args) (string, error) { return args.String("value"), nil }}
			agent := NewAgent(NewMultiPass(map[string]string{"openrouter": key}), tools.NewToolRegistry([]tools.Tool{tool}), AgentConfig{MaxIterations: 3})
			defer agent.Close()
			req := &CompletionRequest{Model: "openrouter/z-ai/glm-5.3-flash", Stream: &stream, MaxTokens: 4096, MaxContextTokens: 16000, Deadline: 30 * time.Second, ThinkingEffort: EffortOff(), Messages: []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "Calculate 179 * 23 + 41. Call echo_probe exactly once with value set to the decimal result. Read its result, then answer only: probe complete."}}}
			result, err := agent.Run(ctx, req, &AgentCallbacks{OnAdaptation: func(n RequestAdaptation) { t.Log(n.Message) }})
			if err != nil {
				t.Fatalf("live OpenRouter: %v", err)
			}
			if req.ThinkingEffort != EffortOff() {
				t.Fatal("saved off changed")
			}
			cfg := sessions.StoreConfig{Mode: sessions.ModeDisk, Path: filepath.Join(t.TempDir(), "live.db")}
			store, err := sessions.OpenStore(cfg)
			if err != nil {
				t.Fatal(err)
			}
			session, err := store.Acquire(ctx, "glm-probe", sessions.AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := session.AddMessages(ctx, result.AllMessages); err != nil {
				t.Fatal(err)
			}
			session.Close()
			store.Close()
			store, err = sessions.OpenStore(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			session, err = store.Acquire(ctx, "glm-probe", sessions.AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			history, err := session.GetHistory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			missingReasoning := false
			for _, m := range history {
				if m.Role != messages.MessageRoleAssistant {
					continue
				}
				meta, ok := m.Metadata["openrouter"].(map[string]any)
				if !ok {
					t.Fatal("missing persisted OpenRouter metadata")
				}
				plain, details := openai.OpenRouterReplay(m, openai.OpenRouterEndpoint(""), "z-ai/glm-5.3-flash")
				t.Logf("persisted response_id=%v model=%v provider=%v endpoint=%v requested_model=%v reasoning_bytes=%d details_bytes=%d tool_calls=%d", meta["response_id"], meta["model"], meta["provider"], meta["endpoint"], meta["requested_model"], len(m.Reasoning), len(details), len(m.ToolCalls))
				if len(m.ToolCalls) > 0 && plain == "" && details == nil {
					missingReasoning = true
				}
				calls += len(m.ToolCalls)
			}
			if calls != 1 || result.IterationCount != 2 {
				t.Fatalf("round-trip: %d calls, %d iterations", calls, result.IterationCount)
			}
			if missingReasoning {
				t.Skip("tool round-trip and SQLite attribution verified; serving provider emitted no reasoning on the tool turn, so live reasoning replay could not be checked")
			}
		})
	}
}
