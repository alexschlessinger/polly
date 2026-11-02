package swarm

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/worktree"
)

func publicationCorrectionState() *State {
	s := &State{
		Members:      map[string]*Member{"reader": {ID: "reader", Execution: "reading"}},
		Executions:   map[string]*Execution{"reading": {Run: "run"}},
		Parents:      map[string]*ParentRecord{"root": {}},
		Publications: map[string]*Publication{},
	}
	for i, author := range []string{"a", "b", "c"} {
		p := &Publication{ID: author + "0", Author: author, Run: "run", Scope: PublicationScopeWorkspace,
			Key: "files/config", Value: "absent", Text: author + " observed absence", Sources: []string{author + "/probe"}, Posted: time.Unix(int64(i+1), 0)}
		if i > 0 {
			p.DuplicateOf = "a0"
		}
		s.Publications[p.ID] = p
	}
	return s
}

func addPublicationCorrection(s *State, id, previous, value string, second int64) *Publication {
	p := *s.Publications[previous]
	p.ID, p.Supersedes, p.Value = id, previous, value
	p.Text, p.Posted = "corrected to "+value, time.Unix(second, 0)
	p.DuplicateOf, p.AdmittedTo = "", nil
	s.Publications[id] = &p
	return &p
}

func checkPublicationIDs(t *testing.T, got []*Publication, want ...string) {
	t.Helper()
	var ids []string
	for _, p := range got {
		ids = append(ids, p.ID)
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("publication IDs = %v, want %v", ids, want)
	}
}

func TestPublicationCorrectionsRetainIndependentEvidence(t *testing.T) {
	t.Parallel()
	for _, author := range []string{"a", "b"} {
		t.Run(author, func(t *testing.T) {
			s := publicationCorrectionState()
			before := *s.Publications["c0"]
			checkPublicationIDs(t, currentPublications(s), "a0")
			addPublicationCorrection(s, author+"1", author+"0", "present", 4)
			representative := "a0"
			if author == "a" {
				representative = "b0"
			}
			checkPublicationIDs(t, currentPublications(s), representative, author+"1")
			checkPublicationIDs(t, visiblePublications(s, "run", false), representative, author+"1")
			if got := publicationConfirmations(s, representative); !slices.Equal(got, []string{"c0"}) {
				t.Fatalf("current support = %v, want c0", got)
			}
			if got := publicationConfirmations(s, author+"0"); len(got) != 0 {
				t.Fatalf("withdrawn evidence still claims current support: %v", got)
			}
			if !reflect.DeepEqual(*s.Publications["c0"], before) || s.Publications["b0"].DuplicateOf != "a0" {
				t.Fatal("selection rewrote historical evidence")
			}
			// Each remaining author must correct their own evidence. Successive
			// representatives keep their original coalescing links and timestamps.
			addPublicationCorrection(s, "remaining1", representative, "present", 5)
			checkPublicationIDs(t, currentPublications(s), "c0", author+"1")
			addPublicationCorrection(s, "c1", "c0", "present", 6)
			checkPublicationIDs(t, currentPublications(s), author+"1")
			if got := publicationConfirmations(s, author+"1"); !slices.Equal(got, []string{"c1", "remaining1"}) {
				t.Fatalf("matching corrections lost current support: %v", got)
			}
		})
	}
}

func TestPublicationPromotedConfirmationUsesExactReceipts(t *testing.T) {
	t.Parallel()
	for _, reader := range []string{"reader", "root"} {
		for _, second := range []int64{0, 5, 7} {
			t.Run(reader+time.Unix(second, 0).Format("05"), func(t *testing.T) {
				s := publicationCorrectionState()
				mark := PublicationMark{}
				if second > 0 {
					mark = PublicationMark{Posted: time.Unix(second, 0), ID: "later"}
				}
				if reader == "root" {
					s.Parents[reader].Publications = mark
				} else {
					s.Members[reader].Publications = mark
				}
				correction := addPublicationCorrection(s, "a1", "a0", "present", 6)
				want := []string{"b0", "a1"}
				if second == 7 {
					want = want[:1]
				}
				checkPublicationIDs(t, pendingPublications(s, reader, "run"), want...)
				b := s.Publications["b0"]
				if got := publicationReaders(s, b); got == nil || len(got) != 0 {
					t.Fatalf("promotion invented receipts: %v", got)
				}
				if err := advancePublicationMark(s, reader, b); err != nil {
					t.Fatal(err)
				}
				if second > 0 && publicationMark(s, reader) != mark {
					t.Fatal("older confirmation rewound the cursor")
				}
				if err := advancePublicationMark(s, reader, b); err == nil {
					t.Fatal("duplicate admission accepted twice")
				}
				if got := publicationReaders(s, b); !slices.Equal(got, []string{reader}) {
					t.Fatalf("actual duplicate receipt missing: %v", got)
				}
				if got := publicationReaders(s, s.Publications["c0"]); len(got) != 0 {
					t.Fatalf("undelivered sibling inherited receipts: %v", got)
				}
				if second < 7 {
					if err := advancePublicationMark(s, reader, correction); err != nil {
						t.Fatal(err)
					}
				}
				checkPublicationIDs(t, pendingPublications(s, reader, "run"))
				// Receipts are historical, not inferred again from the reader's
				// current run. Returning to this run must not redeliver evidence.
				s.Executions["reading"].Run = "later-run"
				if got := publicationReaders(s, b); !slices.Equal(got, []string{reader}) {
					t.Fatalf("run change erased an actual receipt: %v", got)
				}
			})
		}
	}
}

