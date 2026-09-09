package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

// turnExecution carries one turn's state through its phases: request
// preparation, the agent run, persistence, and output. Each phase is a method
// that reads on its own; executeTurnWithUserMessage sequences them.
type turnExecution struct {
	ctx      context.Context
	config   *Config
	state    *conversationState
	settings *Settings
	schema   *llm.Schema
	turnUI   TurnUI

	userMsg   messages.ChatMessage
	reuseUser bool
	// reusingPersistedUser is set when the unchanged restored draft already
	// at the end of history stands in for userMsg, so nothing is persisted
	// for it again.
	reusingPersistedUser bool
	// persistAttempted records that the run's first projection reached the
	// user-message persistence hook, whether or not the write succeeded.
	persistAttempted bool
	// reportIDs are the child reports the managed REPL folded into this
	// prompt; they are marked read when the user message persists.
	reportIDs []int64

	// settledOutput is the non-interactive one-shot without --stream: the
	// final answer prints once instead of streaming.
	settledOutput bool
	stats         turnToolStats
	usage         turnUsage
}

// prepareRequest resolves the user message against the session and builds
// the provider-visible history, with the display and repository contracts
// applied for free-text turns. It returns the warnings raised while loading
// repository instructions, for the turn UI to show once it starts.
func (t *turnExecution) prepareRequest() ([]messages.ChatMessage, []string, error) {
	// An unchanged restored draft must reuse the representation already
	// persisted. If a prior storage failure left prepared bytes inline and
	// the store later recovers, rewriting only the restored candidate to an
	// artifact would make it look like a different user turn and persist a
	// duplicate.
	history, err := t.state.session.GetHistory(t.ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read session history: %w", err)
	}
	t.reusingPersistedUser = t.reuseUser && historyEndsWithEquivalentUserMessage(history, t.userMsg)
	if !t.reusingPersistedUser {
		t.userMsg, err = externalizeMessageImages(t.ctx, t.userMsg, t.state.artifactStore)
		if err != nil {
			return nil, nil, fmt.Errorf("persist input artifacts: %w", err)
		}
	}
	requestMessages, err := prepareSessionImageRequest(history, t.userMsg, t.reuseUser)
	if err != nil {
		return nil, nil, err
	}
	// Structured output is machine-facing: display guidance is irrelevant
	// there, "plain text only" could fight the schema on providers whose
	// structured output is prompt-based, and the context-mechanics guidance
	// (put findings in replies) is moot when the reply is a schema payload.
	var warnings []string
	if t.schema == nil {
		if requestMessages, warnings, err = t.applyContracts(requestMessages); err != nil {
			return nil, nil, err
		}
	}
	// Image references that resolve to nothing (or ambiguously) are rejected
	// before anything is persisted; the complete request, after clamping the
	// budget and applying deterministic reductions, is judged by the run's
	// first projection.
	if err := llm.ValidateImageProjection(requestMessages); err != nil {
		return nil, nil, err
	}
	return requestMessages, warnings, nil
}

// applyContracts composes the system contract for a free-text turn: display
// guidance, the coding contract and repository instructions when no custom
// system prompt replaces them, and session title guidance.
func (t *turnExecution) applyContracts(requestMessages []messages.ChatMessage) ([]messages.ChatMessage, []string, error) {
	contract := sendTimeContracts(t.state.displayContract)
	titleGuidance, err := sessionTitleGuidance(t.ctx, t.state)
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	if t.settings.SystemPrompt == "" {
		instructions, changed := loadRepositoryInstructions(t.state.toolRegistry)
		warnings = t.state.changedInstructionWarnings(changed)
		contract = codingContract + "\n\n" + contract + "\n\n" + instructions
	}
	if titleGuidance != "" {
		contract += "\n\n" + titleGuidance
	}
	return applyDisplayContract(requestMessages, contract), warnings, nil
}

// persistUser is the run's first-projection hook. The user message is
// persisted once the projection shows the request can be sent, before any
// provider tokens are spent. Earlier would make a deterministic projection
// failure permanent for exact retries of a persisted-then-unsendable message;
// later would lose the input when the call fails. A broken session store
// (disk full, lost lease) therefore fails the turn before the call, whose
// result could not be saved either.
func (t *turnExecution) persistUser(llm.ProjectionStats) error {
	t.persistAttempted = true
	t.turnUI.UserMessagePersistenceStarted()
	err := persistUserMessageForTurn(t.ctx, t.state.session, t.userMsg, t.reuseUser, t.reportIDs)
	t.turnUI.UserMessagePersistenceFinished(err == nil)
	if err != nil {
		return fmt.Errorf("failed to persist user message: %w", err)
	}
	return nil
}

