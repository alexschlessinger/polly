package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
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

	// settledOutput is the non-interactive one-shot without --stream: every
	// answer block prints once, after the run, instead of streaming.
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
	// The agent validates references after capability adaptation, before persisting input.
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
		OnAdaptation: func(note llm.RequestAdaptation) { turnUI.AppendWarning(note.Message) },
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
	if t.settledOutput {
		if text := answerText(resp); text != "" {
			t.turnUI.AppendAssistantText(text)
		}
	}
	t.turnUI.FinishTextTurn()
	return nil
}

// executeTurn runs one turn on a prompt (plus --file inputs) and returns the
// process exit code the turn's outcome maps to (0 end_turn, 2 max_tokens,
// 3 max_iterations, 1 hard error) alongside any error. Only the one-shot
// path acts on the code; the REPLs ignore it and consume just the error.
func executeTurn(ctx context.Context, config *Config, state *conversationState, prompt string, schema *llm.Schema, inputReader *bufio.Reader, turnUI TurnUI) (int, error) {
	userMsg, err := buildMessageWithFiles(prompt, config.Files)
	if err != nil {
		return 1, fmt.Errorf("error processing files: %w", err)
	}
	return executeTurnWithUserMessage(ctx, config, state, userMsg, schema, inputReader, turnUI, false)
}

// executeTurnWithUserMessage is the shared turn body behind a caller-built
// user message. The one-shot and fallback paths build theirs from --file;
// the managed REPL builds a multimodal message from composer attachments.
// reuseUser avoids persisting the same user message twice when an unchanged
// restored draft is resubmitted: only an equivalent user message at the very
// end of history is reused, so a missing, changed, or non-terminal message is
// persisted normally. The phases live on turnExecution; this sequences them
// and owns the turn UI's lifecycle.
func executeTurnWithUserMessage(ctx context.Context, config *Config, state *conversationState, userMsg messages.ChatMessage, schema *llm.Schema, inputReader *bufio.Reader, turnUI TurnUI, reuseUser bool) (exitCode int, finalErr error) {
	t := &turnExecution{ctx: ctx, config: config, state: state, settings: &state.settings, schema: schema, userMsg: userMsg, reuseUser: reuseUser}
	requestMessages, instructionWarnings, err := t.prepareRequest()
	if err != nil {
		return 1, err
	}

	if turnUI == nil {
		turnUI = newLineTurnUIWithCapabilities(config, inputReader, state.outputCapabilities)
	}
	t.turnUI = turnUI
	turnUI.Start()
	defer turnUI.Stop()
	state.setTurnUI(turnUI)
	defer state.setTurnUI(nil)
	activityStart := time.Now()
	completed := false
	complete := func(reason messages.StopReason, err error) {
		if !completed {
			completed = true
			turnUI.CompleteTurn(turnCompletion{Reason: reason, Err: err, Elapsed: time.Since(activityStart), ProgressSaved: err == nil || turnProgressSaved(err)})
		}
	}
	defer func() {
		if !completed {
			if outputErr := flushTurnOutputError(turnUI); outputErr != nil {
				finalErr, exitCode = errors.Join(finalErr, outputErr), 1
			}
			complete(messages.StopReasonError, finalErr)
		}
	}()
	for _, warning := range instructionWarnings {
		turnUI.AppendWarning(warning)
	}

	req := createCompletionRequest(config, t.settings, requestMessages, state.effectiveTools(), state.skillCatalog, schema)
	req.MaxContextTokens = resolveContextBudget(ctx, state)
	req.CacheSessionID, err = state.session.CacheSessionID(ctx)
	if err != nil {
		return 1, fmt.Errorf("read session cache identity: %w", err)
	}
	if tui, ok := turnUI.(*gotuiTurnUI); ok {
		t.reportIDs = tui.turn.reportIDs
	}

	// The sandbox probe started with the open and has normally long
	// finished. A backend that cannot start fails the turn here, before the
	// first request and before the user message persists, rather than as
	// silent tool refusals later.
	if err := state.sandboxProbe.wait(ctx); err != nil {
		return 1, err
	}

	turnStart := time.Now()
	line, lineOutput := turnUI.(*lineTurnUI)
	t.settledOutput = lineOutput && !config.Stream && !line.interactive
	if lineOutput {
		line.settledOutput = t.settledOutput
	}
	callbacks := t.callbacks(req)
	var resp *llm.AgentResponse
	if state.swarm != nil {
		// The runtime owns the parent turn's lifecycle; the host still owns
		// persistence and output and reports their verdict below.
		updateSwarmDefaults(state, req, *t.settings)
		resp, err = state.swarm.RunParent(ctx, state.agent, req, callbacks, turnUI.TurnPersistenceAllowed)
	} else {
		resp, err = state.agent.Run(ctx, req, callbacks)
	}
	if ctx.Err() != nil {
		// Cancellation outranks whatever error the aborted run surfaced, but
		// the turn still flows through persistence below: tools that completed
		// changed the world whether or not the user hit cancel.
		err = context.Cause(ctx)
	}
	if err != nil && !t.persistAttempted && !t.reusingPersistedUser {
		// The run stopped before its first projection cleared the request.
		err = fmt.Errorf("prompt was not added to the conversation: %w", err)
	}
	in, out := t.recordUsage(resp)

	// Folding every later stage's error into runErr means the trailer and
	// exit code below always describe the turn's final state, whichever
	// stage failed.
	runErr := t.settlePersistence(resp, err)
	if runErr == nil {
		runErr = t.finishOutput(resp)
	}
	if runErr != nil && t.settledOutput && config.SchemaPath == "" {
		name, _ := state.session.GetName(context.WithoutCancel(ctx))
		turnUI.AppendAssistantText(settledAnswer(resp, runErr, name))
		turnUI.FinishTextTurn()
	}

	// The flush delivers every buffered answer byte; CompleteTurn only writes
	// chrome to stderr afterwards, so the sticky stdout error is final here.
	if outputErr := flushTurnOutputError(turnUI); outputErr != nil {
		runErr = errors.Join(runErr, outputErr)
	}
	if state.swarm != nil {
		state.swarm.ParentTurnSettled(runErr)
	}
	stopReason, code := classifyOutcome(resp, runErr)
	complete(stopReason, runErr)
	if config.Meta {
		writeMetaTrailer(os.Stderr, buildMeta(stopReason, resp, runErr, t.settings.Model, &t.stats, in, out, time.Since(turnStart).Milliseconds()))
	}
	return code, runErr
}
