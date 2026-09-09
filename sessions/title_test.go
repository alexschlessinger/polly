package sessions

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestSessionTitleLifecycle(t *testing.T) {
	for _, mode := range []StoreMode{ModeMemory, ModeDisk} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			ctx := context.Background()
			store, path := openTestStore(t, mode, nil, 24*time.Hour)
			session, err := store.Acquire(ctx, "quiet-otter", AcquireOptions{Auto: true})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hello"}); err != nil {
				t.Fatal(err)
			}
			s := session.(*sqliteSession)
			before, explicitBefore, err := scanSnapshot(ctx, store.db, s.id)
			if err != nil {
				t.Fatal(err)
			}
			id := s.ViewID()
			viewBefore, err := store.ReadView(ctx, ViewTarget{ID: id}, "")
			if err != nil {
				t.Fatal(err)
			}
			stale, _ := session.GetMetadata(ctx)
			got, err := s.SetTitle(ctx, "  Session\n\t naming — 日本語  ", TitleSourceAgent)
			if err != nil || got != "Session naming — 日本語" {
				t.Fatalf("title = %q, %v", got, err)
			}
			after, explicitAfter, err := scanSnapshot(ctx, store.db, s.id)
			if err != nil {
				t.Fatal(err)
			}
			if after.name != before.name || after.updatedNS != before.updatedNS || after.ttlNS != before.ttlNS || after.retention != before.retention || explicitAfter != explicitBefore || s.ViewID() != id {
				t.Fatal("title changed handle, activity, retention, or identity")
			}
			view, err := store.ReadView(ctx, ViewTarget{ID: id}, viewBefore.Revision)
			if err != nil || view.Unchanged || !reflect.DeepEqual(view.History, viewBefore.History) {
				t.Fatalf("view = %+v, %v", view, err)
			}
			if _, err := s.SetTitle(ctx, got, TitleSourceUser); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetTitle(ctx, "agent overwrite", TitleSourceAgent); !errors.Is(err, ErrTitleProtected) {
				t.Fatalf("protection error = %v", err)
			}
			stale.Model = "new/model"
			if err := session.SetMetadata(ctx, stale); err != nil {
				t.Fatal(err)
			}
			if err := session.Clear(ctx); err != nil {
				t.Fatal(err)
			}
			if err := session.Reset(ctx, stale); err != nil {
				t.Fatal(err)
			}
			md, _ := session.GetMetadata(ctx)
			if md.Title != got || md.TitleSource != TitleSourceUser || md.Model != "new/model" {
				t.Fatalf("stale write/reset lost title: %+v", md)
			}
			if err := session.Rename(ctx, "polly-design"); err != nil {
				t.Fatal(err)
			}
			md, _ = session.GetMetadata(ctx)
			if md.Title != got || md.TitleSource != TitleSourceUser || md.TTL != 0 {
				t.Fatalf("manual rename changed title or retention behavior: %+v", md)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			if mode == ModeDisk {
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				store, err = OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
			}
			reopened, err := store.Acquire(ctx, "polly-design", AcquireOptions{ExistingOnly: true, ExpectedID: id})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			md, _ = reopened.GetMetadata(ctx)
			if md.Title != got || md.TitleSource != TitleSourceUser {
				t.Fatalf("reopen lost title: %+v", md)
			}
		})
	}
}

func TestSessionTitleValidationAndLegacyOwnership(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeMemory, nil, time.Hour)
	s := acquireNamed(t, store, "one").(TitleSession)
	for _, title := range []string{"", " \n\t", "bad\x00title", "bad\x7ftitle", "bad\u0080title", strings.Repeat("界", 81), string([]byte{0xff})} {
		if _, err := s.SetTitle(ctx, title, TitleSourceAgent); !errors.Is(err, ErrInvalidTitle) {
			t.Errorf("%q: %v", title, err)
		}
	}
	if _, err := s.SetTitle(ctx, "valid", "unknown"); !errors.Is(err, ErrInvalidTitle) {
		t.Fatalf("source: %v", err)
	}
	for _, name := range []string{"one", "two"} {
		var setter TitleSession = s
		if name == "two" {
			setter = acquireNamed(t, store, name).(TitleSession)
		}
		if _, err := setter.SetTitle(ctx, strings.Repeat("界", 80), TitleSourceAgent); err != nil {
			t.Fatal(err)
		}
	}
	for _, source := range []TitleSource{"", "future", TitleSourceUser, TitleSourceAgent} {
		legacy, _ := openTestStore(t, ModeMemory, &Metadata{Title: "Existing title", TitleSource: source}, time.Hour)
		session := acquireNamed(t, legacy, "legacy")
		md, _ := session.GetMetadata(ctx)
		want := TitleSourceUser
		if source == TitleSourceAgent {
			want = source
		}
		if md.TitleSource != want {
			t.Fatalf("source %q became %q", source, md.TitleSource)
		}
		_, err := session.(TitleSession).SetTitle(ctx, "replacement", TitleSourceAgent)
		if want == TitleSourceUser && !errors.Is(err, ErrTitleProtected) {
			t.Fatalf("legacy overwrite: %v", err)
		}
	}
}

