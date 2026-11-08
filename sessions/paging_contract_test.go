package sessions_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/internal/sessiontest"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func pagingMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func pagingAcquire(t *testing.T, store sessions.SessionStore, name, parent string) sessions.Session {
	t.Helper()
	h, err := store.Acquire(t.Context(), name, sessions.AcquireOptions{Parent: parent})
	pagingMust(t, err)
	t.Cleanup(func() { h.Close() })
	return h
}

func TestPagingContract(t *testing.T) {
	for _, factory := range sessiontest.Factories() {
		t.Run(factory.Name, func(t *testing.T) {
			t.Parallel()
			t.Run("summary_identity_parent_and_ownership", func(t *testing.T) {
				t.Parallel()
				f := factory.New(t)
				store := f.Open()
				parent := pagingAcquire(t, store, "parent", "")
				child := pagingAcquire(t, store, "child", "parent")
				other := pagingAcquire(t, store, "other", "")
				parentID := parent.(sessions.ViewIdentity).ViewID()
				childID := child.(sessions.ViewIdentity).ViewID()
				pagingMust(t, child.AddMessages(t.Context(), messages.User("saved")))
				meta, err := child.GetMetadata(t.Context())
				pagingMust(t, err)
				meta.ActiveSkills = []string{"owned"}
				pagingMust(t, child.SetMetadata(t.Context(), meta))
				before, err := child.GetMetadata(t.Context())
				pagingMust(t, err)
				access, err := sessions.OpenSummaryAccess(struct{ sessions.SessionStore }{store})
				pagingMust(t, err)
				if access.Capabilities().NativeIndexed {
					t.Fatal("legacy fixture advertised native indexing")
				}
				page, err := access.ListSummaryPage(t.Context(), sessions.SummaryQuery{ParentID: &parentID, Limit: 1})
				pagingMust(t, err)
				if len(page.Summaries) != 1 || page.Next != "" || page.Summaries[0].ID != childID || page.Summaries[0].MessageCount != 1 || !page.Summaries[0].InUse {
					t.Fatalf("child page: %+v", page)
				}
				page.Summaries[0].Metadata.ActiveSkills[0] = "mutated"
				got, err := access.GetSummary(t.Context(), childID)
				pagingMust(t, err)
				if got.Metadata.ActiveSkills[0] != "owned" || !got.Metadata.LastUsed.Equal(before.LastUsed) {
					t.Fatal("paging aliases metadata or touches last-used")
				}
				roots := ""
				page, err = access.ListSummaryPage(t.Context(), sessions.SummaryQuery{ParentID: &roots, Limit: 10})
				pagingMust(t, err)
				if len(page.Summaries) != 2 {
					t.Fatalf("roots page: %+v", page)
				}
				pagingMust(t, parent.Rename(t.Context(), "renamed-parent"))
				pagingMust(t, child.Rename(t.Context(), "renamed-child"))
				got, err = access.GetSummary(t.Context(), childID)
				pagingMust(t, err)
				if got.Metadata.Name != "renamed-child" || got.ParentID != parentID || got.Metadata.Parent != "renamed-parent" {
					t.Fatal("lookup used name instead of stable identity")
				}
				pagingMust(t, child.Close())
				pagingMust(t, store.Delete(t.Context(), "renamed-child"))
				replacement := pagingAcquire(t, store, "renamed-child", "")
				if replacement.(sessions.ViewIdentity).ViewID() == childID {
					t.Fatal("reused stable ID")
				}
				if _, err := access.GetSummary(t.Context(), childID); !errors.Is(err, sessions.ErrSessionNotFound) {
					t.Fatalf("deleted identity rebound: %v", err)
				}
				pagingMust(t, other.Close())
				pagingMust(t, store.Close())
				if _, err := access.ListSummaryPage(t.Context(), sessions.SummaryQuery{Limit: 1}); !errors.Is(err, sessions.ErrStoreClosed) {
					t.Fatalf("closed store: %v", err)
				}
			})
			t.Run("transcript_contiguous_oversize_and_epochs", func(t *testing.T) {
				t.Parallel()
				f := factory.New(t)
				store := f.Open()
				h := pagingAcquire(t, store, "conversation", "")
				c := h.(sessions.CoordinationSession)
				target := sessions.ViewTarget{ID: c.ViewID()}
				large := strings.Repeat("image data", 1000)
				want := []messages.ChatMessage{
					{Role: messages.MessageRoleUser, Content: "first", Metadata: map[string]any{"nested": map[string]any{"value": "owned"}}},
					{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "image_base64", ImageData: large, Artifact: &artifacts.Ref{ID: "reference"}}}},
					{Role: messages.MessageRoleAssistant, Content: "last"},
				}
				pagingMust(t, c.UpdateCoordination(t.Context(), func(state *sessions.CoordinationState) error {
					state.Append = want
					state.Transcript = &sessions.TranscriptIdentity{Execution: "execution", Generation: 2}
					return nil
				}))
				access, err := sessions.OpenTranscriptAccess(struct {
					sessions.SessionStore
					sessions.ViewStore
				}{store, store.(sessions.ViewStore)})
				pagingMust(t, err)
				if access.Capabilities() != (sessions.TranscriptCapabilities{}) {
					t.Fatal("legacy fixture advertised native paging")
				}
				first, err := access.ReadTranscriptPage(t.Context(), target, sessions.TranscriptQuery{Limit: 2, MaxBytes: 256})
				pagingMust(t, err)
				if first.ID != target.ID || first.Epoch == "" || first.First != 0 || first.Next != 1 || first.Through != 3 || len(first.Messages) != 1 || first.Oversized || first.Transcript == nil || first.Transcript.Sequence != 3 {
					t.Fatalf("first page: %+v", first)
				}
				query := sessions.TranscriptQuery{Epoch: first.Epoch, After: first.Next, Through: first.Through, Limit: 2, MaxBytes: 256}
				second, err := access.ReadTranscriptPage(t.Context(), target, query)
				pagingMust(t, err)
				if !second.Oversized || second.First != 1 || second.Next != 2 || len(second.Messages) != 1 || second.Messages[0].Parts[0].ImageData != large {
					t.Fatalf("oversized message was dropped/truncated: %+v", second)
				}
				query.After = second.Next
				last, err := access.ReadTranscriptPage(t.Context(), target, query)
				pagingMust(t, err)
				if last.Next != last.Through || len(last.Messages) != 1 || last.Messages[0].Content != "last" {
					t.Fatalf("last page: %+v", last)
				}
				first.Messages[0].Metadata["nested"].(map[string]any)["value"] = "mutated"
				first.Transcript.Execution = "mutated"
				second.Messages[0].Parts[0].Artifact.ID = "mutated"
				again, err := access.ReadTranscriptPage(t.Context(), target, sessions.TranscriptQuery{Limit: 3})
				pagingMust(t, err)
				if again.Messages[0].Metadata["nested"].(map[string]any)["value"] != "owned" || again.Messages[1].Parts[0].Artifact.ID != "reference" || again.Transcript.Execution != "execution" {
					t.Fatal("returned page aliases storage")
				}
				pagingMust(t, h.AddMessages(t.Context(), messages.User("append")))
				if _, err := access.ReadTranscriptPage(t.Context(), target, query); !errors.Is(err, sessions.ErrStaleTranscriptEpoch) {
					t.Fatalf("legacy append failed to invalidate: %v", err)
				}
				for _, reset := range []bool{false, true} {
					before, err := access.ReadTranscriptPage(t.Context(), target, sessions.TranscriptQuery{Limit: 1})
					pagingMust(t, err)
					if reset {
						metadata, err := h.GetMetadata(t.Context())
						pagingMust(t, err)
						pagingMust(t, h.Reset(t.Context(), metadata))
					} else {
						pagingMust(t, h.Clear(t.Context()))
					}
					// Recreate the same length/content: the view revision, not a
					// content hash or message count, must fence replacement.
					for range before.Through {
						pagingMust(t, h.AddMessages(t.Context(), messages.User("replacement")))
					}
					if _, err := access.ReadTranscriptPage(t.Context(), target, sessions.TranscriptQuery{Epoch: before.Epoch, Limit: 1}); !errors.Is(err, sessions.ErrStaleTranscriptEpoch) {
						t.Fatalf("replacement retained epoch: %v", err)
					}
				}
				pagingMust(t, h.Close())
				pagingMust(t, store.Close())
				if _, err := access.ReadTranscriptPage(t.Context(), target, sessions.TranscriptQuery{Limit: 1}); !errors.Is(err, sessions.ErrStoreClosed) {
					t.Fatalf("closed view store: %v", err)
				}
				store = f.Open()
				access, err = sessions.OpenTranscriptAccess(struct {
					sessions.SessionStore
					sessions.ViewStore
				}{store, store.(sessions.ViewStore)})
				pagingMust(t, err)
				reopened, err := access.ReadTranscriptPage(t.Context(), target, sessions.TranscriptQuery{Limit: 1})
				pagingMust(t, err)
				if reopened.ID != target.ID || reopened.Transcript != nil || reopened.Through != 4 {
					t.Fatalf("reopened transcript: %+v", reopened)
				}
				pagingMust(t, store.Delete(t.Context(), "conversation"))
				pagingAcquire(t, store, "conversation", "")
				if _, err := access.ReadTranscriptPage(t.Context(), target, sessions.TranscriptQuery{Limit: 1}); !errors.Is(err, sessions.ErrSessionNotFound) {
					t.Fatalf("deleted transcript rebound: %v", err)
				}
			})
		})
	}
}
