package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func replCompactCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) != 1 {
		return replCommandResult{err: ctx.replyLine("usage: /compact")}
	}
	if ctx == nil || ctx.state == nil || ctx.state.session == nil || ctx.state.agent == nil {
		return replCommandResult{err: ctx.replyLine("compaction unavailable: no active session")}
	}
	if ctx.compactConversation == nil {
		return replCommandResult{err: ctx.replyLine("compaction unavailable here")}
	}
	if err := ctx.compactConversation(); err != nil {
		return replCommandResult{err: ctx.replyLine("compaction failed: " + err.Error())}
	}
	return replCommandResult{}
}

func (r *managedREPL) submitCompactTurnLocked() error {
	return r.submitManagedTurnLocked(managedTurnInput{displayText: "/compact", compact: true})
}

// executeCompaction uses the normal cancellable turn UI but adds no user
// message, executes no tools, and never continues the agent's work. Only an
// accepted summary marker changes future context; the saved transcript stays.
func executeCompaction(ctx context.Context, config *Config, state *conversationState, ui TurnUI) (finalErr error) {
	if state == nil || state.session == nil || state.agent == nil {
		return errors.New("no active session")
	}
	if err := state.waitWorkspaceChanges(ctx); err != nil {
		return err
	}
	if config == nil {
		config = &Config{}
	}
	t := &turnExecution{ctx: ctx, config: config, state: state, settings: &state.settings, turnUI: ui}
	ui.Start()
	defer ui.Stop()
	started := time.Now()
	defer func() {
		in, out, _ := t.usage.tokens()
		state.spend.finishTurn(t.usage.cost(), in+out > 0)
		ui.CompleteTurn(turnCompletion{Err: finalErr, Elapsed: time.Since(started), ProgressSaved: finalErr == nil})
	}()
	history, err := state.session.GetHistory(ctx)
	if err != nil {
		return fmt.Errorf("read session history: %w", err)
	}
	history, warnings, err := t.applyContracts(history)
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		ui.AppendWarning(warning)
	}
	req, err := t.completionRequest(history)
	if err != nil {
		return err
	}
	response, runErr := state.agent.Compact(ctx, req, &llm.AgentCallbacks{
		OnAdaptation: func(note llm.RequestAdaptation) {
			if note.Feature == llm.FeatureCompaction {
				ui.AppendNotice(note.Message)
			} else {
				ui.AppendWarning(note.Message)
			}
		},
		OnCompactionUsage: func(model string, usage llm.UsageUpdate) {
			t.usage.compacted(model, usage)
			t.pushUsage()
		},
	})
	if response != nil {
		t.usage.settle(response.TokenUsage())
		// Normal turns show peak input excluding summary calls; here the
		// summary calls are the entire operation.
		for _, msg := range response.AllMessages {
			t.usage.peakIn = max(t.usage.peakIn, msg.GetInputTokens())
		}
		t.pushUsage()
	}
	if ctx.Err() != nil {
		runErr = context.Cause(ctx)
	}
	if response == nil {
		if runErr != nil {
			return runErr
		}
		return errors.New("compaction returned no result")
	}
	if !ui.TurnPersistenceAllowed() {
		runErr = errors.Join(runErr, errors.New("compaction turn detached; not saved"))
	}
	// Even a rejected summary was billed. Keep its usage on failure, but
	// never apply a marker after cancellation or a failed compaction.
	var marker *messages.Compaction
	summaryModel := ""
	var usage []messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.IsUsageRecord() {
			usage = append(usage, msg)
			summaryModel = msg.UsageModel()
		}
		if c, ok := msg.Compaction(); ok && runErr == nil {
			marker = &c
		}
	}
	saved := false
	if marker != nil {
		// The live context fences this transaction against detachment. A
		// delayed covering marker must never land after newer conversation.
		if err := state.session.AddMessages(ctx, response.AllMessages); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("save compaction to this session: %w", err))
		} else {
			saved = true
		}
	}
	if !saved && len(usage) > 0 {
		// Usage is context-invisible and remains safe to append after a
		// canceled or detached turn. Never retry the summary marker here.
		if err := state.session.AddMessages(context.WithoutCancel(ctx), usage); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("save compaction usage: %w", err))
		}
	}
	if ctx.Err() != nil {
		runErr = errors.Join(runErr, context.Cause(ctx))
	}
	if runErr != nil {
		return runErr
	}
	if marker == nil {
		ui.AppendNotice("Nothing to compact · session context unchanged")
		return nil
	}
	stats := response.Projection
	budget := contextBudgetDetails{window: stats.Window, input: stats.Budget, response: stats.MaxTokens, learned: stats.Learned}
	state.recordBudget(t.settings, budget)
	ui.RecordContextUsage(stats.CountedTokens, !stats.Counted, budget)
	ui.AppendNotice(llm.CompactionNote(*marker, summaryModel) + " · saved to this session · original transcript retained")
	return nil
}