func TestSessionTitleManualWriteWinsRace(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, time.Hour)
	for n := 0; n < 12; n++ {
		s := acquireNamed(t, store, fmt.Sprintf("race-%d", n))
		setter := s.(TitleSession)
		stale, _ := s.GetMetadata(context.Background())
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, source := range []TitleSource{TitleSourceAgent, TitleSourceUser} {
			wg.Add(1)
			go func(source TitleSource) {
				defer wg.Done()
				<-start
				_, err := setter.SetTitle(context.Background(), string(source), source)
				if err != nil && !(source == TitleSourceAgent && errors.Is(err, ErrTitleProtected)) {
					t.Errorf("set: %v", err)
				}
			}(source)
		}
		close(start)
		wg.Wait()
		if err := s.SetMetadata(context.Background(), stale); err != nil {
			t.Fatal(err)
		}
		md, _ := s.GetMetadata(context.Background())
		if md.Title != "user" || md.TitleSource != TitleSourceUser {
			t.Fatalf("race: %+v", md)
		}
	}
}

func TestSessionTitleRequiresLiveLease(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, time.Hour)
	s := acquireNamed(t, store, "leased").(*sqliteSession)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.SetTitle(ctx, "canceled", TitleSourceAgent); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := store.db.Exec("UPDATE session_leases SET owner_token = randomblob(16) WHERE session_id = ?", s.id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTitle(context.Background(), "lost", TitleSourceAgent); !errors.Is(err, ErrSessionLeaseLost) {
		t.Fatalf("lease: %v", err)
	}
}

func TestSessionTitlePreservesExplicitTTLArtifactsAndParent(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeMemory, &Metadata{TTL: 2 * time.Hour}, 24*time.Hour)
	parent := acquireNamed(t, store, "parent")
	child, err := store.Acquire(ctx, "child", AcquireOptions{Auto: true, Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	ref, err := child.ArtifactStore().Put(ctx, artifacts.Blob{Data: []byte("kept bytes")})
	if err != nil {
		t.Fatal(err)
	}
	s := child.(*sqliteSession)
	before, explicitBefore, err := scanSnapshot(ctx, store.db, s.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []TitleSource{TitleSourceAgent, TitleSourceUser} {
		if _, err := s.SetTitle(ctx, "Child objective", source); err != nil {
			t.Fatal(err)
		}
		after, explicitAfter, err := scanSnapshot(ctx, store.db, s.id)
		if err != nil {
			t.Fatal(err)
		}
		if explicitBefore != 1 || explicitAfter != 1 || after.ttlNS != before.ttlNS || after.updatedNS != before.updatedNS || after.retention != before.retention {
			t.Fatal("title changed explicit retention or last-used")
		}
	}
	if string(readArtifact(t, child.ArtifactStore(), ref.ID)) != "kept bytes" {
		t.Fatal("artifact lost")
	}
	if err := child.Report(ctx, Report{Text: "report", Status: ReportFinished}); err != nil {
		t.Fatal(err)
	}
	reports, err := parent.TakeReports(ctx)
	if err != nil || len(reports) != 1 || reports[0].Child != "child" {
		t.Fatalf("report: %+v, %v", reports, err)
	}
	md, _ := child.GetMetadata(ctx)
	if md.Name != "child" || md.Parent != "parent" {
		t.Fatalf("relationships changed: %+v", md)
	}
}
