package sessions

import (
	"context"
	"testing"
)

func TestSummaryParentIdentitySurvivesRenameAndDeletion(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	parent := acquireNamed(t, store, "parent")
	id := parent.(ViewIdentity).ViewID()
	child, err := store.Acquire(ctx, "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	check := func(wantID, wantName string) {
		t.Helper()
		summaries, err := store.ListSummaries(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range summaries {
			if s.Metadata.Name == "child" {
				if s.ParentID != wantID || s.Metadata.Parent != wantName {
					t.Fatalf("parent = %q %q, want %q %q", s.ParentID, s.Metadata.Parent, wantID, wantName)
				}
				return
			}
		}
		t.Fatal("child summary missing")
	}
	check(id, "parent")
	if err := parent.Rename(ctx, "renamed"); err != nil {
		t.Fatal(err)
	}
	check(id, "renamed")
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "renamed"); err != nil {
		t.Fatal(err)
	}
	replacement := acquireNamed(t, store, "renamed")
	defer replacement.Close()
	// Deletion keeps the stored display hint, never the replacement's identity.
	check("", "parent")
}
