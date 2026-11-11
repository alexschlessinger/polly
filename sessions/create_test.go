package sessions

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Create stores a new session as its caller wants it to start, in the
// transaction that creates it: nothing of the store's defaults that the
// caller took out is ever stored.
func TestCreateStoresASessionAsItIsToStart(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, &Metadata{Model: "default-model", SystemPrompt: "default prompt"}, 0)
	acquireNamed(t, store, "parent")
	session, err := store.Create(ctx, "child", AcquireOptions{Auto: true, Parent: "parent"}, func(md *Metadata) {
		if md.Model != "default-model" || md.SystemPrompt != "default prompt" {
			t.Errorf("configure was given %+v, want the store's defaults", md)
		}
		md.Description, md.SpawnCallID, md.SwarmID = "a task", "call-1", "family"
		md.SystemPrompt = ""
		md.Title, md.TitleSource = "  A   title ", TitleSourceAgent
		md.Name, md.Parent = "ignored", "ignored"
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	md, err := session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.Name != "child" || md.Parent != "parent" || md.Model != "default-model" || md.SystemPrompt != "" ||
		md.Description != "a task" || md.SpawnCallID != "call-1" || md.SwarmID != "family" ||
		md.Title != "A title" || md.TitleSource != TitleSourceAgent {
		t.Fatalf("created session = %+v", md)
	}
	if history, err := session.GetHistory(ctx); err != nil || len(history) != 0 {
		t.Fatalf("created session's transcript = %+v, %v", history, err)
	}

	if _, err := store.Create(ctx, "child", AcquireOptions{}, nil); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("creating a session that exists = %v", err)
	}
	if _, err := store.Create(ctx, "untitled", AcquireOptions{}, func(md *Metadata) { md.Title = "bad\x00title" }); !errors.Is(err, ErrInvalidTitle) {
		t.Fatalf("creating a session with an invalid title = %v", err)
	}
	if exists, err := store.Exists(ctx, "untitled"); err != nil || exists {
		t.Fatalf("a session that could not be created exists = %v, %v", exists, err)
	}
	if _, err := store.Create(ctx, "other", AcquireOptions{ExistingOnly: true}, nil); err == nil {
		t.Fatal("Create took a session that must exist")
	}
	// Without configure the session starts with the defaults, as Acquire's.
	plain, err := store.Create(ctx, "plain", AcquireOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if history, err := plain.GetHistory(ctx); err != nil || len(history) != 1 || history[0].Content != "default prompt" {
		t.Fatalf("a session created with the defaults starts with %+v, %v", history, err)
	}
}

// Registering a member in a family this store drives is written at once and
// by itself: the records the same update changes wait for the next
// checkpoint like any others, and nothing pending is written for its sake.
func TestRegisteringAMemberDoesNotWaitForACheckpoint(t *testing.T) {
	ctx := context.Background()
	store, _, parent := drivenFamily(t)
	child, err := store.Create(ctx, "child", AcquireOptions{Auto: true, Parent: "parent"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	id := child.(*sqliteSession).id
	updateFrom(t, parent.(CoordinationSession), func(state *CoordinationState) error {
		state.Changes = map[string]map[string]json.RawMessage{"task": {"a": json.RawMessage(`{"n":2}`)}}
		state.Members = []string{hex.EncodeToString(id)}
		return nil
	})
	var registered int
	if err := store.db.QueryRow("SELECT count(*) FROM swarm_members WHERE member_id = ?", id).Scan(&registered); err != nil || registered != 1 {
		t.Fatalf("member registered %d times, %v", registered, err)
	}
	if stored := storedRecord(t, store, "task", "a"); stored != `{"n":1}` {
		t.Fatalf("the record reached the database with the registration: %s", stored)
	}
	if got := readRecord(t, parent.(CoordinationSession), "task", "a"); got != `{"n":2}` {
		t.Fatalf("the family in memory holds %s", got)
	}
	if err := store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if stored := storedRecord(t, store, "task", "a"); stored != `{"n":2}` {
		t.Fatalf("the record after a checkpoint = %s", stored)
	}
	// A member that is not the parent's child is refused, and nothing of the
	// update is kept.
	stranger := acquireNamed(t, store, "stranger")
	revision, err := parent.(CoordinationRevisionSession).CoordinationRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = parent.(CoordinationDeltaSession).UpdateCoordinationFrom(ctx, revision, func(state *CoordinationState) error {
		state.Changes = map[string]map[string]json.RawMessage{"task": {"a": json.RawMessage(`{"n":3}`)}}
		state.Members = []string{stranger.(*sqliteSession).ViewID()}
		return nil
	})
	if err == nil {
		t.Fatal("a session that is not a child was registered")
	}
	if got := readRecord(t, parent.(CoordinationSession), "task", "a"); got != `{"n":2}` {
		t.Fatalf("a refused update left %s in memory", got)
	}
}

// A handle knows its session's name: only it renames the session while its
// lease stands, so it answers without the database.
func TestHandleAnswersItsNameFromMemory(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	session := acquireNamed(t, store, "first")
	if err := session.Rename(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	// With the store's only connection taken, the database cannot be asked.
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prompt, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	name, err := session.GetName(prompt)
	if closeErr := conn.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || name != "second" {
		t.Fatalf("name = %q, %v", name, err)
	}
	// A rename that fails leaves the name to the database.
	acquireNamed(t, store, "taken")
	if err := session.Rename(ctx, "taken"); err == nil {
		t.Fatal("renamed onto a session that exists")
	}
	if name, err := session.GetName(ctx); err != nil || name != "second" {
		t.Fatalf("name after a refused rename = %q, %v", name, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.GetName(ctx); err == nil {
		t.Fatal("a closed handle answered for its name")
	}
}
