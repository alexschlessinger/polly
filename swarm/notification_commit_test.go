package swarm

import (
	"errors"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
)

func TestNotificationRuntimeCommitBridges(t *testing.T) {
	t.Parallel()
	r, root := lifecycleRecordFixture(t, 0, false)
	access, err := sessions.OpenCoordinationAccess(root)
	if err != nil {
		t.Fatal(err)
	}
	n, clock, start := notificationTestProvider(t, &notificationSource{root: r.ID, durable: true, read: access.ReadRecords})
	r.notifications = n
	selected := notificationTestSubscribe(t, n, sessions.RevisionScope{Index: sessions.CoordinationIndexRecord, Key: sessions.CoordinationIndexKey("member", "selected")})
	unrelated := notificationTestSubscribe(t, n, sessions.RevisionScope{Index: sessions.CoordinationIndexRecord, Key: sessions.CoordinationIndexKey("member", "other")})
	news := notificationTestSubscribe(t, n, sessions.RevisionScope{Index: sessions.CoordinationIndexSemanticNews})
	display := notificationTestSubscribe(t, n, sessions.RevisionScope{Index: sessions.CoordinationIndexDisplay})
	activity := notificationTestSubscribe(t, n, ActivityScope())
	all := []*ChangeSubscription{selected, unrelated, news, display, activity}
	start()
	changeTestReceive(t, clock.timers)
	for _, sub := range all {
		changeTestReceive(t, sub.Changes())
	}
	// The whole-state API preserves its atomic source update, but native
	// receipts must not wake an unrelated member, semantic news, or activity.
	if _, ok := root.(sessions.CoordinationCommitSession); !ok {
		t.Fatal("native whole-state commit capability missing")
	}
	if err := r.update(t.Context(), func(s *State) error {
		s.Members["selected"].Label = "renamed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	changeTestReceive(t, selected.Changes())
	changeTestReceive(t, display.Changes())
	for _, sub := range []*ChangeSubscription{unrelated, news, activity} {
		changeTestQuiet(t, sub.sub)
	}
	// Native no-ops and rollback never become conservative invalidation.
	if err := r.update(t.Context(), func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	veto := errors.New("veto")
	if err := r.update(t.Context(), func(s *State) error {
		s.Members["selected"].Label = "must roll back"
		return veto
	}); !errors.Is(err, veto) {
		t.Fatal(err)
	}
	for _, sub := range all {
		changeTestQuiet(t, sub.sub)
	}
	// Record-scoped commits and local activity use separate routes.
	if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
		m, err := tx.Member("selected")
		if err != nil {
			return err
		}
		m.Label = "record update"
		return tx.PutMember(m)
	}); err != nil {
		t.Fatal(err)
	}
	changeTestReceive(t, selected.Changes())
	changeTestReceive(t, display.Changes())
	for _, sub := range []*ChangeSubscription{unrelated, news, activity} {
		changeTestQuiet(t, sub.sub)
	}
	r.activityChanged()
	changeTestReceive(t, activity.Changes())
	for _, sub := range []*ChangeSubscription{selected, unrelated, news, display} {
		changeTestQuiet(t, sub.sub)
	}
	// A wrapper that does not opt into whole-state receipts stays functional,
	// but cannot promise exact invalidation. No backend discovery bypasses it.
	r.parent = lifecycleLegacySession{root.(sessions.Session), root}
	if err := r.update(t.Context(), func(s *State) error {
		s.Members["selected"].Label = "legacy update"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, sub := range all {
		changeTestReceive(t, sub.Changes())
	}
}
