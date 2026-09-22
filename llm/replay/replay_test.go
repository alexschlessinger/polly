package replay

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/messages"
)

// turns decodes a JSON array of turns and validates it.
func turns(t *testing.T, data string) []Turn {
	t.Helper()
	var out []Turn
	if err := json.Unmarshal([]byte(data), &out); err != nil {
		t.Fatal(err)
	}
	if err := Validate(out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestValidateRejectsMistakesBeforePlay(t *testing.T) {
	for _, tc := range []struct {
		name, data, want string
	}{
		{"two kinds in one step", `[{"steps":[{"content":"x","gate":"g"}]}]`, "exactly one"},
		{"empty step", `[{"steps":[{"mark":"m"}]}]`, "exactly one"},
		{"error with steps", `[{"error":"boom","steps":[{"content":"x"}]}]`, "error turn has no steps"},
		{"unknown stop", `[{"stop":"whenever"}]`, "unknown stop"},
		{"nameless tool", `[{"steps":[{"tool":{"arguments":{}}}]}]`, "tool needs a name"},
		{"negative delay", `[{"steps":[{"content":"x","delay_ms":-1}]}]`, "negative delay"},
	} {
		var out []Turn
		if err := json.Unmarshal([]byte(tc.data), &out); err != nil {
			t.Fatal(err)
		}
		err := Validate(out)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestTurnsReportGatesMarksAndArguments(t *testing.T) {
	ts := turns(t, `[
		{"steps": [{"reasoning": "r", "mark": "thought"}, {"gate": "go"}, {"tool": {"name": "bash", "arguments": {"command": "true"}}}], "mark": "done"},
		{"steps": [{"tool": {"name": "bash", "arguments": "{\"command\":\"false\"}"}}]}
	]`)
	if !Gates(ts)["go"] || len(Gates(ts)) != 1 {
		t.Fatalf("gates = %v", Gates(ts))
	}
	if marks := Marks(ts); !marks["thought"] || !marks["done"] || len(marks) != 2 {
		t.Fatalf("marks = %v", marks)
	}
	if got := ts[0].Steps[2].Tool.arguments(); got != `{"command": "true"}` {
		t.Fatalf("object arguments = %q", got)
	}
	if got := ts[1].Steps[0].Tool.arguments(); got != `{"command":"false"}` {
		t.Fatalf("string arguments = %q", got)
	}
}

func TestBusLatchesInEitherOrder(t *testing.T) {
	bus := NewBus()
	bus.Release("early")
	bus.Reach("seen")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := bus.AwaitRelease(ctx, "early"); err != nil {
		t.Fatalf("a release before the wait was lost: %v", err)
	}
	if err := bus.AwaitMark(ctx, "seen"); err != nil {
		t.Fatalf("a mark before the wait was lost: %v", err)
	}
	blocked, blockedCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer blockedCancel()
	if err := bus.AwaitRelease(blocked, "never"); err == nil {
		t.Fatal("an unreleased gate did not block")
	}
	bus.Release("early") // a second release is harmless
}

// collect plays one request against replay/<name> and returns the events.
func collect(t *testing.T, name string, history []messages.ChatMessage) []*messages.StreamEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := &contract.CompletionRequest{Model: name, Messages: history, Timeout: time.Millisecond}
	var events []*messages.StreamEvent
	for ev := range NewProvider().ChatCompletionStream(ctx, req, messages.NewStreamProcessor()) {
		events = append(events, ev)
	}
	return events
}

func TestProviderRefusesAModelNoFixtureWasInstalledFor(t *testing.T) {
	events := collect(t, "nobody", messages.User("hi"))
	last := events[len(events)-1]
	if last.Type != messages.EventTypeError || !strings.Contains(last.Error.Error(), "--shot-fixture") {
		t.Fatalf("events = %+v", events)
	}
}

func TestProviderPlaysTurnsByMatchThenInOrder(t *testing.T) {
	f := turns(t, `[
		{"steps": [{"content": "first"}], "usage": {"input": 10, "output": 2}},
		{"match": "special", "steps": [{"content": "matched"}, {"tool": {"name": "bash", "arguments": {"command": "true"}}}]},
		{"error": "boom"}
	]`)
	Install("order", f, NewBus())
	defer Uninstall("order")

	events := collect(t, "order", messages.User("something special"))
	done := events[len(events)-1]
	if done.Type != messages.EventTypeComplete || done.Message.Content != "matched" {
		t.Fatalf("matched turn: %+v", done)
	}
	if calls := done.Message.ToolCalls; len(calls) != 1 || calls[0].Name != "bash" || calls[0].Arguments != `{"command": "true"}` {
		t.Fatalf("tool calls = %+v", calls)
	}
	if done.Message.StopReason != messages.StopReasonToolUse {
		t.Fatalf("stop = %q, want tool_use for a turn with calls", done.Message.StopReason)
	}

	events = collect(t, "order", messages.User("plain"))
	done = events[len(events)-1]
	if done.Type != messages.EventTypeComplete || done.Message.Content != "first" {
		t.Fatalf("first unconsumed turn: %+v", done)
	}
	if in, out := done.Message.GetInputTokens(), done.Message.GetOutputTokens(); in != 10 || out != 2 {
		t.Fatalf("usage = %d/%d", in, out)
	}

	events = collect(t, "order", messages.User("again"))
	done = events[len(events)-1]
	if done.Type != messages.EventTypeError || !strings.Contains(done.Error.Error(), "boom") {
		t.Fatalf("error turn: %+v", done)
	}

	events = collect(t, "order", messages.User("one more"))
	done = events[len(events)-1]
	if done.Type != messages.EventTypeError || !strings.Contains(done.Error.Error(), "every fixture turn has been played") {
		t.Fatalf("exhausted: %+v", done)
	}
}

func TestProviderHoldsAtAGateAndReportsMarks(t *testing.T) {
	f := turns(t, `[{"steps": [
		{"content": "before", "mark": "half"},
		{"gate": "go"},
		{"content": " after"}
	], "mark": "done"}]`)
	bus := NewBus()
	Install("gated", f, bus)
	defer Uninstall("gated")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := &contract.CompletionRequest{Model: "gated", Messages: messages.User("go")}
	events := NewProvider().ChatCompletionStream(ctx, req, messages.NewStreamProcessor())

	if err := bus.AwaitMark(ctx, "half"); err != nil {
		t.Fatal(err)
	}
	first := <-events
	if first.Type != messages.EventTypeContent || first.Content != "before" {
		t.Fatalf("first event = %+v", first)
	}
	select {
	case ev := <-events:
		t.Fatalf("the gate did not hold: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
	held, heldCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	if err := bus.AwaitMark(held, "done"); err == nil {
		t.Fatal("the turn's mark was reported before the gate opened")
	}
	heldCancel()

	bus.Release("go")
	var last *messages.StreamEvent
	for ev := range events {
		last = ev
	}
	if last.Type != messages.EventTypeComplete || last.Message.Content != "before after" {
		t.Fatalf("last event = %+v", last)
	}
	if err := bus.AwaitMark(ctx, "done"); err != nil {
		t.Fatal(err)
	}
}

func TestProviderStopsAtAGateWhenTheRequestIsCancelled(t *testing.T) {
	f := turns(t, `[{"steps": [{"gate": "never"}]}]`)
	Install("cancelled", f, NewBus())
	defer Uninstall("cancelled")
	ctx, cancel := context.WithCancelCause(context.Background())
	req := &contract.CompletionRequest{Model: "cancelled", Messages: messages.User("go")}
	events := NewProvider().ChatCompletionStream(ctx, req, messages.NewStreamProcessor())
	cancel(errors.New("interrupted"))
	// A cancelled stream ends like any provider's: the channel closes, with
	// no completion fabricated from the partial state.
	for ev := range events {
		if ev.Type == messages.EventTypeComplete {
			t.Fatalf("a cancelled gate completed: %+v", ev.Message)
		}
	}
}

func TestListModelsAdvertisesInstalledFixtures(t *testing.T) {
	f := turns(t, `[{"steps": [{"content": "x"}]}]`)
	Install("listed", f, NewBus())
	defer Uninstall("listed")
	catalog, err := ListModels(context.Background(), nil, contract.ModelTarget{Provider: "replay", Model: "listed"})
	if err != nil || len(catalog.Models) != 1 || catalog.Models[0].ID != "listed" {
		t.Fatalf("catalog = %+v, err = %v", catalog, err)
	}
	if caps := catalog.Models[0].ModelCapabilities; caps.Tools == nil || !*caps.Tools || caps.ContextWindow() == 0 {
		t.Fatalf("capabilities = %+v", caps)
	}
	if _, err := ListModels(context.Background(), nil, contract.ModelTarget{Provider: "replay", Model: "absent"}); !errors.Is(err, contract.ErrModelMetadataUnknown) {
		t.Fatalf("absent model err = %v", err)
	}
}
