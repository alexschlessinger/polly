package sessions

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestSessionViewDoesNotLeaseOrTouchAndArtifactsOutliveWriter(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	session := acquireNamed(t, store, "child")
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	ref, err := session.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindText, Data: []byte("saved output")})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := session.GetLastUsed(ctx)
	view, err := store.ReadView(ctx, ViewTarget{Name: "child"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !view.InUse || view.ID != session.(ViewIdentity).ViewID() {
		t.Fatalf("identity/lease: %+v", view)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if got := string(readArtifact(t, view.Artifacts, ref.ID)); got != "saved output" {
		t.Fatal(got)
	}
	if _, err := view.Artifacts.Put(ctx, artifacts.Blob{Data: []byte("no")}); !errors.Is(err, ErrReadOnlyView) {
		t.Fatal(err)
	}
	if err := view.Artifacts.RemoveAll(ctx); !errors.Is(err, ErrReadOnlyView) {
		t.Fatal(err)
	}
	fresh, err := store.ReadView(ctx, ViewTarget{ID: view.ID}, view.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.InUse || !fresh.Unchanged || fresh.History != nil || !fresh.Metadata.LastUsed.Equal(before) {
		t.Fatalf("view changed session: %+v", fresh)
	}
	other := acquireNamed(t, store, "other")
	otherView, err := store.ReadView(ctx, ViewTarget{Name: "other"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherView.Artifacts.Open(ctx, ref.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cross-session artifact: %v", err)
	}
	_ = other.Close()
	reader, err := view.Artifacts.Open(ctx, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	if _, err := io.ReadAll(reader); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("reader survived store close: %v", err)
	}
	_ = reader.Close()
}

func TestSessionViewIdentityRevisionAndReusedNames(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	parent := acquireNamed(t, store, "parent")
	child, err := store.Acquire(ctx, "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	md, _ := child.GetMetadata(ctx)
	md.SpawnCallID = "spawn-1"
	if err := child.SetMetadata(ctx, md); err != nil {
		t.Fatal(err)
	}
	if err := child.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "first"}); err != nil {
		t.Fatal(err)
	}
	view, err := store.ReadView(ctx, ViewTarget{Parent: "parent", SpawnCallID: "spawn-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Rename(ctx, "caller"); err != nil {
		t.Fatal(err)
	}
	if err := child.Rename(ctx, "renamed"); err != nil {
		t.Fatal(err)
	}
	renamed, err := store.ReadView(ctx, ViewTarget{ID: view.ID}, view.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Unchanged || renamed.Metadata.Name != "renamed" || renamed.Metadata.Parent != "caller" {
		t.Fatalf("rename lost: %+v", renamed)
	}
	if err := child.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if err := child.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "replacement"}); err != nil {
		t.Fatal(err)
	}
	changed, err := store.ReadView(ctx, ViewTarget{ID: view.ID}, renamed.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Unchanged || len(changed.History) != len(view.History) || changed.History[len(changed.History)-1].Content != "replacement" {
		t.Fatalf("reset did not invalidate: %+v", changed)
	}
	_ = child.Close()
	if err := store.Delete(ctx, "renamed"); err != nil {
		t.Fatal(err)
	}
	replacement := acquireNamed(t, store, "renamed")
	_ = replacement.Close()
	if _, err := store.ReadView(ctx, ViewTarget{ID: view.ID}, ""); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal(err)
	}
	if _, err := store.Acquire(ctx, "renamed", AcquireOptions{ExpectedID: view.ID}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("reused name acquired: %v", err)
	}
	if _, err := store.Acquire(ctx, "missing", AcquireOptions{ExpectedID: view.ID}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing name created: %v", err)
	}
}

func TestSessionViewRevisionAdvancesWhenWallClockDoesNot(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	session := acquireNamed(t, store, "clock")
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "first"}); err != nil {
		t.Fatal(err)
	}
	// A future stored timestamp deterministically exercises a repeated or
	// backward clock tick without depending on the host timer's resolution.
	previous := time.Now().Add(time.Minute).UnixNano()
	if _, err := store.db.ExecContext(ctx, "UPDATE sessions SET updated_ns=? WHERE id=?", previous, session.(*sqliteSession).id); err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadView(ctx, ViewTarget{Name: "clock"}, "")
	if err != nil {
		t.Fatal(err)
	}
	md, err := session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, update := range []func() error{
		func() error { return session.Clear(ctx) },
		func() error {
			return session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "replacement"})
		},
		func() error { return session.Reset(ctx, md) },
		func() error { md.Description = "updated"; return session.SetMetadata(ctx, md) },
		func() error { return session.Rename(ctx, "renamed-clock") },
	} {
		if err := update(); err != nil {
			t.Fatal(err)
		}
		after, err := store.ReadView(ctx, ViewTarget{ID: before.ID}, before.Revision)
		if err != nil {
			t.Fatal(err)
		}
		if after.Unchanged || after.Metadata.LastUsed.UnixNano() <= previous {
			t.Fatalf("revision did not advance: unchanged=%t last=%d previous=%d", after.Unchanged, after.Metadata.LastUsed.UnixNano(), previous)
		}
		before, previous = after, after.Metadata.LastUsed.UnixNano()
	}
}

func TestSessionViewRejectsAmbiguousChildrenAndSequenceLoss(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	_ = acquireNamed(t, store, "parent")
	for _, name := range []string{"one", "two"} {
		child, err := store.Acquire(ctx, name, AcquireOptions{Parent: "parent"})
		if err != nil {
			t.Fatal(err)
		}
		md, _ := child.GetMetadata(ctx)
		md.SpawnCallID = "same"
		if err := child.SetMetadata(ctx, md); err != nil {
			t.Fatal(err)
		}
		_ = child.Close()
	}
	if _, err := store.ReadView(ctx, ViewTarget{Parent: "parent", SpawnCallID: "same"}, ""); err == nil {
		t.Fatal("ambiguous child accepted")
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE sessions SET next_sequence=next_sequence+1 WHERE name='one'"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadView(ctx, ViewTarget{Name: "one"}, ""); err == nil {
		t.Fatal("missing messages accepted")
	}
}

// A view must agree with Acquire about expiry: a session past its TTL is
// still served while a lease holds it (Acquire keeps it too), and refused
// once released (Acquire would retire it), so a follow-up never lands on a
// conversation the view just showed but the store no longer has.
func TestSessionViewRefusesExpiredSessionUnlessLeased(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, &Metadata{TTL: time.Second}, 0)
	ctx := context.Background()
	session := acquireNamed(t, store, "stale")
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "keep me"}); err != nil {
		t.Fatal(err)
	}
	id := session.(ViewIdentity).ViewID()
	backdate := func() {
		t.Helper()
		if _, err := store.db.ExecContext(ctx, "UPDATE sessions SET updated_ns = ? WHERE id = ?", time.Now().Add(-time.Hour).UnixNano(), session.(*sqliteSession).id); err != nil {
			t.Fatal(err)
		}
	}
	backdate()
	held, err := store.ReadView(ctx, ViewTarget{ID: id}, "")
	if err != nil || !held.InUse || len(held.History) != 1 {
		t.Fatalf("expired session under a live lease: %+v, %v", held, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	backdate()
	if _, err := store.ReadView(ctx, ViewTarget{Name: "stale"}, ""); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expired view by name: %v", err)
	}
	if _, err := store.ReadView(ctx, ViewTarget{ID: id}, ""); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expired view by id: %v", err)
	}
	if _, err := store.Acquire(ctx, "stale", AcquireOptions{ExpectedID: id}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expired identity acquired: %v", err)
	}
}
