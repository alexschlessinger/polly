package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestContextProfileConcurrentUpdates(t *testing.T) {
	home, state := sandboxTryState(t)
	profile := state.sandboxProfile
	mkdirs(t, filepath.Join(home, "reference"), filepath.Join(home, "build"), filepath.Join(home, "output"))
	variants := [][]sandboxProfileItem{
		nil,
		{{Kind: profileRead, Path: "~/reference"}},
		{{Kind: profileWrite, Path: "~/build"}, {Kind: profileWrite, Path: "~/output"}},
	}
	wantReads, wantWrites := []string{"~/reference"}, []string{"~/build", "~/output"}
	readGrants := func() error {
		reads, writes := profile.grantedPaths()
		if len(reads) == 0 && len(writes) == 0 ||
			slices.Equal(reads, wantReads) && len(writes) == 0 ||
			len(reads) == 0 && slices.Equal(writes, wantWrites) {
			return nil
		}
		return fmt.Errorf("incoherent profile grants: reads=%v writes=%v", reads, writes)
	}
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "continue"}}
	readSnapshot := func() error {
		// Empty SystemPrompt and no schema keep the contract composition path
		// that reads the profile's current grants on a background UI worker.
		snapshot, err := collectContextSnapshot(context.Background(), &Config{}, state, history)
		if err != nil {
			return err
		}
		if len(snapshot.roles) != 4 || snapshot.roles[3].role != messages.MessageRoleSystem || snapshot.roles[3].count != 1 || snapshot.roles[3].tokens == 0 {
			return fmt.Errorf("snapshot lacks composed system guidance: %+v", snapshot.roles)
		}
		return nil
	}

	stop := make(chan struct{})
	ready := make(chan struct{}, 2)
	errors := make(chan error, 2)
	var readers sync.WaitGroup
	for _, read := range []func() error{readGrants, readSnapshot} {
		readers.Go(func() {
			// Complete one read before publication starts, then keep reading
			// until every update has finished. This also exercises empty grants.
			err := read()
			ready <- struct{}{}
			if err != nil {
				errors <- err
				return
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := read(); err != nil {
					errors <- err
					return
				}
			}
		})
	}
	defer func() {
		close(stop)
		readers.Wait()
		close(errors)
		for err := range errors {
			t.Error(err)
		}
	}()
	<-ready
	<-ready
	for i := 0; i < 200; i++ {
		items := variants[i%len(variants)]
		// A nil file edit keeps the test session-only while going through the
		// same updateProfileContext publication used by sandbox_prepare.
		if err := profile.update(state.toolRegistry, nil, func([]sandboxProfileItem) []sandboxProfileItem {
			return slices.Clone(items)
		}); err != nil {
			t.Fatal(err)
		}
		if err := readGrants(); err != nil {
			t.Fatal(err)
		}
		reads, writes := profile.grantedPaths()
		switch i % len(variants) {
		case 0:
			if len(reads) != 0 || len(writes) != 0 {
				t.Fatalf("empty profile grants reads=%v writes=%v", reads, writes)
			}
		case 1:
			if !slices.Equal(reads, wantReads) || len(writes) != 0 {
				t.Fatalf("read profile grants reads=%v writes=%v", reads, writes)
			}
		case 2:
			if len(reads) != 0 || !slices.Equal(writes, wantWrites) {
				t.Fatalf("write profile grants reads=%v writes=%v", reads, writes)
			}
		}
	}
	if saved, err := readSandboxProfile(profile.ws.profile); err != nil || len(saved.Items) != 0 {
		t.Fatalf("session updates changed the profile file: %+v, %v", saved, err)
	}
	if got := testSessionHistory(t, state.session); len(got) != 0 {
		t.Fatalf("context inspection persisted history: %+v", got)
	}
}

func TestContextProfileNilGrants(t *testing.T) {
	var profile *sandboxProfileState
	if reads, writes := profile.grantedPaths(); len(reads) != 0 || len(writes) != 0 {
		t.Fatalf("nil profile grants reads=%v writes=%v", reads, writes)
	}
}
