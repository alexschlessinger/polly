package main

import (
	"context"
	"fmt"

	"github.com/alexschlessinger/pollytool/sessions"
)

const hostSummaryPageSize = 128

// These accessors borrow exactly the host-supplied store. Legacy access remains
// functional, but materializes ListSummaries and is not an indexed fast path.
func sessionSummary(ctx context.Context, store sessions.SessionStore, id string) (*sessions.SessionSummary, error) {
	access, err := sessions.OpenSummaryAccess(store)
	if err != nil {
		return nil, err
	}
	return access.GetSummary(ctx, id)
}

// readNavigationSummary is metadata-only; callers must load a full display
// before hydration. A stable ID is never resolved again through its display name.
func readNavigationSummary(ctx context.Context, reader sessions.ViewStore, target sessions.ViewTarget) (*sessions.SessionView, error) {
	if store, ok := reader.(sessions.SessionStore); ok && target.ID != "" {
		summary, err := sessionSummary(ctx, store, target.ID)
		if err != nil {
			return nil, err
		}
		return &sessions.SessionView{ID: summary.ID, ParentID: summary.ParentID, Metadata: summary.Metadata, Sequence: int64(summary.MessageCount), InUse: summary.InUse}, nil
	}
	return readSessionHeader(ctx, reader, target)
}

func launchedAgentSummary(ctx context.Context, store sessions.SessionStore, parentID, callID string) (*sessions.SessionSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parentID == "" || callID == "" {
		return nil, sessions.ErrSessionNotFound
	}
	parent, err := sessionSummary(ctx, store, parentID)
	if err != nil {
		return nil, err
	}
	if parent == nil || parent.ID != parentID || parent.Metadata == nil {
		return nil, sessions.ErrSessionNotFound
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Live summary pages cannot prove uniqueness: a duplicate may move ahead
	// of the cursor. Use one atomic spawn selector on the supplied backend.
	// Header-only navigation needs no transcript paging capability.
	target := sessions.ViewTarget{Parent: parent.Metadata.Name, SpawnCallID: callID}
	var info *sessions.SessionView
	if headers, ok := store.(sessions.ViewHeaderStore); ok {
		header, readErr := headers.ReadViewHeader(ctx, target, "")
		err = readErr
		if err == nil && header != nil {
			info = sessionViewFromHeader(header)
		}
	} else if reader, ok := store.(sessions.ViewStore); ok {
		info, err = reader.ReadView(ctx, target, "")
	} else {
		return nil, sessions.ErrPagingUnavailable
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The parent's name can change or be reused after GetSummary. Never bind
	// the successful name lookup to a different parent, and never retry it.
	if info == nil || info.ID == "" || info.ParentID != parentID || info.Metadata == nil || info.Metadata.SpawnCallID != callID {
		return nil, sessions.ErrSessionNotFound
	}
	return &sessions.SessionSummary{ID: info.ID, ParentID: info.ParentID, Metadata: info.Metadata, MessageCount: int(info.Sequence), InUse: info.InUse}, nil
}

// sessionSummaries preserves complete picker/hydration results, selecting only
// direct children when parentID is supplied. Paging limits each storage request,
// not a requested full result. Pages are live, not a cross-page snapshot.
func sessionSummaries(ctx context.Context, store sessions.SessionStore, parentID *string) ([]sessions.SessionSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A caller requesting all rows must not turn a legacy backend's one full
	// listing into a full listing on every compatibility page. Keep its former
	// complete-result ordering/cost; only a supplied paging capability is paged.
	if _, ok := store.(sessions.SummaryPageStore); store != nil && !ok {
		rows, err := store.ListSummaries(ctx)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if parentID == nil {
			return rows, nil
		}
		var children []sessions.SessionSummary
		for _, row := range rows {
			if row.ParentID == *parentID {
				children = append(children, row)
			}
		}
		return children, nil
	}
	access, err := sessions.OpenSummaryAccess(store)
	if err != nil {
		return nil, err
	}
	query := sessions.SummaryQuery{ParentID: parentID, Limit: hostSummaryPageSize}
	var summaries []sessions.SessionSummary
	seen := make(map[string]bool)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := access.ListSummaryPage(ctx, query)
		if err != nil {
			return nil, err
		}
		for _, summary := range page.Summaries {
			// Live updates may move an identity across a page boundary. Do not
			// duplicate a picker row or a child launch binding in that case.
			if !seen[summary.ID] {
				summaries = append(summaries, summary)
				seen[summary.ID] = true
			}
		}
		if page.Next == "" {
			return summaries, nil
		}
		if page.Next == query.After {
			return nil, fmt.Errorf("session summary cursor did not advance")
		}
		query.After = page.Next
	}
}
