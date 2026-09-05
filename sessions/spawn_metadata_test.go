package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSpawnMetadataRoundTripAndRenames(t *testing.T) {
	ctx := context.Background()
	store, path := openTestStore(t, ModeDisk, nil, 0)
	parent := acquireNamed(t, store, "parent")
	child, err := store.Acquire(ctx, "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	md, err := child.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	md.SpawnCallID, md.SpawnOutcome = "call-42", ReportCanceled
	if err := child.SetMetadata(ctx, md); err != nil {
		t.Fatal(err)
	}
	if err := parent.Rename(ctx, "renamed-parent"); err != nil {
		t.Fatal(err)
	}
	if err := child.Rename(ctx, "renamed-child"); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	summaries, err := reopened.ListSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, summary := range summaries {
		md := summary.Metadata
		if md.Name != "renamed-child" {
			continue
		}
		found = true
		if md.Parent != "renamed-parent" || md.SpawnCallID != "call-42" || md.SpawnOutcome != ReportCanceled {
			t.Fatalf("restored metadata = %+v", md)
		}
	}
	if !found {
		t.Fatal("renamed child missing")
	}
	legacy, err := json.Marshal(&Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacy), "spawn") {
		t.Fatalf("optional fields not omitted: %s", legacy)
	}
}

func TestAcquireExistingOnlyNeverCreatesSession(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	if _, err := store.Acquire(ctx, "missing", AcquireOptions{ExistingOnly: true}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing lookup = %v", err)
	}
	if exists, err := store.Exists(ctx, "missing"); err != nil || exists {
		t.Fatalf("lookup created a session: %v / %v", exists, err)
	}
	session := acquireNamed(t, store, "saved")
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	session, err := store.Acquire(ctx, "saved", AcquireOptions{ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
}

func TestSpawnMetadataSurvivesStaleSettingsWritesAndReset(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	session := acquireNamed(t, store, "child")
	stale, err := session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := *stale
	first.SpawnCallID, first.SpawnOutcome = "initial", ReportFinished
	if err := session.SetMetadata(ctx, &first); err != nil {
		t.Fatal(err)
	}
	stale.Model = "follow-up-model"
	if err := session.SetMetadata(ctx, stale); err != nil {
		t.Fatal(err)
	}
	md, err := session.GetMetadata(ctx)
	if err != nil || md.Model != "follow-up-model" || md.SpawnCallID != "initial" || md.SpawnOutcome != ReportFinished {
		t.Fatalf("stale write erased initial run: %+v / %v", md, err)
	}
	stale.SpawnCallID, stale.SpawnOutcome = "later", ReportFailed
	if err := session.Reset(ctx, stale); err != nil {
		t.Fatal(err)
	}
	md, err = session.GetMetadata(ctx)
	if err != nil || md.SpawnCallID != "initial" || md.SpawnOutcome != ReportFinished {
		t.Fatalf("reset changed initial run: %+v / %v", md, err)
	}
}
