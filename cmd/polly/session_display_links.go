package main

import (
	"context"
	"errors"

	"github.com/alexschlessinger/pollytool/sessions"
)

// hydrateDisplayedAgentSessions resolves only launch rows present in this
// display. The native parent/call index rejects ambiguous links before metadata
// hydration, and stable ancestry is checked after resolving the parent's name.
// Older supplied hosts retain their complete summary adapter, never SQLite
// discovery. The model belongs exclusively to the caller during this operation.
func hydrateDisplayedAgentSessions(ctx context.Context, store sessions.SessionStore, parentID, parentName string, model *replModel) error {
	if !model.hasAgentRows() {
		return nil
	}
	headers, native := store.(sessions.ViewHeaderStore)
	if !native {
		var parent *string
		if parentID != "" {
			parent = &parentID
		}
		rows, err := sessionSummaries(ctx, store, parent)
		if err != nil {
			return err
		}
		model.hydrateAgentSessions(parentName, rows)
		return nil
	}
	calls := map[string]int{}
	for _, record := range model.toolDisclosures.all() {
		for _, row := range record.rows {
			if row.isAgent() && row.callID != "" {
				calls[row.callID]++
			}
		}
	}
	rows := make([]sessions.SessionSummary, 0, len(calls))
	for call, count := range calls {
		if count != 1 {
			continue
		}
		header, err := headers.ReadViewHeader(ctx, sessions.ViewTarget{Parent: parentName, SpawnCallID: call}, "")
		if errors.Is(err, sessions.ErrSessionNotFound) || errors.Is(err, sessions.ErrAgentSessionAmbiguous) {
			continue
		}
		if err != nil {
			return err
		}
		if header == nil || header.Metadata == nil || parentID != "" && header.ParentID != parentID {
			continue
		}
		rows = append(rows, sessions.SessionSummary{ID: header.ID, ParentID: header.ParentID, Metadata: header.Metadata, InUse: header.InUse})
	}
	model.hydrateAgentSessions(parentName, rows)
	return ctx.Err()
}