func TestPublicationSelfCorrectionStillDeliversOtherAuthors(t *testing.T) {
	t.Parallel()
	s := publicationCorrectionState()
	s.Members["a"] = &Member{ID: "a", Execution: "reading", Publications: PublicationMark{Posted: time.Unix(3, 0), ID: "c0"}}
	addPublicationCorrection(s, "a1", "a0", "present", 4)
	addPublicationCorrection(s, "a2", "a1", "unknown", 5)
	checkPublicationIDs(t, pendingPublications(s, "reader", "run"), "b0", "a2")
	checkPublicationIDs(t, pendingPublications(s, "a", "run"), "b0")
	if err := advancePublicationMark(s, "a", s.Publications["b0"]); err != nil {
		t.Fatal(err)
	}
	checkPublicationIDs(t, pendingPublications(s, "a", "run"))
	if got := publicationReaders(s, s.Publications["b0"]); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("correction author lost the other author's evidence receipt: %v", got)
	}
	if slices.Contains(publicationReaders(s, s.Publications["a2"]), "a") {
		t.Fatal("correction acquired a self-receipt")
	}
}

func TestPublicationCorrectionEventsSurviveCoalescing(t *testing.T) {
	t.Parallel()
	s := publicationCorrectionState()
	addPublicationCorrection(s, "c1", "c0", "present", 4)
	s.Members["reader"].Publications = PublicationMark{Posted: time.Unix(4, 0), ID: "c1"}
	addPublicationCorrection(s, "a1", "a0", "present", 5)
	// c1 represents the present claim, but a1 is still a new correction.
	checkPublicationIDs(t, currentPublications(s), "b0", "c1")
	pending := pendingPublications(s, "reader", "run")
	checkPublicationIDs(t, pending, "b0", "a1")
	for _, p := range pending {
		if err := advancePublicationMark(s, "reader", p); err != nil {
			t.Fatal(err)
		}
	}
	addPublicationCorrection(s, "b1", "b0", "present", 6)
	checkPublicationIDs(t, currentPublications(s), "c1")
	checkPublicationIDs(t, pendingPublications(s, "reader", "run"), "b1")
}

func TestPublicationPromotionPreservesLegacyDeliveryEvents(t *testing.T) {
	t.Parallel()
	s := publicationCorrectionState()
	addPublicationCorrection(s, "a1", "a0", "present", 4)
	// Before survivor promotion existed, a new publication of the surviving
	// value could become another canonical record. Keep that event eligible.
	d := *s.Publications["b0"]
	d.ID, d.Author, d.DuplicateOf, d.Posted = "d0", "d", "", time.Unix(5, 0)
	s.Publications[d.ID] = &d
	checkPublicationIDs(t, currentPublications(s), "b0", "a1")
	checkPublicationIDs(t, pendingPublications(s, "reader", "run"), "b0", "a1", "d0")
}

func TestPublicationPromotedClaimKeepsItsAudience(t *testing.T) {
	t.Parallel()
	s := publicationCorrectionState()
	mark := PublicationMark{Posted: time.Unix(5, 0), ID: "unrelated"}
	s.Members["reader"].Publications, s.Parents["root"].Publications = mark, mark
	p := addPublicationCorrection(s, "a1", "a0", "present", 6)
	p.Run, p.Scope = "later-run", PublicationScopeRun
	checkPublicationIDs(t, pendingPublications(s, "reader", "run"), "b0")
	checkPublicationIDs(t, pendingPublications(s, "root", ""), "b0")
	checkPublicationIDs(t, pendingPublications(s, "reader", "later-run"), "a1")
	checkPublicationIDs(t, visiblePublications(s, "run", false), "b0")
	checkPublicationIDs(t, visiblePublications(s, "later-run", false), "a1")
}

