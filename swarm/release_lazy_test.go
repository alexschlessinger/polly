package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
)

func TestRecordReleaseMalformedOwnJournalIsRejectedWithoutPromotion(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"automatic", "targeted", "cleanup", "forget", "workflow"} {
		t.Run(mode, func(t *testing.T) {
			r := runtimeTest(t, idleModel(), 1, 1)
			payload := json.RawMessage(`{"intent":true}`)
			if err := r.parent.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
				raw.Records["parent_turn"] = map[string]json.RawMessage{r.ID: payload}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			r.config.Promote = func(context.Context) error { t.Fatal("release promoted malformed journal"); return nil }
			var err error
			switch mode {
			case "automatic":
				_, err = r.releaseWorkspaces(t.Context())
			case "targeted":
				_, err = r.releaseWorkspace(t.Context(), "missing")
			case "cleanup":
				err = r.Cleanup(t.Context(), "")
			case "forget":
				err = r.Forget(t.Context())
			case "workflow":
				_, err = (&workflowHost{runtime: r, controller: "missing"}).release(t.Context(), "missing")
			}
			var typed *json.UnmarshalTypeError
			if !errors.As(err, &typed) {
				t.Fatalf("%s ignored malformed own journal: %v", mode, err)
			}
			if records := rawRecords(t, r); recordRows(records) != 1 || string(records["parent_turn"][r.ID]) != string(payload) {
				t.Fatal("release mutated malformed journal or bootstrapped swarm format")
			}
		})
	}
}

func TestRecordReleaseEmptyRootStaysLazy(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"automatic", "targeted", "cleanup", "cleanup_target", "discard", "forget", "workflow"} {
		t.Run(mode, func(t *testing.T) {
			r := runtimeTest(t, idleModel(), 1, 1)
			promoted := errors.New("unexpected promotion")
			r.config.Promote = func(context.Context) error { return promoted }
			var err error
			switch mode {
			case "automatic":
				_, err = r.releaseWorkspaces(t.Context())
			case "targeted":
				_, err = r.releaseWorkspace(t.Context(), "missing")
			case "cleanup":
				err = r.Cleanup(t.Context(), "")
			case "cleanup_target":
				err = r.Cleanup(t.Context(), "missing")
			case "discard":
				err = r.Discard(t.Context(), "missing")
			case "forget":
				err = r.Forget(t.Context())
			case "workflow":
				_, err = (&workflowHost{runtime: r, controller: "missing"}).release(t.Context(), "missing")
			}
			if errors.Is(err, promoted) {
				t.Fatalf("empty %s promoted root: %v", mode, err)
			}
			if mode == "automatic" || mode == "cleanup" || mode == "forget" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("missing context accepted")
			}
			if rows := recordRows(rawRecords(t, r)); rows != 0 {
				t.Fatalf("empty %s wrote %d records", mode, rows)
			}
		})
	}
}
