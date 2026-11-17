package swarm

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

// inUse reports whether store sees the session id leased.
func inUse(t *testing.T, ctx context.Context, store sessions.SessionStore, id string) bool {
	t.Helper()
	view, err := store.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: id}, "")
	if err != nil {
		t.Fatal(err)
	}
	return view.InUse
}

// A member's slice ends by closing its session. The store that drives the
// family keeps the lease for the member's next slice, so another polly sees
// the member in use while this one sees it free, until the family's root
// closes.
func TestStoreKeepsTheLeaseOfAMemberBetweenSlices(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "polly.db")
	model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") })
	r := runtimeTestOn(t, sessions.StoreConfig{Mode: sessions.ModeDisk, Path: path}, model, 1, 1, nil)
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "answer", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	<-result.Done
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	member := s.Members[result.Session].ID
	other, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if here, elsewhere := inUse(t, ctx, r.config.Store, member), inUse(t, ctx, other, member); here || !elsewhere {
		t.Fatalf("a finished member is in use here = %v, elsewhere = %v", here, elsewhere)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.config.Parent.Close(); err != nil {
		t.Fatal(err)
	}
	if inUse(t, ctx, other, member) {
		t.Fatal("the member is in use after its family's root closed")
	}
}