func TestPublicationCurrentClaimsRespectCoalescingBoundaries(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"run", "scope", "commit", "unkeyed", "different text", "same text", "same instant"} {
		t.Run(kind, func(t *testing.T) {
			s := publicationCorrectionState()
			delete(s.Publications, "c0")
			a, b := s.Publications["a0"], s.Publications["b0"]
			switch kind {
			case "run":
				b.Run = "another-run"
			case "scope":
				b.Scope = PublicationScopeRun
			case "commit":
				a.Scope, b.Scope = PublicationScopeSnapshot, PublicationScopeSnapshot
				a.Snapshot, b.Snapshot = "one", "two"
				s.Snapshots = map[string]*worktree.Snapshot{
					"one": {ID: "one", Commit: "first", Tree: "tree", Source: "/repo"},
					"two": {ID: "two", Commit: "second", Tree: "tree", Source: "/repo"},
				}
			case "unkeyed":
				a.Key, b.Key = "", ""
			case "different text":
				a.Value, b.Value = "", ""
			case "same text":
				a.Value, b.Value = "", ""
				b.Text = "  " + a.Text + "  "
			case "same instant":
				b.Posted = a.Posted
			}
			if kind == "same text" || kind == "same instant" {
				checkPublicationIDs(t, currentPublications(s), "a0")
			} else {
				checkPublicationIDs(t, currentPublications(s), "a0", "b0")
			}
		})
	}
}

func TestPublicationPromotionReceiptsCommitOnlyStagedEvidence(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, nilModel(), 1, 1)
	ctx := context.Background()
	if err := r.update(ctx, func(s *State) error {
		for _, id := range []string{"a", "b", "c"} {
			s.Members[id] = &Member{ID: id}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	publish := func(author string, p Publication) *Publication {
		t.Helper()
		got, err := r.Publish(ctx, author, p)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	var originals []*Publication
	for _, author := range []string{"a", "b", "c"} {
		p := publish(author, Publication{Key: "files/config", Value: "absent", Scope: PublicationScopeWorkspace, Text: author + " found no file", AdmittedTo: []string{r.ID}})
		if len(p.AdmittedTo) != 0 {
			t.Fatal("publisher forged delivery receipts")
		}
		originals = append(originals, p)
	}
	admitParent(t, r)
	// Move the cursor beyond every confirmation before correcting the original.
	publish("a", Publication{Scope: PublicationScopeWorkspace, Text: "unrelated observation"})
	admitParent(t, r)
	a1 := publish("a", Publication{Supersedes: originals[0].ID, Value: "present", Text: "a corrected its claim"})
	cb := &llm.AgentCallbacks{}
	r.bindParent(cb, nil)
	input, err := cb.AdmitInput(ctx)
	if err != nil || len(input) != 1 {
		t.Fatalf("staged publications: %+v %v", input, err)
	}
	ids, _ := input[0].Metadata[messages.MetadataKeySwarmPublications].([]string)
	if !slices.Equal(ids, []string{originals[1].ID, a1.ID}) {
		t.Fatalf("staged IDs = %v", ids)
	}
	if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{}); err != nil {
		t.Fatal(err)
	}
	bad := append(slices.Clone(input), messages.ChatMessage{Role: messages.MessageRoleAssistant, Metadata: map[string]any{"invalid": make(chan struct{})}})
	if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: bad}); err == nil {
		t.Fatal("invalid transcript append committed")
	}
	state, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(publicationReaders(state, state.Publications[originals[1].ID])) != 0 {
		t.Fatal("staging, omitted input or failed checkpoint claimed an uncommitted receipt")
	}
	// The staged confirmation is corrected before checkpointing. Receipt
	// persistence must use the accepted input, not recompute its representative.
	b1 := publish("b", Publication{Supersedes: originals[1].ID, Value: "present", Text: "b corrected its claim"})
	if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: input}); err != nil {
		t.Fatal(err)
	}
	state, err = r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := publicationReaders(state, state.Publications[originals[1].ID]); !slices.Equal(got, []string{r.ID}) {
		t.Fatalf("persisted receipt = %v", got)
	}
	if len(publicationReaders(state, state.Publications[originals[2].ID])) != 0 || state.Parents[r.ID].Publications.ID != a1.ID {
		t.Fatal("checkpoint admitted unstaged evidence or advanced past staged input")
	}
	checkPublicationIDs(t, pendingPublications(state, r.ID, ""), originals[2].ID, b1.ID)
	admitParent(t, r)
	state, err = r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkPublicationIDs(t, pendingPublications(state, r.ID, ""))
	if got := publicationReaders(state, state.Publications[originals[2].ID]); !slices.Equal(got, []string{r.ID}) {
		t.Fatalf("next promotion lost its receipt: %v", got)
	}
	if got := state.Publications[originals[2].ID]; got.DuplicateOf != originals[0].ID || !got.Posted.Equal(originals[2].Posted) || got.Author != originals[2].Author || got.Text != originals[2].Text {
		t.Fatal("promotion rewrote historical evidence")
	}
}
