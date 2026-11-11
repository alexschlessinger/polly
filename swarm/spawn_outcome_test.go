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

// A member's run ends by noting its outcome on the member's session. The
// store writes it behind the member, and it is in the database for another
// polly to read once the family's root has closed.
func TestMemberOutcomeReachesTheDatabase(t *testing.T) {
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
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.config.Parent.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	view, err := other.ReadView(ctx, sessions.ViewTarget{ID: member}, "")
	if err != nil {
		t.Fatal(err)
	}
	if view.Metadata == nil || view.Metadata.SpawnOutcome != sessions.ReportFinished {
		t.Fatalf("a finished member's stored settings = %+v", view.Metadata)
	}
}
