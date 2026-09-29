package sessiontest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func acquire(t *testing.T, s sessions.SessionStore, name, parent string) sessions.Session {
	t.Helper()
	h, err := s.Acquire(t.Context(), name, sessions.AcquireOptions{Parent: parent})
	must(t, err)
	t.Cleanup(func() { h.Close() })
	return h
}

func coord(t *testing.T, s sessions.Session) sessions.CoordinationSession {
	t.Helper()
	c, ok := s.(sessions.CoordinationSession)
	if !ok {
		t.Fatal("missing CoordinationSession")
	}
	return c
}

func view(t *testing.T, s sessions.SessionStore, target sessions.ViewTarget, revision string) *sessions.SessionView {
	t.Helper()
	v, ok := s.(sessions.ViewStore)
	if !ok {
		t.Fatal("missing ViewStore")
	}
	out, err := v.ReadView(t.Context(), target, revision)
	must(t, err)
	return out
}

func put(t *testing.T, s sessions.Session, text string) artifacts.Ref {
	t.Helper()
	ref, err := s.ArtifactStore().Put(t.Context(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte(text)})
	must(t, err)
	return ref
}

func record(c *sessions.CoordinationState, value string) {
	c.Records["contract"] = map[string]json.RawMessage{"checkpoint": json.RawMessage(value)}
}

func contents(t *testing.T, r io.ReadCloser) string {
	t.Helper()
	defer r.Close()
	b, err := io.ReadAll(r)
	must(t, err)
	return string(b)
}

