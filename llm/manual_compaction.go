package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/alexschlessinger/pollytool/messages"
)

// Compact summarizes the entire effective conversation in req.Messages on
// demand, independently of the automatic compaction threshold. It adds no user
// input, executes no tools (including an incomplete historical tool batch), and
// makes no continuation request. The caller must serialize Compact with Run and
// configuration changes on the same agent.
//
// The result's Message is nil and IterationCount and PersistedMessages are zero.
// AllMessages contains summary usage records and, on success, a Compaction marker
// with KeepsTurn false. Empty or system-only history, or a summary with no new
// content to cover, returns no generated messages and makes no provider call.
// A failed, canceled, over-budget or non-shrinking summary returns an error and
// no marker, but retains any completed summary usage in AllMessages.
//
// Only OnAdaptation, OnCompactionUsage and OnRequestProjection are used.
// OnAdaptation reports preparation, summary start and failures, never compaction
// success: only the caller knows when the marker has been saved. OnRequestProjection
// observes the accepted conversation projection once, with iteration zero, not
// the summary provider requests. No Checkpoint or other callback is called; the
// caller must persist AllMessages, including usage returned on failure, and append
// them to the original history without rewriting it.
func (a *Agent) Compact(ctx context.Context, req *CompletionRequest, cb *AgentCallbacks) (result *AgentResponse, compactErr error) {
	if req == nil {
		return &AgentResponse{}, errors.New("a compaction request is required")
	}
	var observers *AgentCallbacks
	if cb != nil {
		observers = &AgentCallbacks{
			OnAdaptation: cb.OnAdaptation, OnCompactionUsage: cb.OnCompactionUsage,
			OnRequestProjection: cb.OnRequestProjection,
		}
	}
	r := a.newRun(req, observers)
	r.summaryOnly = true
	r.noted = map[string]bool{}
	a.resetArtifactIndex(r.msgs)
	a.setTranscript(r.msgs)
	var marked, built bool
	var before ProjectionStats
	defer func() {
		if compactErr != nil {
			if marked {
				r.withdraw()
			}
			if built {
				r.lastProjection = before
			}
			r.adapt(RequestAdaptation{Feature: FeatureCompactionFailure, Message: fmt.Sprintf("Compaction failed: %v", compactErr)})
		}
		result = r.response(nil, 0)
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !hasUncompactedContent(r.msgs) {
		return nil, nil
	}
	if a.client == nil {
		return nil, errors.New("a compaction client is required")
	}
	prepared, size, err := r.build(ctx, nil)
	if err != nil {
		return nil, err
	}
	before, built = r.lastProjection, true
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plan := compactionPlan{
		summarize: true,
		input:     contextView(r.msgs, r.projectionTools(r.loopTools())),
		target:    summaryTarget(&prepared, a.compactionModel(&prepared)),
	}
	// A transcript has no tools and accepts even an unfinished historical
	// batch. In-place summarization would advertise tools and require replayable
	// provider history, neither of which a manual compaction needs.
	if _, err := r.compact(ctx, &prepared, plan, false); err != nil {
		return nil, err
	}
	marked = true
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	compacted, after, err := r.build(ctx, nil)
	if err != nil {
		return nil, err
	}
	if budget := compacted.MaxContextTokens; budget > 0 && after > budget {
		return nil, r.overBudget(after, budget)
	}
	// Manual requests can be much smaller than the automatic threshold. Do
	// not replace such a conversation with a summary that consumes more room.
	if after >= size {
		return nil, fmt.Errorf("compaction did not reduce the request size (%d → %d tokens)", size, after)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if observers != nil && observers.OnRequestProjection != nil {
		observers.OnRequestProjection(0, r.lastProjection)
	}
	return nil, ctx.Err()
}

// hasUncompactedContent ignores the last summary's covered prefix, except the
// turn it keeps verbatim. A summary itself, systems and accounting records do
// not justify paying for another summary with nothing new to cover.
func hasUncompactedContent(history []messages.ChatMessage) bool {
	start := 0
	if summary := lastSummary(history, len(history)); summary >= 0 {
		start = summary + 1
		if c, _ := history[summary].Compaction(); c.KeepsTurn {
			if turn := keptTurnStart(history, summary); turn >= 0 {
				start = turn
			}
		}
	}
	return slices.ContainsFunc(history[start:], func(msg messages.ChatMessage) bool {
		return msg.Role != messages.MessageRoleSystem && msg.Role != messages.MessageRoleInternal
	})
}
