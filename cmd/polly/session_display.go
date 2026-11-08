package main

import (
	"context"
	"fmt"
	"maps"
	"reflect"

	"github.com/alexschlessinger/pollytool/sessions"
)

const (
	hostTranscriptPageMessages = 128
	hostTranscriptPageBytes    = 256 << 10
)

// readSessionDisplay selects only the visible real-user turns on explicitly
// native stores, plus compact prefix facts needed to preserve display semantics.
// Backends without that contract retain the complete-history fallback.
// A retained header belongs to the same immutable display and window policy as
// knownRevision; equal epoch/sequence skips all transcript and prefix payloads.
func readSessionDisplay(ctx context.Context, reader sessions.ViewStore, target sessions.ViewTarget, knownRevision string, retained **sessions.SessionViewHeader) (*sessions.SessionView, error) {
	return readSessionDisplayWindow(ctx, reader, target, knownRevision, retained, resumedTurnLimit, true)
}

func readSessionDisplayWindow(ctx context.Context, reader sessions.ViewStore, target sessions.ViewTarget, knownRevision string, retained **sessions.SessionViewHeader, turns int, prefixNeeded bool) (*sessions.SessionView, error) {
	headers, headerOK := reader.(sessions.ViewHeaderStore)
	pages, pageOK := reader.(sessions.TranscriptPageStore)
	if !headerOK || !pageOK {
		if retained != nil {
			*retained = nil
		}
		return reader.ReadView(ctx, target, knownRevision)
	}
	header, err := headers.ReadViewHeader(ctx, target, knownRevision)
	if err != nil {
		return nil, err
	}
	if header == nil || header.ID == "" || header.Epoch == "" || header.Revision == "" || header.Metadata == nil || header.Sequence < 0 || target.ID != "" && target.ID != header.ID {
		return nil, fmt.Errorf("invalid session view header")
	}
	info := sessionViewFromHeader(header)
	if retained != nil && *retained != nil {
		previous := *retained
		if previous.Revision == knownRevision && previous.ID == header.ID && previous.Epoch == header.Epoch && previous.Sequence == header.Sequence {
			info.Unchanged = true
		}
	}
	if !info.Unchanged {
		start := int64(0)
		if display, ok := nativeTranscriptDisplay(reader); ok {
			prefix, err := readSessionDisplayPrefix(ctx, display, header, turns, prefixNeeded)
			if err != nil {
				return nil, err
			}
			info.Display, start = prefix, prefix.Start
		}
		for after := start; after < header.Sequence; {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			page, err := pages.ReadTranscriptPage(ctx, sessions.ViewTarget{ID: header.ID}, sessions.TranscriptQuery{
				Epoch: header.Epoch, After: after, Through: header.Sequence,
				Limit: hostTranscriptPageMessages, MaxBytes: hostTranscriptPageBytes,
			})
			if err != nil {
				return nil, err
			}
			if page.ID != header.ID || page.Epoch != header.Epoch || page.First != after || page.Through != header.Sequence || page.Next <= after || page.Next > header.Sequence || page.Next-after != int64(len(page.Messages)) {
				return nil, fmt.Errorf("inconsistent session transcript page")
			}
			info.History = append(info.History, page.Messages...)
			after = page.Next
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if retained != nil {
		*retained = header
	}
	return info, nil
}

func sessionHeaderBytes(header *sessions.SessionViewHeader) int64 {
	if header == nil {
		return 0
	}
	return int64(len(header.ID)+len(header.Revision)+len(header.Epoch)+len(header.ParentID)) + retainedViewBytes(reflect.ValueOf(header.Metadata))
}

func sessionViewFromHeader(header *sessions.SessionViewHeader) *sessions.SessionView {
	return &sessions.SessionView{ID: header.ID, Revision: header.Revision, Sequence: header.Sequence,
		ParentID: header.ParentID, Metadata: header.Metadata, Artifacts: header.Artifacts,
		InUse: header.InUse, Unchanged: header.Unchanged}
}

// Metadata-only consumers may use a header only when the supplied backend also
// supplies the corresponding epoch-bound transcript capability. No hidden store
// or independently sampled summary is substituted for a display header.
func readSessionHeader(ctx context.Context, reader sessions.ViewStore, target sessions.ViewTarget) (*sessions.SessionView, error) {
	if headers, ok := reader.(sessions.ViewHeaderStore); ok {
		if _, ok := reader.(sessions.TranscriptPageStore); ok {
			header, err := headers.ReadViewHeader(ctx, target, "")
			if err != nil {
				return nil, err
			}
			return sessionViewFromHeader(header), nil
		}
	}
	return reader.ReadView(ctx, target, "")
}

// Keep the saved display immutable while updating header-derived presentation.
// Continuation is display-only and its record pointers are rebound by ID when
// preparing a live overlay; no session or mutation authority is retained.
func refreshSavedDisplayMetadata(ctx context.Context, saved *replModel, info *sessions.SessionView, cfg *Config) *replModel {
	next := childDisplayCopy(saved)
	// Cached child bindings are observations, not transcript identity. Drop
	// them on the owned copy even if the subsequent summary refresh fails.
	next.clearAgentSessionSummaries()
	next.historyContinuation, next.historyContinuationAt = saved.historyContinuation, saved.historyContinuationAt
	next.unshownCallIDs = maps.Clone(saved.unshownCallIDs)
	next.swarmParent = saved.swarmParent
	settings := readOnlyConversationState(cfg, nil, info).settings
	next.status = newSessionStatus(&settings, info.Metadata.Name, len(info.Metadata.ActiveTools), len(info.Metadata.ActiveSkills))
	next.status.parentName, next.status.description = info.Metadata.Parent, info.Metadata.Description
	next.status.title, next.status.titleSource = info.Metadata.Title, info.Metadata.TitleSource
	next.artifactStore = info.Artifacts
	return next
}
