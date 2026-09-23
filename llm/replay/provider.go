package replay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

var _ contract.LLM = Provider{}

// installed maps a fixture name to the script answering replay/<name>.
var installed sync.Map

// Install makes replay/<name> play turns, signalling through bus. Only a
// fixture loader installs anything, which is what keeps the provider inert
// for every other run.
func Install(name string, turns []Turn, bus *Bus) {
	installed.Store(name, &script{turns: turns, consumed: make([]bool, len(turns)), bus: bus})
}

// Uninstall forgets name.
func Uninstall(name string) { installed.Delete(name) }

func lookup(name string) (*script, bool) {
	v, ok := installed.Load(name)
	if !ok {
		return nil, false
	}
	return v.(*script), true
}

// script is a fixture's turns as they are consumed. Members of a swarm share
// the router, so consumption is serialized and Match is what keys a member's
// turn.
type script struct {
	mu       sync.Mutex
	turns    []Turn
	consumed []bool
	bus      *Bus
}

// take consumes the first unconsumed turn keyed to the last user message,
// or, when none is, the first unconsumed turn with no key: a keyed turn is
// never swallowed by an earlier unkeyed one.
func (s *script) take(lastUser string) (Turn, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, keyed := range []bool{true, false} {
		for i, t := range s.turns {
			if s.consumed[i] || (t.Match != "") != keyed {
				continue
			}
			if keyed && !strings.Contains(lastUser, t.Match) {
				continue
			}
			s.consumed[i] = true
			return t, i, true
		}
	}
	return Turn{}, 0, false
}

// Provider answers replay/<name> requests from the installed script.
type Provider struct{}

// NewProvider returns the provider; every instance reads the same table.
func NewProvider() Provider { return Provider{} }

// ChatCompletionStream plays the next turn. The request's stall and deadline
// budgets are dropped on purpose: a gate is scripted silence, not a stall.
func (Provider) ChatCompletionStream(ctx context.Context, req *contract.CompletionRequest, processor contract.EventStreamProcessor) <-chan *messages.StreamEvent {
	return contract.RunStream(ctx, 0, 0, processor, adapter{}, func(ctx context.Context, core *streaming.StreamingCore) {
		s, ok := lookup(req.Model)
		if !ok {
			core.EmitError(fmt.Errorf("replay/%s: no fixture is loaded for it (a headless run loads one with --shot-fixture)", req.Model))
			return
		}
		s.play(ctx, req, core)
	})
}

func (s *script) play(ctx context.Context, req *contract.CompletionRequest, core *streaming.StreamingCore) {
	turn, index, ok := s.take(lastUserContent(req.Messages))
	if !ok {
		core.EmitError(errors.New("replay: every fixture turn has been played; the scenario asked for one more"))
		return
	}
	if turn.Error != "" {
		core.EmitError(fmt.Errorf("turn %d: %s", index, turn.Error))
		return
	}
	for i, step := range turn.Steps {
		if step.DelayMS > 0 {
			if err := sleep(ctx, time.Duration(step.DelayMS)*time.Millisecond); err != nil {
				core.EmitError(err)
				return
			}
		}
		switch {
		case step.Gate != "":
			if err := s.bus.AwaitRelease(ctx, step.Gate); err != nil {
				core.EmitError(err)
				return
			}
		case step.Reasoning != "":
			core.EmitReasoning(step.Reasoning)
		case step.Content != "":
			core.EmitContent(step.Content)
		case step.Tool != nil:
			core.GetState().AddToolCall(messages.ChatMessageToolCall{
				ID:        fmt.Sprintf("replay_%d_%d", index, i),
				Name:      step.Tool.Name,
				Arguments: step.Tool.arguments(),
			})
		}
		if step.Mark != "" {
			s.bus.Reach(step.Mark)
		}
	}
	if turn.Usage != nil {
		core.SetTokenUsage(turn.Usage.Input, turn.Usage.Output)
		if turn.Usage.Cost != nil {
			core.SetReportedCost(*turn.Usage.Cost)
		}
	}
	stop := turn.Stop
	if stop == "" {
		stop = messages.StopReasonEndTurn
	}
	core.SetStopReason(stop)
	core.Complete()
	if turn.Mark != "" {
		s.bus.Reach(turn.Mark)
	}
}

func lastUserContent(history []messages.ChatMessage) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == messages.MessageRoleUser {
			return history[i].GetContent()
		}
	}
	return ""
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// adapter is the no-op stream adapter: there are no wire chunks to process
// and nothing to add to the final message.
type adapter struct{}

func (adapter) ProcessChunk(any, streaming.StreamStateInterface) error                   { return nil }
func (adapter) EnrichFinalMessage(*messages.ChatMessage, streaming.StreamStateInterface) {}

// ListModels advertises every installed fixture as a tool-capable reasoning
// model with a large window, so request preparation adapts nothing away.
func ListModels(_ context.Context, _ *http.Client, t contract.ModelTarget) (contract.ModelCatalog, error) {
	yes := true
	window, output := 200000, 32000
	info := func(name string) contract.ModelInfo {
		return contract.ModelInfo{
			ID:   name,
			Name: "replay " + name,
			ModelCapabilities: contract.ModelCapabilities{
				Chat:             &yes,
				Tools:            &yes,
				Reasoning:        &yes,
				InputModalities:  []string{"text"},
				OutputModalities: []string{"text"},
				ContextTokens:    &window,
				OutputTokens:     &output,
			},
		}
	}
	var catalog contract.ModelCatalog
	if t.Model != "" {
		if _, ok := lookup(t.Model); !ok {
			return catalog, contract.ErrModelMetadataUnknown
		}
		catalog.Models = append(catalog.Models, info(t.Model))
		return catalog, nil
	}
	installed.Range(func(key, _ any) bool {
		catalog.Models = append(catalog.Models, info(key.(string)))
		return true
	})
	return catalog, nil
}
