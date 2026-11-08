package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// Supply functional capabilities over the legacy backend, not a mock that only
// reports flags. Production must select these methods without unwrapping it.
type countedHostSummaries struct {
	sessions.SessionStore
	pages      sessions.SummaryPageStore
	queries    []sessions.SummaryQuery
	ids        []string
	views      []sessions.ViewTarget
	listed     int
	beforePage func(context.Context) error
}

func (s *countedHostSummaries) ListSummaries(context.Context) ([]sessions.SessionSummary, error) {
	s.listed++
	return nil, errors.New("unbounded summary read")
}
func (s *countedHostSummaries) ReadView(ctx context.Context, target sessions.ViewTarget, known string) (*sessions.SessionView, error) {
	s.views = append(s.views, target)
	return s.SessionStore.(sessions.ViewStore).ReadView(ctx, target, known)
}
func (s *countedHostSummaries) GetSummary(ctx context.Context, id string) (*sessions.SessionSummary, error) {
	s.ids = append(s.ids, id)
	return s.pages.GetSummary(ctx, id)
}
func (s *countedHostSummaries) ListSummaryPage(ctx context.Context, q sessions.SummaryQuery) (sessions.SummaryPage, error) {
	s.queries = append(s.queries, q)
	if s.beforePage != nil {
		if err := s.beforePage(ctx); err != nil {
			return sessions.SummaryPage{}, err
		}
	}
	return s.pages.ListSummaryPage(ctx, q)
}

func countedSummaries(t *testing.T, store sessions.SessionStore) *countedHostSummaries {
	t.Helper()
	// Hiding optional methods forces the independently functional fallback.
	access, err := sessions.OpenSummaryAccess(struct{ sessions.SessionStore }{store})
	if err != nil {
		t.Fatal(err)
	}
	return &countedHostSummaries{SessionStore: store, pages: access}
}

