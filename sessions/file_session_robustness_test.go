package sessions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/gofrs/flock"
)

// #1: a non-empty but unparseable session file must NOT be silently discarded.
// The original bytes must be preserved (backed up) and a fresh session returned.
func TestFileStore_CorruptFileIsPreservedNotLost(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileSessionStore(dir, &Metadata{SystemPrompt: "sys"})
	if err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(dir, "ctx.json")
	garbage := []byte(`{"id":"ctx","history":[ TRUNCATED`)
	if err := os.WriteFile(p, garbage, 0600); err != nil {
		t.Fatal(err)
	}

	sess, err := store.Get("ctx")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	sess.Close()

	backup, err := os.ReadFile(p + ".corrupt")
	if err != nil {
		t.Fatalf("expected corrupt content preserved at %s.corrupt: %v", p, err)
	}
	if string(backup) != string(garbage) {
		t.Errorf("backup content mismatch:\n got %q\nwant %q", backup, garbage)
	}
}

// #1: an existing, valid session must still round-trip, and saving must not
// leave temp/partial files behind in the directory.
func TestFileStore_SaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSessionStore(dir, &Metadata{SystemPrompt: "sys"})
	sess, _ := store.Get("ctx")
	for range 5 {
		sess.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "m"})
	}
	sess.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file in store dir: %s", e.Name())
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, "ctx.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fs FileSession
	if err := json.Unmarshal(data, &fs); err != nil {
		t.Fatalf("saved session file is not valid JSON: %v", err)
	}
}

// #2: Range is read-only iteration; it must not modify sessions on disk.
func TestFileStore_RangeDoesNotMutate(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSessionStore(dir, &Metadata{})
	sess, _ := store.Get("a")
	sess.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "x"})
	sess.Close()

	p := filepath.Join(dir, "a.json")
	fiBefore, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	before := fiBefore.ModTime()

	time.Sleep(10 * time.Millisecond)
	count := 0
	store.Range(func(k, v any) bool { count++; return true })
	if count != 1 {
		t.Fatalf("Range visited %d sessions, want 1", count)
	}

	fiAfter, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fiAfter.ModTime().Equal(before) {
		t.Errorf("Range mutated the session file: mtime changed %v -> %v", before, fiAfter.ModTime())
	}
}

// #2: Range must not leak file locks. After iterating, the lock guarding a
// session must be free to acquire.
func TestFileStore_RangeDoesNotLeakLocks(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSessionStore(dir, &Metadata{})
	sess, _ := store.Get("a")
	sess.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "x"})
	sess.Close()

	store.Range(func(k, v any) bool { return true })

	fl := flock.New(lockPath(filepath.Join(dir, "a.json")))
	locked, err := fl.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("Range leaked the session lock: TryLock denied after Range")
	}
	_ = fl.Unlock()
}

// #2: Range callbacks receive detached snapshots; mutating the callback value
// must not perform an unlocked write to the backing session file.
func TestFileStore_RangeMutationDoesNotWriteUnlocked(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSessionStore(dir, &Metadata{})
	sess, _ := store.Get("a")
	sess.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "x"})
	sess.Close()

	p := filepath.Join(dir, "a.json")
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	count := 0
	store.Range(func(k, v any) bool {
		count++
		fs, ok := v.(*FileSession)
		if !ok {
			t.Fatalf("Range yielded %T, want *FileSession", v)
		}
		fs.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "should-not-save"})
		if err := fs.UpdateMetadata(&Metadata{Description: "should-not-save"}); err == nil {
			t.Fatal("UpdateMetadata on a detached Range snapshot unexpectedly succeeded")
		}
		return true
	})
	if count != 1 {
		t.Fatalf("Range visited %d sessions, want 1", count)
	}

	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("Range callback mutation wrote to the backing session file")
	}
}

// #1 regression guard: atomic saves replace the data file's inode via rename.
// Verify mutual exclusion still holds across a save — i.e. the lock is bound to
// the dedicated lock file, not the (replaced) data file inode.
func TestFileStore_LockSurvivesAtomicSave(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSessionStore(dir, &Metadata{})
	sess, _ := store.Get("a")
	// Trigger several atomic saves (each renames a new inode over a.json).
	for range 3 {
		sess.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "x"})
	}

	// While the session is open, the lock must still be denied to others.
	fl := flock.New(lockPath(filepath.Join(dir, "a.json")))
	locked, err := fl.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	if locked {
		_ = fl.Unlock()
		t.Fatal("lock was acquirable while session open: atomic save dropped the lock")
	}

	// After Close, the lock is released.
	sess.Close()
	locked, err = fl.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("lock still held after Close")
	}
	_ = fl.Unlock()
}