// Run checks the storage capabilities managed execution consumes. Each case
// receives new backing storage, so the cases run in parallel; the same
// assertions apply to every adapter.
func Run(t *testing.T, newFixture func(*testing.T) *Fixture) {
	t.Run("stable_identity_and_views", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		s := f.Open()
		parent := acquire(t, s, "parent", "")
		child := acquire(t, s, "child", "parent")
		id := coord(t, child).ViewID()
		cache, err := child.CacheSessionID(t.Context())
		must(t, err)
		meta, err := child.GetMetadata(t.Context())
		must(t, err)
		meta.SpawnCallID = "spawn-1"
		must(t, child.SetMetadata(t.Context(), meta))
		must(t, child.AddMessage(t.Context(), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "original"}))
		ref := put(t, child, "private bytes")
		before := view(t, s, sessions.ViewTarget{ID: id}, "")
		if !before.InUse || before.ParentID != coord(t, parent).ViewID() {
			t.Fatalf("bad view: %+v", before)
		}
		unchanged := view(t, s, sessions.ViewTarget{ID: id}, before.Revision)
		if !unchanged.Unchanged || len(unchanged.History) != 0 || !unchanged.Metadata.LastUsed.Equal(before.Metadata.LastUsed) {
			t.Fatal("view changed state")
		}
		before.History[0].Content = "mutated"
		before.Metadata.Description = "mutated"
		again := view(t, s, sessions.ViewTarget{ID: id}, "")
		if again.History[0].Content != "original" || again.Metadata.Description != "" {
			t.Fatal("snapshot aliases storage")
		}
		_, err = before.Artifacts.Put(t.Context(), artifacts.Blob{Data: []byte("bad")})
		if !errors.Is(err, sessions.ErrReadOnlyView) {
			t.Fatalf("view Put: %v", err)
		}
		if err = before.Artifacts.RemoveAll(t.Context()); !errors.Is(err, sessions.ErrReadOnlyView) {
			t.Fatalf("view RemoveAll: %v", err)
		}
		must(t, parent.Rename(t.Context(), "renamed-parent"))
		linked := view(t, s, sessions.ViewTarget{Parent: "renamed-parent", SpawnCallID: "spawn-1"}, before.Revision)
		if linked.ID != id || linked.Metadata.Parent != "renamed-parent" || linked.Unchanged {
			t.Fatal("parent rename lost identity or revision")
		}
		must(t, child.Rename(t.Context(), "renamed-child"))
		must(t, child.Close())
		reader, err := before.Artifacts.Open(t.Context(), ref.ID)
		must(t, err)
		if contents(t, reader) != "private bytes" {
			t.Fatal("view lost closed writer bytes")
		}
		must(t, s.Close())
		s = f.Open()
		child, err = s.Acquire(t.Context(), "renamed-child", sessions.AcquireOptions{ExpectedID: id})
		must(t, err)
		if coord(t, child).ViewID() != id {
			t.Fatal("reopen changed identity")
		}
		got, err := child.CacheSessionID(t.Context())
		must(t, err)
		if got != cache {
			t.Fatal("reopen changed cache identity")
		}
		reopened := view(t, s, sessions.ViewTarget{ID: id}, "")
		if len(reopened.History) != 1 {
			t.Fatal("transcript missing after reopen")
		}
		must(t, child.Close())
		must(t, s.Delete(t.Context(), "renamed-child"))
		replacement := acquire(t, s, "renamed-child", "")
		if coord(t, replacement).ViewID() == id {
			t.Fatal("identity reused")
		}
		must(t, replacement.Close())
		_, err = s.Acquire(t.Context(), "renamed-child", sessions.AcquireOptions{ExpectedID: id})
		if !errors.Is(err, sessions.ErrSessionNotFound) {
			t.Fatalf("stale identity rebound: %v", err)
		}
		_, err = s.(sessions.ViewStore).ReadView(t.Context(), sessions.ViewTarget{ID: id}, "")
		if !errors.Is(err, sessions.ErrSessionNotFound) {
			t.Fatalf("old view rebound: %v", err)
		}
		_, err = reopened.Artifacts.Open(t.Context(), ref.ID)
		if err == nil {
			t.Fatal("deleted view retained access")
		}
		_, err = s.(sessions.ViewStore).ReadView(t.Context(), sessions.ViewTarget{Name: "missing"}, "")
		if !errors.Is(err, sessions.ErrSessionNotFound) {
			t.Fatal(err)
		}
		exists, err := s.Exists(t.Context(), "missing")
		must(t, err)
		if exists {
			t.Fatal("view created a row")
		}
	})
	t.Run("lease_loss_fences_every_operation", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		s := f.Open()
		other := f.Open()
		h := acquire(t, s, "shared", "")
		c := coord(t, h)
		ref := put(t, h, "lease protected")
		reader, err := h.ArtifactStore().Open(t.Context(), ref.ID)
		must(t, err)
		defer reader.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		contender, err := other.Acquire(ctx, "shared", sessions.AcquireOptions{})
		if err == nil {
			contender.Close()
			t.Fatal("two live leases")
		}
		if !errors.Is(err, sessions.ErrSessionInUse) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("contention: %v", err)
		}
		if err = other.Delete(t.Context(), "shared"); !errors.Is(err, sessions.ErrSessionInUse) {
			t.Fatalf("delete leased: %v", err)
		}
		f.Revoke(c.ViewID())
		next := acquire(t, other, "shared", "")
		called := false
		err = c.UpdateCoordination(t.Context(), func(*sessions.CoordinationState) error { called = true; return nil })
		if !errors.Is(err, sessions.ErrSessionLeaseLost) || called {
			t.Fatalf("unfenced coordination: %v, called=%v", err, called)
		}
		if !errors.Is(context.Cause(h.Context()), sessions.ErrSessionLeaseLost) {
			t.Fatal("lease loss did not cancel session")
		}
		checks := []func() error{func() error { return h.AddMessage(t.Context(), messages.ChatMessage{Content: "stale"}) }, func() error { _, e := h.GetHistory(t.Context()); return e }, func() error {
			_, e := h.ArtifactStore().Put(t.Context(), artifacts.Blob{Data: []byte("stale")})
			return e
		}, func() error { _, e := reader.Read(make([]byte, 1)); return e }}
		for _, check := range checks {
			if err = check(); !errors.Is(err, sessions.ErrSessionLeaseLost) {
				t.Fatalf("stale operation: %v", err)
			}
		}
		must(t, h.Close())
		must(t, next.AddMessage(t.Context(), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "new owner"}))
		must(t, other.Close())
		if !errors.Is(context.Cause(next.Context()), sessions.ErrStoreClosed) {
			t.Fatal("store close did not cancel owner")
		}
	})
	t.Run("atomic_checkpoint_and_commit_failure", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		s := f.Open()
		parent := acquire(t, s, "parent", "")
		h := acquire(t, s, "child", "parent")
		c := coord(t, h)
		ref := put(t, h, "checkpoint bytes")
		calls := 0
		checkpoint := func(state *sessions.CoordinationState) error {
			calls++
			record(state, `{"generation":1,"delivered":true}`)
			seq := state.Sequence
			state.ExpectedSequence = &seq
			state.Append = []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Content: "checkpoint"}}
			state.Pins = []string{ref.ID}
			return nil
		}
		verifyEmpty := func() {
			t.Helper()
			state, err := c.ReadCoordination(t.Context())
			must(t, err)
			history, err := h.GetHistory(t.Context())
			must(t, err)
			if len(state.Records) != 0 || len(history) != 0 || state.Sequence != 0 {
				t.Fatalf("partial checkpoint: %+v / %+v", state, history)
			}
			_, r, err := coord(t, parent).OpenPublishedArtifact(t.Context(), ref.ID)
			if err == nil {
				r.Close()
				t.Fatal("rollback leaked publication")
			}
		}
		veto := errors.New("callback veto")
		err := c.UpdateCoordination(t.Context(), func(state *sessions.CoordinationState) error { checkpoint(state); return veto })
		if !errors.Is(err, veto) {
			t.Fatal(err)
		}
		verifyEmpty()
		restore := f.FailCommit()
		err = c.UpdateCoordination(t.Context(), checkpoint)
		restore()
		if err == nil {
			t.Fatal("commit failure not observed")
		}
		if calls != 2 {
			t.Fatalf("callback retried: %d", calls)
		}
		verifyEmpty()
		// A successful commit whose reply the caller loses is ambiguous. Do not
		// retry: reopen and inspect the saved receipt and transcript together.
		must(t, c.UpdateCoordination(t.Context(), checkpoint))
		if calls != 3 {
			t.Fatalf("callback retried: %d", calls)
		}
		must(t, s.Close())
		s = f.Open()
		h = acquire(t, s, "child", "parent")
		c = coord(t, h)
		state, err := c.ReadCoordination(t.Context())
		must(t, err)
		history, err := h.GetHistory(t.Context())
		must(t, err)
		if state.Sequence != 1 || len(history) != 1 || string(state.Records["contract"]["checkpoint"]) != `{"generation":1,"delivered":true}` {
			t.Fatalf("checkpoint not durable: %+v / %+v", state, history)
		}
		stale := int64(0)
		err = c.UpdateCoordination(t.Context(), func(state *sessions.CoordinationState) error {
			record(state, `{"generation":2}`)
			state.ExpectedSequence = &stale
			state.Append = []messages.ChatMessage{{Content: "duplicate"}}
			return nil
		})
		if err == nil {
			t.Fatal("stale sequence accepted")
		}
		state, err = c.ReadCoordination(t.Context())
		must(t, err)
		if state.Sequence != 1 || string(state.Records["contract"]["checkpoint"]) != `{"generation":1,"delivered":true}` {
			t.Fatal("stale checkpoint mutated receipt")
		}
		state.Records["contract"]["checkpoint"][0] = '!'
		state, err = c.ReadCoordination(t.Context())
		must(t, err)
		if !json.Valid(state.Records["contract"]["checkpoint"]) {
			t.Fatal("coordination snapshot aliases store")
		}
	})
	t.Run("member_retention_is_atomic", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		s := f.Open()
		parent := acquire(t, s, "parent", "")
		child := acquire(t, s, "child", "parent")
		parentID, childID := coord(t, parent).ViewID(), coord(t, child).ViewID()
		checkpoint := func(state *sessions.CoordinationState) error {
			record(state, `{"paused":true}`)
			state.Members = []string{childID}
			state.Append = []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "keep family"}}
			return nil
		}
		restore := f.FailCommit()
		err := coord(t, parent).UpdateCoordination(t.Context(), checkpoint)
		restore()
		if err == nil {
			t.Fatal("commit failure not observed")
		}
		must(t, child.Close())
		f.ExpireNow(childID)
		must(t, s.Expire(t.Context()))
		exists, err := s.Exists(t.Context(), "child")
		must(t, err)
		if exists {
			t.Fatal("failed commit leaked member retention")
		}
		child = acquire(t, s, "child", "parent")
		childID = coord(t, child).ViewID()
		must(t, coord(t, parent).UpdateCoordination(t.Context(), checkpoint))
		must(t, s.Close())
		f.ExpireNow(parentID)
		f.ExpireNow(childID)
		s = f.Open()
		must(t, s.Expire(t.Context()))
		for _, id := range []string{parentID, childID} {
			v := view(t, s, sessions.ViewTarget{ID: id}, "")
			h, err := s.Acquire(t.Context(), v.Metadata.Name, sessions.AcquireOptions{ExpectedID: id})
			must(t, err)
			must(t, h.Close())
		}
	})
	t.Run("family_updates_serialize", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		s := f.Open()
		parent := acquire(t, s, "parent", "")
		a := acquire(t, s, "a", "parent")
		b := acquire(t, s, "b", "parent")
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, h := range []sessions.Session{a, b} {
			c := coord(t, h)
			wg.Go(func() {
				for range 10 {
					err := c.UpdateCoordination(t.Context(), func(state *sessions.CoordinationState) error {
						n := 0
						if value := state.Records["contract"]["checkpoint"]; value != nil {
							if err := json.Unmarshal(value, &n); err != nil {
								return err
							}
						}
						raw, _ := json.Marshal(n + 1)
						record(state, string(raw))
						return nil
					})
					if err != nil {
						errs <- err
						return
					}
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			must(t, err)
		}
		state, err := coord(t, parent).ReadCoordination(t.Context())
		must(t, err)
		if string(state.Records["contract"]["checkpoint"]) != "20" {
			t.Fatalf("lost update: %+v", state.Records)
		}
		display := s.(sessions.CoordinationViewStore)
		read, err := display.ReadCoordinationView(t.Context(), coord(t, parent).ViewID())
		must(t, err)
		read.Records["contract"]["checkpoint"][0] = '9'
		read, err = display.ReadCoordinationView(t.Context(), coord(t, parent).ViewID())
		must(t, err)
		if string(read.Records["contract"]["checkpoint"]) != "20" {
			t.Fatal("display aliases records")
		}
		if _, err = display.ReadCoordinationView(t.Context(), coord(t, a).ViewID()); !errors.Is(err, sessions.ErrSessionNotFound) {
			t.Fatalf("child accepted as root: %v", err)
		}
	})
	t.Run("publication_and_collection", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		s := f.Open()
		parent := acquire(t, s, "parent", "")
		author := acquire(t, s, "author", "parent")
		sibling := acquire(t, s, "sibling", "parent")
		outsider := acquire(t, s, "outsider", "")
		ref := put(t, author, "published bytes")
		if _, r, err := coord(t, sibling).OpenPublishedArtifact(t.Context(), ref.ID); err == nil {
			r.Close()
			t.Fatal("unpublished private bytes exposed")
		}
		must(t, coord(t, parent).UpdateCoordination(t.Context(), func(state *sessions.CoordinationState) error {
			state.Members = []string{coord(t, author).ViewID(), coord(t, sibling).ViewID()}
			return nil
		}))
		err := coord(t, sibling).UpdateCoordination(t.Context(), func(state *sessions.CoordinationState) error {
			state.PinOwner = coord(t, author).ViewID()
			state.Pins = []string{ref.ID}
			return nil
		})
		if err == nil {
			t.Fatal("child published sibling bytes")
		}
		must(t, coord(t, parent).UpdateCoordination(t.Context(), func(state *sessions.CoordinationState) error {
			state.PinOwner = coord(t, author).ViewID()
			state.Pins = []string{ref.ID}
			return nil
		}))
		if _, r, err := coord(t, outsider).OpenPublishedArtifact(t.Context(), ref.ID); err == nil {
			r.Close()
			t.Fatal("cross-family publication exposed")
		}
		must(t, author.Close())
		must(t, s.Delete(t.Context(), "author"))
		if !f.HasBlob(ref.ID) {
			t.Fatal("publication collected with author")
		}
		must(t, s.Close())
		s = f.Open()
		sibling = acquire(t, s, "sibling", "parent")
		got, reader, err := coord(t, sibling).OpenPublishedArtifact(t.Context(), ref.ID)
		must(t, err)
		if got.ID != ref.ID || contents(t, reader) != "published bytes" {
			t.Fatal("publication lost on reopen")
		}
		must(t, s.Delete(t.Context(), "parent"))
		if !f.HasBlob(ref.ID) {
			t.Fatal("private reader ownership collected with parent")
		}
		reader, err = sibling.ArtifactStore().Open(t.Context(), ref.ID)
		must(t, err)
		if contents(t, reader) != "published bytes" {
			t.Fatal("private ownership not durable")
		}
		must(t, sibling.ArtifactStore().RemoveAll(t.Context()))
		if f.HasBlob(ref.ID) {
			t.Fatal("unowned blob retained")
		}
	})
}
