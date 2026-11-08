package main

import (
	"context"
	"fmt"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// readTabDisplayHistory uses only the store/identity supplied by the active
// state. It never discovers a different backend or acquires another lease.
func readTabDisplayHistory(ctx context.Context, state *conversationState) ([]messages.ChatMessage, *sessions.TranscriptDisplay, error) {
	if reader, ok := state.sessionStore.(sessions.ViewStore); ok {
		if _, native := nativeTranscriptDisplay(reader); native {
			if identity, ok := state.session.(sessions.ViewIdentity); ok && identity.ViewID() != "" {
				info, err := readSessionDisplay(ctx, reader, sessions.ViewTarget{ID: identity.ViewID()}, "", nil)
				if err != nil {
					return nil, nil, err
				}
				return info.History, info.Display, nil
			}
		}
	}
	history, err := state.session.GetHistory(ctx)
	return history, nil, err
}

func nativeTranscriptDisplay(reader sessions.ViewStore) (sessions.TranscriptDisplayStore, bool) {
	display, ok := reader.(sessions.TranscriptDisplayStore)
	if !ok {
		return nil, false
	}
	reporter, ok := reader.(sessions.TranscriptCapabilityReporter)
	if !ok {
		return nil, false
	}
	caps := reporter.TranscriptCapabilities()
	return display, caps.NativeIndexed && caps.StableEpoch && caps.NativeDisplay
}

func readSessionDisplayPrefix(ctx context.Context, reader sessions.TranscriptDisplayStore, header *sessions.SessionViewHeader, turns int, prefixNeeded bool) (*sessions.TranscriptDisplay, error) {
	target := sessions.ViewTarget{ID: header.ID}
	window, err := reader.ReadTranscriptWindow(ctx, target, sessions.TranscriptWindowQuery{Epoch: header.Epoch, Through: header.Sequence, Turns: turns})
	if err != nil {
		return nil, err
	}
	if window.ID != header.ID || window.Epoch != header.Epoch || window.Through != header.Sequence || window.Start < 0 || window.Start > window.Through || window.PreviousTurn < -1 || window.PreviousTurn >= window.Start {
		return nil, fmt.Errorf("inconsistent session display window")
	}
	out := &sessions.TranscriptDisplay{Start: window.Start, HasPreviousTurn: window.PreviousTurn >= 0}
	// Agent progress only projects the latest turn; whole-conversation section
	// identities, attachment-token reuse and change totals are not consumed.
	if !prefixNeeded {
		return out, nil
	}
	// Ordinary prior prompts take one indexed lookup. Context imports and old
	// report notices are walked, not silently mistaken for the assignment.
	if out.HasPreviousTurn {
		for before := window.Start; before > 0; {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			previous, err := reader.ReadTranscriptPreviousUser(ctx, target, header.Epoch, before)
			if err != nil {
				return nil, err
			}
			if previous == nil {
				break
			}
			if previous.Sequence < 0 || previous.Sequence >= before || previous.Message.Role != messages.MessageRoleUser || agentSyntheticMessage(previous.Message) {
				return nil, fmt.Errorf("inconsistent preceding session display user")
			}
			_, _, contextOnly, notice := historyUserSummary(previous.Message)
			if !contextOnly && !notice {
				out.Previous = previous
				break
			}
			before = previous.Sequence
		}
	}
	for after := int64(0); after < window.Start; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := reader.ReadTranscriptDisplayPage(ctx, target, sessions.TranscriptQuery{Epoch: header.Epoch, After: after, Through: window.Start, Limit: hostTranscriptPageMessages, MaxBytes: hostTranscriptPageBytes})
		if err != nil {
			return nil, err
		}
		if page.ID != header.ID || page.Epoch != header.Epoch || page.First != after || page.Through != window.Start || page.Next <= after || page.Next > window.Start {
			return nil, fmt.Errorf("inconsistent session display prefix page")
		}
		last := after - 1
		for _, fact := range page.Facts {
			if fact.Sequence <= last || fact.Sequence >= page.Next || fact.Turn < -1 || fact.Turn > fact.Sequence || fact.Barrier < -1 || fact.Barrier >= fact.Sequence || fact.AnswerBarrier < -1 || fact.AnswerBarrier >= fact.Sequence {
				return nil, fmt.Errorf("inconsistent session display prefix fact")
			}
			last = fact.Sequence
		}
		out.Prefix = append(out.Prefix, page.Facts...)
		after = page.Next
	}
	return out, nil
}