// #1: Delete must not remove or replace the stable lock file while a session is
// open; otherwise a later Get can coordinate on a different inode.
func TestFileStore_DeleteSkipsOpenSessionAndKeepsStableLock(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSessionStore(dir, &Metadata{})
	sess, err := store.Get("a")
	if err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(dir, "a.json")
	lock := lockPath(p)
	before, err := os.Stat(lock)
	if err != nil {
		t.Fatal(err)
	}

	store.Delete("a")
	if !store.Exists("a") {
		t.Fatal("Delete removed an in-use session")
	}
	afterOpenDelete, err := os.Stat(lock)
	if err != nil {
		t.Fatal("Delete removed the lock file for an open session")
	}
	if !os.SameFile(before, afterOpenDelete) {
		t.Fatal("Delete replaced the lock file for an open session")
	}

	probe := flock.New(lock)
	locked, err := probe.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	if locked {
		_ = probe.Unlock()
		t.Fatal("lock was acquirable while session remained open after Delete")
	}

	sess.Close()
	store.Delete("a")
	if store.Exists("a") {
		t.Fatal("Delete did not remove a closed session")
	}
	afterClosedDelete, err := os.Stat(lock)
	if err != nil {
		t.Fatal("Delete removed the stable lock file")
	}
	if !os.SameFile(before, afterClosedDelete) {
		t.Fatal("Delete replaced the stable lock file")
	}
}

// #1: calling Delete on an open session must not split the lock namespace for
// a later Get. The later Get should wait on the same lock until Close.
func TestFileStore_DeleteOpenSessionDoesNotSplitLockForGet(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSessionStore(dir, &Metadata{})
	sess, err := store.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	store.Delete("a")

	done := make(chan error, 1)
	go func() {
		next, err := store.Get("a")
		if err == nil {
			next.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("Get returned while original session was still open: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	sess.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Get after Close returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Get did not proceed after original session closed")
	}
}

// #4: Delete must validate the name and never touch paths outside the store.
func TestFileStore_DeleteRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileSessionStore(filepath.Join(root, "store"), nil)
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "victim.json")
	if err := os.WriteFile(victim, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}

	store.Delete("../victim")

	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("Delete escaped the store dir and removed %s: %v", victim, err)
	}
}

// #1: if corrupt-file backup cannot be created, Get must fail closed and leave
// the corrupt bytes in place instead of overwriting them with a fresh session.
func TestFileStore_CorruptBackupFailureDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileSessionStore(dir, &Metadata{})
	if err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(dir, "ctx.json")
	garbage := []byte(`{"id":"ctx","history":[ TRUNCATED`)
	if err := os.WriteFile(p, garbage, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath(p), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)

	sess, err := store.Get("ctx")
	if err == nil {
		sess.Close()
		t.Skip("directory permissions did not block corrupt backup on this platform")
	}

	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(garbage) {
		t.Fatalf("corrupt session was overwritten after backup failure:\n got %q\nwant %q", data, garbage)
	}
}

// #4: Exists must validate the name rather than statting arbitrary paths.
func TestFileStore_ExistsRejectsInvalidName(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileSessionStore(filepath.Join(root, "store"), nil)
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "victim.json")
	if err := os.WriteFile(victim, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	if store.Exists("../victim") {
		t.Fatal("Exists returned true for an out-of-store traversal name")
	}
}

// #3: Expire honors a configured TTL instead of a hardcoded duration.
func TestFileStore_ExpireHonorsConfiguredTTL(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileSessionStore(dir, &Metadata{TTL: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := store.Get("a")
	sess.Close() // release the lock so Expire can claim it

	time.Sleep(40 * time.Millisecond)
	store.Expire()

	if store.Exists("a") {
		t.Fatal("session older than its configured TTL was not expired")
	}
	if _, err := os.Stat(lockPath(filepath.Join(dir, "a.json"))); err != nil {
		t.Fatal("Expire removed the stable lock file")
	}
}

// #3: with no TTL configured, recent sessions are retained (safety-net default).
func TestFileStore_ExpireKeepsRecentSessionWithoutTTL(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileSessionStore(dir, &Metadata{}) // no TTL
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := store.Get("a")
	sess.Close()

	store.Expire()

	if !store.Exists("a") {
		t.Fatal("recent session with no TTL should be retained")
	}
}

// Batch append must persist with a single disk write, not one per message.
// executeTurn replays a whole agentic turn (the assistant message for each
// iteration plus every tool result) into the session at once; doing that one
// AddMessage at a time rewrites the entire history file once per message.
func TestFileStore_AddMessagesPersistsOnce(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSessionStore(dir, &Metadata{})
	sess, _ := store.Get("a")
	defer sess.Close()

	// Count writes after creation so Get's own save doesn't skew the count.
	orig := atomicWriteFile
	t.Cleanup(func() { atomicWriteFile = orig })
	writes := 0
	atomicWriteFile = func(path string, data []byte, perm os.FileMode) error {
		writes++
		return orig(path, data, perm)
	}

	sess.AddMessages([]messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, Content: "calling tools"},
		{Role: messages.MessageRoleTool, Content: "result-1"},
		{Role: messages.MessageRoleTool, Content: "result-2"},
		{Role: messages.MessageRoleAssistant, Content: "done"},
	})
	if writes != 1 {
		t.Fatalf("AddMessages persisted %d times, want 1", writes)
	}

	// An empty batch must not write at all.
	sess.AddMessages(nil)
	if writes != 1 {
		t.Fatalf("empty AddMessages wrote the file; total writes = %d, want 1", writes)
	}
}

