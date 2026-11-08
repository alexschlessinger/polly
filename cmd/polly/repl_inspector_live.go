package main

import (
	"context"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

const inspectorLiveRefreshInterval = 100 * time.Millisecond

// Only the selected member's bounded transcript is copied. Agent list/status
// paints continue to use the metadata-only LiveActivities snapshot.
type inspectorLiveReader interface {
	LiveTranscript(string) (swarm.LiveTranscript, bool)
}

type inspectorLiveIdentity struct {
	execution  string
	generation int
}

type inspectorLiveStatus struct {
	active, truncated, incomplete bool
	// identified also records an invalidated durable binding. Such a view
	// must not fall back to assignment-text matching as legacy history does.
	identified bool
	// identity records a verified saved/live pairing, even after settlement.
	// It is not inferred from the user-facing assignment text.
	identity inspectorLiveIdentity
}

// inspectorLiveSource uses the workspace owner's runtime, including while the
// visible tab is a saved child. Merely opening an inspector never acquires a
// session lease or starts an agent.
func (r *managedREPL) inspectorLiveSource(target viewTarget) (inspectorLiveReader, inspectorLiveIdentity) {
	if target.kind != conversationViewKind || target.session.ID == "" {
		return nil, inspectorLiveIdentity{}
	}
	owner := r.rootTab(r.visibleTab())
	if owner.state == nil || owner.state.swarm == nil || owner.displaySnapshot() == nil {
		return nil, inspectorLiveIdentity{}
	}
	member := owner.displaySnapshot().Members[target.session.ID]
	if member == nil {
		return nil, inspectorLiveIdentity{}
	}
	execution := owner.displaySnapshot().Executions[member.Execution]
	if execution == nil {
		return nil, inspectorLiveIdentity{}
	}
	return owner.state.swarm, inspectorLiveIdentity{execution.ID, execution.Generation}
}

func (r *managedREPL) inspectorRefreshInterval(target viewTarget) time.Duration {
	if reader, _ := r.inspectorLiveSource(target); reader != nil {
		owner := r.rootTab(r.visibleTab())
		member := owner.displaySnapshot().Members[target.session.ID]
		if agentExecutionBusy(owner.displaySnapshot(), member) {
			return inspectorLiveRefreshInterval
		}
		// A terminal/absent snapshot must replace the last provisional render
		// promptly even if the execution status has already become idle.
		if v := r.workspace().inspector.current; v != nil && v.live.active {
			return inspectorLiveRefreshInterval
		}
	}
	return time.Second
}

// readInspectorConversation deliberately samples live BEFORE the saved view.
// A checkpoint racing these reads either leaves a matching prefix or makes the
// pair unusable. Never append by content/role similarity, and never read Intent.
// Even an absent/settled live snapshot must read storage: it can acknowledge the
// final checkpoint and invalidate a previously painted provisional response.
func readInspectorConversation(ctx context.Context, reader sessions.ViewStore, target sessions.ViewTarget, knownRevision string, liveReader inspectorLiveReader, identity inspectorLiveIdentity) (*sessions.SessionView, swarm.LiveTranscript, inspectorLiveStatus, error) {
	return readInspectorConversationCached(ctx, reader, target, knownRevision, liveReader, identity, nil)
}

func readInspectorConversationCached(ctx context.Context, reader sessions.ViewStore, target sessions.ViewTarget, knownRevision string, liveReader inspectorLiveReader, identity inspectorLiveIdentity, retained **sessions.SessionViewHeader) (*sessions.SessionView, swarm.LiveTranscript, inspectorLiveStatus, error) {
	return readInspectorWindowCached(ctx, reader, target, knownRevision, liveReader, identity, retained, false)
}

// Progress checkpoints have a distinct cache namespace and window policy. They
// never replace a conversation's five-turn display or its whole-session catalog.
func readInspectorProgressCached(ctx context.Context, reader sessions.ViewStore, target sessions.ViewTarget, knownRevision string, liveReader inspectorLiveReader, identity inspectorLiveIdentity, retained **sessions.SessionViewHeader) (*sessions.SessionView, swarm.LiveTranscript, inspectorLiveStatus, error) {
	return readInspectorWindowCached(ctx, reader, target, knownRevision, liveReader, identity, retained, true)
}

func readInspectorWindowCached(ctx context.Context, reader sessions.ViewStore, target sessions.ViewTarget, knownRevision string, liveReader inspectorLiveReader, identity inspectorLiveIdentity, retained **sessions.SessionViewHeader, progress bool) (*sessions.SessionView, swarm.LiveTranscript, inspectorLiveStatus, error) {
	var live swarm.LiveTranscript
	var available bool
	if liveReader != nil {
		live, available = liveReader.LiveTranscript(target.ID)
	}
	turns := resumedTurnLimit
	if progress {
		turns = 1
	}
	info, err := readSessionDisplayWindow(ctx, reader, target, knownRevision, retained, turns, !progress)
	if err != nil {
		return nil, swarm.LiveTranscript{}, inspectorLiveStatus{}, err
	}
	var status inspectorLiveStatus
	if info.Metadata != nil {
		status.identified = info.Metadata.Transcript != nil
		if saved := info.Metadata.Transcript; saved != nil && saved.Execution != "" && saved.Generation > 0 && saved.Sequence == info.Sequence {
			status.identity = inspectorLiveIdentity{saved.Execution, saved.Generation}
		}
	}
	matches := available && live.Ready && identity.execution != "" &&
		live.Execution == identity.execution && live.Generation == identity.generation &&
		(live.SessionID == "" || live.SessionID == info.ID) && info.Sequence == live.BaseSequence &&
		(!status.identified || status.identity == identity)
	if !matches {
		return info, swarm.LiveTranscript{}, status, nil
	}
	if live.Settled {
		// Failed/canceled partials are not committed assistant content. Retain
		// only an honest notice; the saved checkpoint is the source of truth.
		return info, swarm.LiveTranscript{}, inspectorLiveStatus{incomplete: live.Incomplete, identified: true, identity: identity}, nil
	}
	return info, live, inspectorLiveStatus{active: true, truncated: live.Truncated, identified: true, identity: identity}, nil
}

func inspectorConversationRevision(info *sessions.SessionView, live swarm.LiveTranscript, status inspectorLiveStatus) string {
	if status.active {
		return fmt.Sprintf("%s/live:%s:%d:%d:%d", info.Revision, live.Execution, live.Generation, live.Epoch, live.Revision)
	}
	if status.incomplete {
		return info.Revision + "/incomplete"
	}
	return info.Revision
}
