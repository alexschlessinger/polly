package main

import (
	"context"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

// contextStats describes what the session's requests carry: its history as
// the agent sends it, compaction markers applied, with the same generated
// system guidance as a request; how often it compacted; the stored
// transcript beside it; and its cache rate. It does not project, hydrate
// media, or persist.
func contextStats(ctx context.Context, config *Config, state *conversationState, history []messages.ChatMessage) ([]string, error) {
	var cache turnCacheUsage
	summaries, clears, stored := 0, 0, 0
	for _, msg := range history {
		cache.add(msg)
		stored += sessions.EstimateTokens(msg)
		if c, ok := msg.Compaction(); ok {
			if c.Summary != "" {
				summaries++
			} else {
				clears++
			}
		}
	}
	view := llm.RequestView(history, state.viewTools())
	var err error
	if config == nil || config.SchemaPath == "" {
		view, _, err = composeSessionContracts(ctx, state, &state.settings, view)
		if err != nil {
			return nil, err
		}
	}
	req := llm.CompletionRequest{Messages: view, Skills: state.skillCatalog}
	view, err = llm.WithSandboxContext(req.ResolvedMessages(), state.toolRegistry)
	if err != nil {
		return nil, err
	}
	counts, tokens := map[string]int{}, map[string]int{}
	for _, msg := range view {
		counts[msg.Role]++
		tokens[msg.Role] += llm.EstimateMessageTokens(msg)
	}
	details := []string{"requests carry:"}
	for _, role := range []string{messages.MessageRoleUser, messages.MessageRoleAssistant, messages.MessageRoleTool, messages.MessageRoleSystem} {
		details = append(details, fmt.Sprintf("%-10s %d · ~%s tokens", role, counts[role], humanizeTokens(tokens[role])))
	}
	details = append(details,
		fmt.Sprintf("compactions: %d %s · %d %s", summaries, pluralWord(summaries, "summary", "summaries"), clears, pluralWord(clears, "clear", "clears")),
		fmt.Sprintf("transcript: %d %s · ~%s tokens stored", len(history), pluralWord(len(history), "msg", "msgs"), humanizeTokens(stored)))
	rate := "unknown"
	if field, ok := turnCacheField(cache); ok {
		rate = field.raw
	}
	return append(details, "", "session cache: "+rate), nil
}

// contextBudgetDetails is how a request is sized: the model's window, the
// input budget and the output limit, and whether a provider's rejection set
// the budget.
type contextBudgetDetails struct {
	window, input, response int
	learned                 bool
}

// details are the rows that say how the next request is sized, where
// compaction starts, and which model summarizes.
func (b *contextBudgetDetails) details(compactModel string) []string {
	if b == nil {
		return nil
	}
	var rows []string
	if b.window > 0 {
		rows = append(rows, "window: "+humanizeTokens(b.window))
	}
	if b.input > 0 {
		budget := humanizeTokens(b.input)
		if b.learned {
			budget += " (provider limit)"
		}
		rows = append(rows, "input budget: "+budget,
			fmt.Sprintf("compacts at: %s (%d%%)", humanizeTokens(llm.CompactionPoint(b.input)), llm.CompactionPoint(100)))
	} else {
		rows = append(rows, "input budget: unlimited", "compacts: on a provider rejection")
	}
	output := "provider default"
	if b.response > 0 {
		output = humanizeTokens(b.response)
	}
	model := "session model"
	if compactModel != "" {
		model = llm.ModelName(compactModel)
	}
	return append(rows, "output limit: "+output, "compaction model: "+model, "")
}

// requestBudget is how a request on a model with caps is sized, as the
// agent clamps it.
func (s *Settings) requestBudget(caps llm.ModelCapabilities) contextBudgetDetails {
	window := caps.ContextWindow()
	req := &llm.CompletionRequest{Model: s.Model, MaxTokens: s.MaxTokens, MaxContextTokens: s.contextLimit(window)}
	if prepared, _, err := llm.PrepareCapabilities(req, caps, false); err == nil {
		req = prepared
	}
	return contextBudgetDetails{window: window, input: req.MaxContextTokens, response: req.MaxTokens}
}

// sizedBudget is how a request was sized, and under which settings (see
// budgetKey).
type sizedBudget struct {
	key    string
	budget contextBudgetDetails
}

// budgetKey names the settings that decide how requests are sized.
func budgetKey(s *Settings) string {
	return fmt.Sprint(s.Model, "\x00", s.ModelHost, "\x00", s.AutoMaxContext, s.MaxHistoryTokens, "\x00", s.MaxTokens)
}

// recordBudget keeps how a request under settings was sized.
func (s *conversationState) recordBudget(settings *Settings, budget contextBudgetDetails) {
	if s == nil || settings == nil {
		return
	}
	s.sized.Store(&sizedBudget{key: budgetKey(settings), budget: budget})
}

// currentBudget is how the session's next request under settings is sized:
// as its last one was, which carries what a provider's rejection taught,
// when settings have not changed since; else from model metadata already
// cached. It makes no network request, so the UI may ask on its goroutine,
// and is nil when nothing is known.
func (s *conversationState) currentBudget(settings *Settings) *contextBudgetDetails {
	if s == nil || settings == nil {
		return nil
	}
	if sized := s.sized.Load(); sized != nil && sized.key == budgetKey(settings) {
		budget := sized.budget
		return &budget
	}
	if s.agent == nil || settings.Model == "" {
		return nil
	}
	target := modelMetadataTarget(settings.Model, settings.ModelHost, s.metadataBaseURL)
	info := s.agent.CachedModelInfo(target)
	if info == nil {
		return nil
	}
	budget := settings.requestBudget(info.EffectiveCapabilities(llm.RouteHost(target)))
	return &budget
}

// viewTools are the tools the session's requests offer, which decide how
// compaction markers leave history in them.
func (s *conversationState) viewTools() []tools.Tool {
	if registry := s.effectiveTools(); registry != nil {
		return registry.All()
	}
	return nil
}

// estimatedRequestSize estimates the messages a request over history
// carries, as an agent with tools sends them.
func estimatedRequestSize(history []messages.ChatMessage, tools []tools.Tool) int {
	size := 0
	for _, msg := range llm.RequestView(history, tools) {
		size += llm.EstimateMessageTokens(msg)
	}
	return size
}

// lastCountedSize is about the size of the conversation's next request in
// the provider's count: what the last reply's request came to, and the
// reply itself. ok is false unless the last reply reported a count and
// nothing a request would carry differently followed it: input, tool
// results, a compaction.
func lastCountedSize(history []messages.ChatMessage) (int, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		if _, compaction := msg.Compaction(); msg.Role == messages.MessageRoleInternal && !compaction {
			continue
		}
		if in := msg.GetInputTokens(); msg.Role == messages.MessageRoleAssistant && in > 0 {
			return in + msg.GetOutputTokens(), true
		}
		return 0, false
	}
	return 0, false
}