// Batch append must store every message, in order.
func TestFileStore_AddMessagesAppendsAllInOrder(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSessionStore(dir, &Metadata{})
	sess, _ := store.Get("a")
	defer sess.Close()

	sess.AddMessages([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "one"},
		{Role: messages.MessageRoleAssistant, Content: "two"},
	})

	h := sess.GetHistory()
	if len(h) != 2 || h[0].Content != "one" || h[1].Content != "two" {
		t.Fatalf("history = %+v, want [one two] in order", h)
	}
}

// #6: opening an existing, valid session is a read; it must not rewrite the
// file. Loading on every Get re-marshals + fsyncs + renames the whole history
// for no reason and bumps mtime/last-used merely by looking at a context.
func TestFileStore_GetDoesNotWriteOnRead(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSessionStore(dir, &Metadata{})
	sess, _ := store.Get("a")
	sess.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "x"})
	sess.Close()

	// Count writes only for the reopen below.
	orig := atomicWriteFile
	t.Cleanup(func() { atomicWriteFile = orig })
	writes := 0
	atomicWriteFile = func(path string, data []byte, perm os.FileMode) error {
		writes++
		return orig(path, data, perm)
	}

	reopened, err := store.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()

	if writes != 0 {
		t.Fatalf("Get wrote %d times when loading an existing session, want 0", writes)
	}
}

