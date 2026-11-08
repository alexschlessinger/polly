package swarm

import (
	"context"
	"encoding/json"

	"github.com/alexschlessinger/pollytool/sessions"
)

// ReadMemberWorkspaceView resolves exactly one member/context binding through
// the supplied trusted reader. Inspection never opens a session or repairs a
// stale binding, and an unrelated context cannot confer a workspace path.
func ReadMemberWorkspaceView(ctx context.Context, store any, root, memberID, contextID string) (string, error) {
	access, err := sessions.OpenCoordinationViewAccess(store)
	if err != nil {
		return "", err
	}
	var path string
	err = access.ReadRecordsView(ctx, root, func(raw sessions.CoordinationReader) error {
		member, found, err := raw.Get(sessions.RecordKey{Kind: "member", ID: memberID})
		if err != nil || !found {
			return err
		}
		format, _, err := raw.Get(sessions.RecordKey{Kind: formatKind, ID: formatID})
		if err != nil {
			return err
		}
		var f *FormatRecord
		if len(format.Payload) > 0 {
			if err = json.Unmarshal(format.Payload, &f); err != nil {
				return err
			}
		}
		if f == nil || f.Version != swarmFormatVersion {
			return unsupportedFormat(root, f)
		}
		var m Member
		if err = json.Unmarshal(member.Payload, &m); err != nil {
			return err
		}
		if contextID == "" || m.Context != contextID {
			return nil
		}
		context, found, err := raw.Get(sessions.RecordKey{Kind: "context", ID: contextID})
		if err != nil || !found {
			return err
		}
		var c ExecutionContext
		if err = json.Unmarshal(context.Payload, &c); err != nil {
			return err
		}
		if c.Owner == memberID {
			path = c.Root
		}
		return nil
	})
	return path, err
}

// DisplayStatus is a small, owned observation for UI badges and navigation.
// It does not include batch Next text or confer mutation authority.
type DisplayStatus struct {
	Counts            Counts
	HasRuns           bool
	ParentDisposition string
	First             *DecisionItem
	RetryPause        *RetryPause
}

func displayStatus(raw sessions.CoordinationReader, actor string) (DisplayStatus, bool, error) {
	reader, ok := raw.(sessions.CoordinationStatusReader)
	if !ok {
		return DisplayStatus{}, false, nil
	}
	h, err := reader.SemanticStatus(actor, false)
	if err != nil {
		return DisplayStatus{}, true, err
	}
	if h.ColdCompatibility {
		return DisplayStatus{}, false, nil
	}
	out := DisplayStatus{Counts: Counts(h.Counts), HasRuns: h.HasRuns, ParentDisposition: h.ParentDisposition}
	turn, found, err := raw.Get(sessions.RecordKey{Kind: "parent_turn", ID: actor})
	if err != nil {
		return out, true, err
	}
	if found {
		var p ParentTurn
		if err = json.Unmarshal(turn.Payload, &p); err != nil {
			return out, true, err
		}
		out.RetryPause = p.RetryPause
	}
	page, err := reader.SemanticStatusPage(sessions.SemanticStatusQuery{Actor: actor, Section: "decisions", Limit: 1})
	if len(page.Decisions) > 0 {
		d := page.Decisions[0]
		out.First = &DecisionItem{Kind: d.Kind, ID: d.ID, Label: d.Label, Why: d.Why, Action: d.Action, Member: d.Member, State: d.State}
	}
	return out, true, err
}

func legacyDisplayStatus(s *State, actor string) DisplayStatus {
	p := Present(s, actor, actor)
	out := DisplayStatus{Counts: p.Counts, HasRuns: len(s.Runs) > 0, ParentDisposition: parentDisposition(s)}
	if len(p.Decisions) > 0 {
		out.First = &p.Decisions[0]
	}
	if turn := s.ParentTurns[actor]; turn != nil {
		out.RetryPause = turn.RetryPause
	}
	return out
}

func readDisplayStatus(rd *recordReader, actor string) (DisplayStatus, error) {
	out, native, err := displayStatus(rd.raw, actor)
	if err != nil || native {
		return out, err
	}
	s, err := rd.SlowSnapshot()
	if err != nil {
		return out, err
	}
	return legacyDisplayStatus(s, actor), nil
}

func (r *Runtime) ReadDisplayStatus(ctx context.Context) (DisplayStatus, error) {
	var out DisplayStatus
	err := r.readRecords(ctx, func(rd *recordReader) error {
		var err error
		out, err = readDisplayStatus(rd, r.ID)
		return err
	})
	return out, err
}

func ReadDisplayStatusView(ctx context.Context, store any, root string) (DisplayStatus, error) {
	access, err := sessions.OpenCoordinationViewAccess(store)
	if err != nil {
		return DisplayStatus{}, err
	}
	var out DisplayStatus
	err = access.ReadRecordsView(ctx, root, func(raw sessions.CoordinationReader) error {
		rd := &recordReader{raw: raw, ctx: ctx, active: true}
		defer func() { rd.active = false }()
		var err error
		out, err = readDisplayStatus(rd, root)
		return err
	})
	return out, err
}

// ParentDisplayState overlays the in-memory parent turn on a durable narrow
// status observation. Runtime idleness is never inferred from persisted counts.
func (r *Runtime) ParentDisplayState(status DisplayStatus) AgentPresentation {
	return r.parentDisplayState(status.RetryPause, &status.ParentDisposition)
}
