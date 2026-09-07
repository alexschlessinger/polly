package sessions

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

func skipIfWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX process-wide locks")
	}
}

func TestDiskStorePreservesLocksAcrossProcesses(t *testing.T) {
	skipIfWindows(t)
	const helperPath = "POLLYTOOL_TEST_LOCK_DATABASE"
	if path := os.Getenv(helperPath); path != "" {
		store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		session := acquireNamed(t, store, "child-process")
		if err := session.AddMessage(context.Background(), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "child"}); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	store, path := openTestStore(t, ModeDisk, nil, 0)
	parent := acquireNamed(t, store, "parent-process")
	ctx := context.Background()
	if err := parent.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "before"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	childCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	child := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestDiskStorePreservesLocksAcrossProcesses$")
	child.Env = append(os.Environ(), helperPath+"="+path)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child process: %v\n%s", err, out)
	}
	after, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatalf("child removed the live parent's WAL: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("child replaced the live parent's WAL")
	}
	if err := parent.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "after"}); err != nil {
		t.Fatal(err)
	}
	for name, count := range map[string]int{"parent-process": 2, "child-process": 1} {
		view, err := store.ReadView(ctx, ViewTarget{Name: name}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(view.History) != count {
			t.Fatalf("%s has %d messages, want %d", name, len(view.History), count)
		}
	}
}