// Sessions with valid JSON but no metadata are invalid. There is intentionally
// no backwards-compatibility migration for old metadata-free files.
func TestFileStore_GetErrorsOnNilMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{
			name: "null metadata",
			data: `{"id":"a","history":[],"created":"2026-01-01T00:00:00Z","updated":"2026-01-01T00:00:00Z","metadata":null}`,
		},
		{
			name: "missing metadata",
			data: `{"id":"a","history":[],"created":"2026-01-01T00:00:00Z","updated":"2026-01-01T00:00:00Z"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := NewFileSessionStore(dir, &Metadata{})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}

			sess, err := store.Get("a")
			if err == nil {
				sess.Close()
				t.Fatal("Get succeeded for a session without metadata")
			}
		})
	}
}

func TestFileStore_MutatorSaveFailureLeavesMemoryUnchanged(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSessionStore(dir, &Metadata{SystemPrompt: "sys"})
	sess, _ := store.Get("a")
	defer sess.Close()

	if err := sess.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "kept"}); err != nil {
		t.Fatal(err)
	}

	orig := atomicWriteFile
	t.Cleanup(func() { atomicWriteFile = orig })
	atomicWriteFile = func(path string, data []byte, perm os.FileMode) error {
		return fmt.Errorf("disk full")
	}

	beforeHistory := sess.GetHistory()
	beforeMetadata := cloneMetadata(sess.GetMetadata())
	beforeUpdated := sess.GetLastUsed()

	checkUnchanged := func(t *testing.T) {
		t.Helper()
		if got := sess.GetHistory(); !reflect.DeepEqual(got, beforeHistory) {
			t.Fatalf("history changed after failed save:\n got %+v\nwant %+v", got, beforeHistory)
		}
		if got := sess.GetMetadata(); !reflect.DeepEqual(got, beforeMetadata) {
			t.Fatalf("metadata changed after failed save:\n got %+v\nwant %+v", got, beforeMetadata)
		}
		if got := sess.GetLastUsed(); !got.Equal(beforeUpdated) {
			t.Fatalf("updated changed after failed save: got %v want %v", got, beforeUpdated)
		}
	}

	if err := sess.AddMessages([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "lost"}}); err == nil {
		t.Fatal("AddMessages returned nil on save failure")
	}
	checkUnchanged(t)

	if err := sess.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "lost-one"}); err == nil {
		t.Fatal("AddMessage returned nil on save failure")
	}
	checkUnchanged(t)

	if err := sess.Clear(); err == nil {
		t.Fatal("Clear returned nil on save failure")
	}
	checkUnchanged(t)

	if err := sess.SetMetadata(&Metadata{Name: "changed", Description: "lost"}); err == nil {
		t.Fatal("SetMetadata returned nil on save failure")
	}
	checkUnchanged(t)

	metadataCopy := sess.GetMetadata()
	metadataCopy.Description = "lost-through-copy"
	if err := sess.SetMetadata(metadataCopy); err == nil {
		t.Fatal("SetMetadata with a modified metadata copy returned nil on save failure")
	}
	checkUnchanged(t)

	if err := sess.UpdateMetadata(&Metadata{Description: "lost"}); err == nil {
		t.Fatal("UpdateMetadata returned nil on save failure")
	}
	checkUnchanged(t)
}

func TestFileStore_MutatorsUpdatePersistedLastUsed(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSessionStore(dir, &Metadata{SystemPrompt: "sys"})
	sess, _ := store.Get("a")
	defer sess.Close()

	assertLastUsedAdvances := func(label string, mutate func() error) {
		t.Helper()
		before := readPersistedMetadata(t, dir, "a").LastUsed
		time.Sleep(time.Millisecond)
		if err := mutate(); err != nil {
			t.Fatalf("%s failed: %v", label, err)
		}
		after := readPersistedMetadata(t, dir, "a").LastUsed
		if !after.After(before) {
			t.Fatalf("%s did not advance LastUsed: before %v after %v", label, before, after)
		}
	}

	assertLastUsedAdvances("AddMessage", func() error {
		return sess.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "one"})
	})
	assertLastUsedAdvances("AddMessages", func() error {
		return sess.AddMessages([]messages.ChatMessage{{Role: messages.MessageRoleAssistant, Content: "two"}})
	})
	assertLastUsedAdvances("Clear", sess.Clear)
	assertLastUsedAdvances("SetMetadata", func() error {
		return sess.SetMetadata(&Metadata{Name: "a", Created: sess.GetMetadata().Created, SystemPrompt: "sys"})
	})
	assertLastUsedAdvances("UpdateMetadata", func() error {
		return sess.UpdateMetadata(&Metadata{Description: "fresh"})
	})
}

func TestFileStore_SetMetadataNilReturnsErrorAndLeavesMetadata(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSessionStore(dir, &Metadata{})
	sess, _ := store.Get("a")
	defer sess.Close()

	before := cloneMetadata(sess.GetMetadata())
	if err := sess.SetMetadata(nil); err == nil {
		t.Fatal("SetMetadata(nil) returned nil")
	}
	if got := sess.GetMetadata(); !reflect.DeepEqual(got, before) {
		t.Fatalf("metadata changed after SetMetadata(nil):\n got %+v\nwant %+v", got, before)
	}
}

// #5: mutators must surface persistence failures instead of dropping them.
// A caller that appends a message and gets no error must be able to trust the
// message reached disk.
func TestFileStore_MutatorsReturnSaveErrors(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSessionStore(dir, &Metadata{SystemPrompt: "sys"})
	sess, _ := store.Get("a")
	defer sess.Close()

	orig := atomicWriteFile
	t.Cleanup(func() { atomicWriteFile = orig })
	fail := false
	atomicWriteFile = func(path string, data []byte, perm os.FileMode) error {
		if fail {
			return fmt.Errorf("disk full")
		}
		return orig(path, data, perm)
	}

	fail = true
	if err := sess.AddMessages([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "x"}}); err == nil {
		t.Error("AddMessages dropped a save error")
	}
	if err := sess.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "y"}); err == nil {
		t.Error("AddMessage dropped a save error")
	}
	if err := sess.Clear(); err == nil {
		t.Error("Clear dropped a save error")
	}
	if err := sess.SetMetadata(&Metadata{Name: "a"}); err == nil {
		t.Error("SetMetadata dropped a save error")
	}

	// When the write succeeds, no error is reported.
	fail = false
	if err := sess.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "z"}); err != nil {
		t.Errorf("AddMessage returned an error on a successful save: %v", err)
	}
}

func readPersistedMetadata(t *testing.T, dir, name string) *Metadata {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var session FileSession
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	if session.Metadata == nil {
		t.Fatal("persisted session has nil metadata")
	}
	return session.Metadata
}