func TestHostSummarySelectionPagingAndOwnership(t *testing.T) {
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	root := testAcquireSession(t, store, "root")
	rootID := root.(sessions.ViewIdentity).ViewID()
	for n := range hostSummaryPageSize + 3 {
		child, err := store.Acquire(ctx, fmt.Sprintf("child-%03d", n), sessions.AcquireOptions{Parent: "root"})
		if err != nil {
			t.Fatal(err)
		}
		md, err := child.GetMetadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		md.SpawnCallID = fmt.Sprintf("call-%03d", n)
		if err := child.SetMetadata(ctx, md); err != nil {
			t.Fatal(err)
		}
		if err := child.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	other := testAcquireSession(t, store, "unrelated")
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	counted := countedSummaries(t, store)
	rows, err := sessionSummaries(ctx, counted, &rootID)
	if err != nil || len(rows) != hostSummaryPageSize+3 || len(counted.queries) != 2 || counted.listed != 0 {
		t.Fatalf("rows=%d queries=%+v listed=%d err=%v", len(rows), counted.queries, counted.listed, err)
	}
	for _, q := range counted.queries {
		if q.ParentID == nil || *q.ParentID != rootID || q.Limit != hostSummaryPageSize {
			t.Fatalf("selector=%+v", q)
		}
	}
	if counted.queries[0].After != "" || counted.queries[1].After == "" {
		t.Fatal("continuation not consumed")
	}
	got, err := sessionSummary(ctx, counted, rootID)
	if err != nil || got.ID != rootID || !reflect.DeepEqual(counted.ids, []string{rootID}) {
		t.Fatalf("lookup=%+v ids=%v err=%v", got, counted.ids, err)
	}
	got.Metadata.Name = "mutated owned result"
	fresh, err := sessionSummary(ctx, counted, rootID)
	if err != nil || fresh.Metadata.Name != "root" {
		t.Fatalf("ownership lost: %+v %v", fresh, err)
	}
	match, err := launchedAgentSummary(ctx, counted, rootID, "call-130")
	if err != nil || match.Metadata.Name != "child-130" {
		t.Fatalf("child lookup=%+v %v", match, err)
	}
	legacyCalls := 0
	legacyStore := pickerListingStore{SessionStore: store, list: func(ctx context.Context) ([]sessions.SessionSummary, error) {
		legacyCalls++
		return store.ListSummaries(ctx)
	}}
	legacy, err := sessionSummaries(ctx, legacyStore, &rootID)
	if err != nil || !reflect.DeepEqual(rows, legacy) || legacyCalls != 1 {
		t.Fatalf("legacy parity: calls=%d err=%v", legacyCalls, err)
	}
	// Accessors never own or close their host backend.
	if _, err := store.ListSummaries(ctx); err != nil {
		t.Fatalf("borrowed store closed: %v", err)
	}
}

func TestHostSummaryCancellationAndAmbiguousChild(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	root := testAcquireSession(t, store, "root")
	id := root.(sessions.ViewIdentity).ViewID()
	for _, name := range []string{"first", "second"} {
		child, err := store.Acquire(context.Background(), name, sessions.AcquireOptions{Parent: "root"})
		if err != nil {
			t.Fatal(err)
		}
		md, _ := child.GetMetadata(context.Background())
		md.SpawnCallID = "same"
		if err := child.SetMetadata(context.Background(), md); err != nil {
			t.Fatal(err)
		}
		if err := child.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	counted := countedSummaries(t, store)
	if _, err := launchedAgentSummary(context.Background(), counted, id, "same"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous lookup=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	counted.beforePage = func(context.Context) error { cancel(); return ctx.Err() }
	if _, err := sessionSummaries(ctx, counted, &id); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}

func TestHostTitleAndPickerUseSuppliedSummaries(t *testing.T) {
	base := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, base, "root")
	counted := countedSummaries(t, base)
	r.state.sessionStore = counted
	id := r.visibleTab().viewID()
	r.refreshSessionTitle(id, nil)
	if !reflect.DeepEqual(counted.ids, []string{id}) || counted.listed != 0 || len(counted.queries) != 0 {
		t.Fatalf("title did not select exact identity: ids=%v queries=%v listed=%d", counted.ids, counted.queries, counted.listed)
	}
	r.openSessionsPicker()
	waitSessionsPicker(t, r)
	if r.sessionsPicker == nil || len(r.sessionsPicker.infos) != 1 || len(counted.queries) != 1 || counted.listed != 0 {
		t.Fatalf("picker did not consume supplied pages: picker=%+v queries=%v listed=%d", r.sessionsPicker, counted.queries, counted.listed)
	}
	r.openSessionTitleInput(r.sessionsPicker.infos[id])
	if !reflect.DeepEqual(counted.ids, []string{id, id}) || counted.listed != 0 {
		t.Fatalf("title editor did not recheck exact identity: ids=%v listed=%d", counted.ids, counted.listed)
	}
}

type countedAppendSession struct {
	sessions.Session
	records                                              sessions.CoordinationAccess
	reads, updates, identities, effects, payloads, plain int
	afterRead                                            func()
	writeErr                                             error
}

func (s *countedAppendSession) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	s.reads++
	err := s.records.ReadRecords(ctx, func(r sessions.CoordinationReader) error {
		return fn(&countedAppendReader{CoordinationReader: r, owner: s})
	})
	if err == nil && s.afterRead != nil {
		s.afterRead()
	}
	return err
}
func (s *countedAppendSession) UpdateRecords(ctx context.Context, fn func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	s.updates++
	commit, err := s.records.UpdateRecords(ctx, func(tx sessions.CoordinationTx) error { return fn(&countedAppendTx{CoordinationTx: tx, owner: s}) })
	if err == nil {
		err = s.writeErr
	}
	return commit, err
}
func (s *countedAppendSession) AddMessages(ctx context.Context, ms []messages.ChatMessage) error {
	s.plain++
	return s.Session.AddMessages(ctx, ms)
}

type countedAppendReader struct {
	sessions.CoordinationReader
	owner *countedAppendSession
}

func (r *countedAppendReader) Identity() (sessions.CoordinationIdentity, error) {
	r.owner.identities++
	return r.CoordinationReader.Identity()
}
func (r *countedAppendReader) Get(sessions.RecordKey) (sessions.CoordinationRecord, bool, error) {
	r.owner.payloads++
	return sessions.CoordinationRecord{}, false, errors.New("unexpected payload read")
}
func (r *countedAppendReader) Query(sessions.RecordQuery) (sessions.RecordPage, error) {
	r.owner.payloads++
	return sessions.RecordPage{}, errors.New("unexpected payload query")
}

type countedAppendTx struct {
	sessions.CoordinationTx
	owner *countedAppendSession
}

func (tx *countedAppendTx) Get(sessions.RecordKey) (sessions.CoordinationRecord, bool, error) {
	tx.owner.payloads++
	return sessions.CoordinationRecord{}, false, errors.New("unexpected transaction payload read")
}
func (tx *countedAppendTx) Query(sessions.RecordQuery) (sessions.RecordPage, error) {
	tx.owner.payloads++
	return sessions.RecordPage{}, errors.New("unexpected transaction payload query")
}
func (tx *countedAppendTx) Effects(e sessions.CoordinationEffects) error {
	tx.owner.effects++
	return tx.CoordinationTx.Effects(e)
}

type appendGateUI struct {
	TurnUI
	allowed func() bool
}

func (ui appendGateUI) TurnPersistenceAllowed() bool { return ui.allowed() }

func TestHostAppendRecordSelectionAndFences(t *testing.T) {
	for _, mode := range []string{"append", "detached during read", "newer append", "uncertain commit", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			session := testAcquireSession(t, store, "root")
			defer session.Close()
			access, err := sessions.OpenCoordinationAccess(session)
			if err != nil {
				t.Fatal(err)
			}
			counted := &countedAppendSession{Session: session, records: access}
			allowed := true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			uncertain := errors.New("uncertain committed write")
			switch mode {
			case "detached during read":
				counted.afterRead = func() { allowed = false }
			case "newer append":
				counted.afterRead = func() {
					if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "newer"}); err != nil {
						t.Fatal(err)
					}
				}
			case "uncertain commit":
				counted.writeErr = uncertain
			case "canceled":
				cancel()
			}
			execution := &turnExecution{state: &conversationState{session: counted}, turnUI: appendGateUI{allowed: func() bool { return allowed }}}
			err = execution.appendTurnMessages(ctx, []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Content: "saved"}})
			wantRows := 1
			if mode == "detached during read" || mode == "canceled" {
				wantRows = 0
			}
			if mode == "append" && err != nil || mode != "append" && err == nil {
				t.Fatalf("append error=%v", err)
			}
			if mode == "uncertain commit" && !errors.Is(err, uncertain) {
				t.Fatalf("uncertain error=%v", err)
			}
			if counted.plain != 0 || counted.payloads != 0 || counted.reads != 1 || counted.updates > 1 {
				t.Fatalf("unexpected calls: %+v", counted)
			}
			if mode != "canceled" && counted.identities != 1 {
				t.Fatalf("identity reads=%d", counted.identities)
			}
			if mode == "append" && counted.effects != 1 {
				t.Fatalf("effects=%d", counted.effects)
			}
			history := testHistory(t, session)
			if len(history) != wantRows || mode == "newer append" && history[0].Content != "newer" {
				t.Fatalf("sequence fence/replay violated: %+v", history)
			}
		})
	}
}

func TestHostAppendLegacyAndPlainParity(t *testing.T) {
	for _, plain := range []bool{false, true} {
		store := testOpenMemoryStore(t, nil)
		session := testAcquireSession(t, store, fmt.Sprint(plain))
		var supplied sessions.Session = struct {
			sessions.Session
			sessions.CoordinationSession
		}{session, session.(sessions.CoordinationSession)}
		if plain {
			supplied = struct{ sessions.Session }{session}
		}
		execution := &turnExecution{state: &conversationState{session: supplied}, turnUI: appendGateUI{allowed: func() bool { return true }}}
		if err := execution.appendTurnMessages(context.Background(), []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Content: "same"}}); err != nil {
			t.Fatal(err)
		}
		if got := testHistory(t, session); len(got) != 1 || got[0].Content != "same" {
			t.Fatalf("history=%+v", got)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
