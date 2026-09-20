package main

import (
	"fmt"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

// contextMessageStats describes the full session, with the same generated
// system guidance as a request. It does not project, hydrate media, or persist.
func (r *managedREPL) contextMessageStats() ([]string, error) {
	state := r.state
	ctx := state.session.Context()
	history, err := state.session.GetHistory(ctx)
	if err != nil {
		return nil, err
	}
	var cache turnCacheUsage
	for _, msg := range history {
		cache.add(msg)
	}
	view := append([]messages.ChatMessage(nil), history...)
	if r.config == nil || r.config.SchemaPath == "" {
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
	var details []string
	for _, role := range []string{"user", "assistant", "tool", "system"} {
		details = append(details, fmt.Sprintf("%-10s %d · ~%s tokens", role, counts[role], humanizeTokens(tokens[role])))
	}
	rate := "unknown"
	if field, ok := turnCacheField(cache); ok {
		rate = field.raw
	}
	details = append(details, "", "session cache: "+rate)
	return details, nil
}

// contextBudgetDetails snapshots request sizing without another metadata lookup.
type contextBudgetDetails struct {
	window, input, response int
}

func (b *contextBudgetDetails) details() []string {
	if b == nil {
		return nil
	}
	if b.input <= 0 {
		return []string{"input budget: unlimited", ""}
	}
	details := []string{"input budget: " + humanizeTokens(b.input)}
	if b.window > 0 {
		// The half-window floor caps the total reserve for small windows.
		reserve := b.window - llm.ClampContextBudget(b.window, b.window, b.response)
		safety := min(b.window/10, reserve)
		details = append(details, "response reserve: "+humanizeTokens(reserve-safety), "safety margin: "+humanizeTokens(safety))
	}
	return append(details, "")
}