// callbacks wires the agent run to the turn UI, tool statistics, and usage
// bookkeeping.
func (t *turnExecution) callbacks(req *llm.CompletionRequest) *llm.AgentCallbacks {
	turnUI, config := t.turnUI, t.config
	// trimLeadingNL strips leading newlines from the next content burst.
	// Armed only after a reasoning event fires — models with thinking enabled
	// commonly emit a leading "\n\n" to visually separate the (hidden)
	// reasoning from the reply. We strip only \n/\r so leading spaces/tabs
	// (e.g. code-block indentation) are preserved.
	trimLeadingNL := false
	return &llm.AgentCallbacks{
		OnReasoning: func(content string) {
			trimLeadingNL = true
			turnUI.ShowThinking(content)
		},
		OnContent: func(content string) {
			if config.SchemaPath != "" || t.settledOutput {
				return
			}
			if trimLeadingNL {
				content = trimLeadingResponseNewlines(content)
				if content == "" {
					return
				}
				trimLeadingNL = false
			}
			turnUI.AppendAssistantText(content)
		},
		OnToolStart: func(calls []messages.ChatMessageToolCall) {
			turnUI.AppendToolStart(calls)
		},
		// A spawned child's approvals come back through this turn's UI.
		BeforeToolExecute: func(ctx context.Context, call messages.ChatMessageToolCall, _ map[string]any) context.Context {
			return withToolCall(withParentTurnUI(ctx, turnUI), call)
		},
		ApproveToolCalls: func(calls []messages.ChatMessageToolCall) []bool {
			return approveToolCalls(t.ctx, turnUI, "", calls)
		},
		OnToolEnd: func(tc messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
			t.stats.record(tc.Name, err)
			turnUI.AppendToolEnd(tc, result, duration, err)
		},
		OnToolResult: func(tc messages.ChatMessageToolCall, result messages.ChatMessage) {
			turnUI.AppendToolResult(tc, result)
			if images := inspectionTranscriptImages(result, t.state.artifactStore); len(images) > 0 {
				turnUI.AppendToolMedia(tc, images)
			}
		},
		OnError:            func(err error) {},
		BeforeFirstRequest: t.persistUser,
		OnRequestProjection: func(iteration int, stats llm.ProjectionStats) {
			t.usage.project(iteration, stats, req.MaxContextTokens)
			turnUI.RecordContextUsage(t.usage.used, t.usage.limit, t.usage.estimated)
		},
		OnIterationUsage: func(iteration, in, out int) {
			peak, total := t.usage.record(iteration, in, out)
			turnUI.RecordTurnTokens(peak, total)
			turnUI.RecordContextUsage(t.usage.used, t.usage.limit, t.usage.estimated)
		},
	}
}

// recordUsage reports the finished run's token usage to the turn UI and
// returns it for the meta trailer. The trailer retains peak input usage and
// total output usage; context usage follows the latest call, since projection
// can shrink between iterations and an unreported final usage must fall back
// to its estimate.
func (t *turnExecution) recordUsage(resp *llm.AgentResponse) (in, out int) {
	if resp == nil {
		return 0, 0
	}
	in, out = resp.TokenUsage()
	t.turnUI.RecordTurnTokens(in, out)
	if t.usage.projected {
		t.turnUI.RecordContextUsage(t.usage.used, t.usage.limit, t.usage.estimated)
	}
	return in, out
}

// settlePersistence persists everything the run generated, completed or not,
// and returns the turn's error with the persistence outcome folded in.
// Executed tool calls already changed the world; dropping their record would
// leave the session blind to work that actually happened and make a retry
// redo it. Run guarantees AllMessages ends at a provider-valid boundary (an
// aborted tool batch is completed with interrupted stubs), so a partial turn
// replays cleanly. The detached context keeps a canceled turn's save from
// being canceled along with it; the persistence gate stops a detached turn
// from appending after newer turns already have.
func (t *turnExecution) settlePersistence(resp *llm.AgentResponse, runErr error) error {
	if resp == nil || len(resp.AllMessages) == 0 || !t.turnUI.TurnPersistenceAllowed() {
		return runErr
	}
	perr := t.persistTurn(context.WithoutCancel(t.ctx), resp, runErr)
	switch {
	case perr != nil:
		return errors.Join(runErr, perr)
	case runErr != nil:
		// Both facts matter downstream: the turn failed, and the work it
		// completed is durable. UIs label the outcome accordingly.
		return &turnProgressSavedError{cause: runErr}
	}
	return nil
}

func (t *turnExecution) persistTurn(ctx context.Context, resp *llm.AgentResponse, runErr error) error {
	if err := persistActiveSkills(ctx, t.state.session, t.state.skillRuntime, t.state.skillSources); err != nil {
		return fmt.Errorf("failed to persist active skills: %w", err)
	}
	// Persist the whole turn (assistant message per iteration + every tool
	// result) with a single write instead of one rewrite per message. A
	// failed turn additionally records why it ended, so hydration can settle
	// it instead of rendering an abandoned turn.
	durable := durableTurnMessages(resp.AllMessages[resp.PersistedMessages:])
	if runErr != nil {
		durable = append(durable, interruptedTurnMarker(runErr))
	}
	if err := t.state.session.AddMessages(ctx, durable); err != nil {
		return fmt.Errorf("failed to persist turn: %w", err)
	}
	return nil
}

// finishOutput is the success tail: everything downstream of a completed
// agent run, from projection and truncation warnings to the final output.
func (t *turnExecution) finishOutput(resp *llm.AgentResponse) error {
	if resp == nil {
		return fmt.Errorf("agent returned no response")
	}
	if resp.Projection.OmittedExchanges > 0 {
		word := "exchanges"
		if resp.Projection.OmittedExchanges == 1 {
			word = "exchange"
		}
		t.turnUI.AppendWarning(fmt.Sprintf("model context omitted %d earlier %s; full transcript retained", resp.Projection.OmittedExchanges, word))
	}
	if resp.Message != nil && resp.Message.StopReason == messages.StopReasonMaxTokens {
		t.turnUI.AppendWarning(fmt.Sprintf("response truncated (hit %d token limit, use --maxtokens to increase)", t.settings.MaxTokens))
	}
	if t.config.SchemaPath != "" {
		if ui, ok := t.turnUI.(*lineTurnUI); ok {
			ui.pauseActivity()
		}
		var content string
		if resp.Message != nil {
			content = resp.Message.Content
		}
		return outputStructured(content, t.schema)
	}
	if t.settledOutput && resp.Message != nil {
		t.turnUI.AppendAssistantText(resp.Message.Content)
	}
	t.turnUI.FinishTextTurn()
	return nil
}
