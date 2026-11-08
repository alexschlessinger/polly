package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/sessiontest"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

// Embedding only the old capability forces the functional fallback, even once
// SQLite grows record methods. These tests are not native indexed acceptance.
type repositoryLegacyOnly struct{ sessions.CoordinationSession }

type repositoryMemoryFixture struct {
	sessions.CoordinationSession
	state          *sessions.CoordinationState
	reads, updates int
	readErr        error
}

func (f *repositoryMemoryFixture) ReadCoordination(context.Context) (*sessions.CoordinationState, error) {
	f.reads++
	return f.state, f.readErr
}
func (f *repositoryMemoryFixture) UpdateCoordination(_ context.Context, fn func(*sessions.CoordinationState) error) error {
	f.updates++
	copy := *f.state
	if err := fn(&copy); err != nil {
		return err
	}
	f.state = &copy
	return nil
}

func repositoryFixture() *repositoryMemoryFixture {
	return &repositoryMemoryFixture{state: &sessions.CoordinationState{
		ActorID: "actor", ParentID: "root", Records: map[string]map[string]json.RawMessage{
			"format": {"swarm": json.RawMessage(`{"version":2}`)},
		},
	}}
}

func repositoryFor(t *testing.T, handle any) *recordRepository {
	t.Helper()
	repository, err := openRecordRepository(handle)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func TestRecordRepositoryFormatGuard(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, format string
		want         error
	}{
		{"missing", "", ErrUnsupportedFormat},
		{"old", `{"version":1}`, ErrUnsupportedFormat},
		{"future", `{"version":3}`, ErrUnsupportedFormat},
		{"null", `null`, sessions.ErrCoordinationFormat},
		{"malformed", `{`, sessions.ErrCoordinationFormat},
		{"current", `{"version":2}`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := repositoryFixture()
			if test.format == "" {
				delete(fixture.state.Records, "format")
			} else {
				fixture.state.Records["format"]["swarm"] = json.RawMessage(test.format)
			}
			repository := repositoryFor(t, fixture)
			reads, updates := 0, 0
			readErr := repository.Read(t.Context(), func(r *recordReader) error { reads++; _, err := r.Member("absent"); return err })
			_, updateErr := repository.Update(t.Context(), func(tx *recordTx) error { updates++; return tx.PutMember(&Member{ID: "new"}) })
			if !errors.Is(readErr, test.want) || !errors.Is(updateErr, test.want) {
				t.Fatalf("read=%v update=%v, want %v", readErr, updateErr, test.want)
			}
			if test.want != nil && (reads != 0 || updates != 0 || len(fixture.state.Records["member"]) != 0) {
				t.Fatal("invalid format reached callback or wrote records")
			}
			if test.want == nil && (reads != 1 || updates != 1) {
				t.Fatal("callback not called once")
			}
		})
	}
	// An incompatible format is rejected even for an absent record, effects-only
	// update or parent-turn access. No query decides format based on its results.
	fixture := repositoryFixture()
	fixture.state.Records = map[string]map[string]json.RawMessage{"parent_turn": {"actor": json.RawMessage(`{}`)}}
	repository := repositoryFor(t, fixture)
	if err := repository.Read(t.Context(), func(r *recordReader) error { _, err := r.ParentTurn("actor"); return err }); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatal(err)
	}
	if _, err := repository.Update(t.Context(), func(tx *recordTx) error { return tx.Effects(sessions.CoordinationEffects{}) }); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatal(err)
	}
}

func TestRecordRepositoryOwnedTypedValuesAndLifetime(t *testing.T) {
	t.Parallel()
	fixture := repositoryFixture()
	repository := repositoryFor(t, fixture)
	var escapedReader *recordReader
	var escapedTx *recordTx
	var owned *Task
	if _, err := repository.Update(t.Context(), func(tx *recordTx) error {
		escapedTx = tx
		task := &Task{ID: "task", Dependencies: []string{"dep"}, Result: map[string]any{"nested": []any{"original"}}}
		if err := tx.PutTask(task); err != nil {
			return err
		}
		task.Dependencies[0] = "changed after Put"
		one, err := tx.Task("task")
		if err != nil {
			return err
		}
		one.Dependencies[0] = "changed after Get"
		one.Result.(map[string]any)["nested"].([]any)[0] = "changed"
		two, err := tx.Task("task")
		if err != nil {
			return err
		}
		if two.Dependencies[0] != "dep" || two.Result.(map[string]any)["nested"].([]any)[0] != "original" {
			t.Fatal("decode or Put aliases caller values")
		}
		owned = two
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Read(t.Context(), func(r *recordReader) error {
		escapedReader = r
		page, err := r.Tasks(sessions.RecordQuery{Index: sessions.CoordinationIndexKind, Key: "task", Limit: 1, IncludeTotal: true, MaxBytes: 1})
		if err != nil {
			return err
		}
		if page.Total != 1 || !page.Oversized || page.Next != "" || len(page.Records) != 1 || page.Records[0].Key.ID != "task" || page.Records[0].Revision.Token == "" {
			t.Fatalf("page: %+v", page)
		}
		page.Records[0].Value.Dependencies[0] = "query mutation"
		value, err := r.Task("task")
		if err != nil {
			return err
		}
		if value.Dependencies[0] != "dep" {
			t.Fatal("query aliases exact read")
		}
		missing, err := r.Task("missing")
		if missing != nil || err != nil {
			t.Fatal("missing record is not nil,nil")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	owned.Dependencies[0] = "retained value is owned"
	if _, err := escapedReader.Task("task"); !errors.Is(err, sessions.ErrCoordinationClosed) {
		t.Fatal(err)
	}
	if _, err := escapedReader.SlowSnapshot(); !errors.Is(err, sessions.ErrCoordinationClosed) {
		t.Fatal(err)
	}
	if err := escapedTx.PutTask(&Task{ID: "late"}); !errors.Is(err, sessions.ErrCoordinationClosed) {
		t.Fatal(err)
	}
	if err := escapedTx.Effects(sessions.CoordinationEffects{}); !errors.Is(err, sessions.ErrCoordinationClosed) {
		t.Fatal(err)
	}
}

func TestRecordRepositoryErrorsAndWorkflowShape(t *testing.T) {
	t.Parallel()
	fixture := repositoryFixture()
	sentinel := errors.New("host read failed")
	fixture.readErr = sentinel
	repository := repositoryFor(t, fixture)
	if err := repository.Read(t.Context(), func(*recordReader) error { t.Fatal("failed read called back"); return nil }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	fixture.readErr = nil
	fixture.state.Records["member"] = map[string]json.RawMessage{"bad": json.RawMessage(`{"tools":12}`), "null": json.RawMessage(`null`)}
	fixture.state.Records["workflow"] = map[string]json.RawMessage{"embedded": json.RawMessage(`{"id":"embedded","steps":[{}]}`)}
	if err := repository.Read(t.Context(), func(r *recordReader) error {
		_, err := r.Member("bad")
		var decode *json.UnmarshalTypeError
		if !errors.As(err, &decode) {
			t.Fatalf("decode identity lost: %v", err)
		}
		if _, err := r.Member("null"); !errors.Is(err, sessions.ErrCoordinationFormat) {
			t.Fatal(err)
		}
		if _, err := r.WorkflowHeader("embedded"); !errors.Is(err, sessions.ErrCoordinationFormat) {
			t.Fatal(err)
		}
		if _, err := r.Tasks(sessions.RecordQuery{Index: "unsupported", Limit: 1}); !errors.Is(err, sessions.ErrCoordinationSelector) {
			t.Fatal(err)
		}
		if _, err := r.Tasks(sessions.RecordQuery{Kind: "member", Limit: 1}); !errors.Is(err, sessions.ErrCoordinationRequest) {
			t.Fatal(err)
		}
		if _, err := r.WorkflowStep("workflow", -1); !errors.Is(err, sessions.ErrCoordinationRequest) {
			t.Fatal(err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fixture = repositoryFixture()
	repository = repositoryFor(t, fixture)
	if _, err := repository.Update(t.Context(), func(tx *recordTx) error {
		if err := tx.Delete(sessions.RecordKey{Kind: "format", ID: "swarm"}); !errors.Is(err, sessions.ErrCoordinationRequest) {
			t.Fatal(err)
		}
		if err := tx.PutTask(nil); !errors.Is(err, sessions.ErrCoordinationRequest) {
			t.Fatal(err)
		}
		err := tx.PutTask(&Task{ID: "bad", Result: make(chan int)})
		var encode *json.UnsupportedTypeError
		if !errors.As(err, &encode) {
			t.Fatalf("encode identity lost: %v", err)
		}
		if err := tx.PutWorkflowHeader(&workflow.ReportHeader{ID: "workflow"}); err != nil {
			return err
		}
		for i := 0; i < 12; i++ {
			if err := tx.PutWorkflowStep("workflow", i, &workflow.Step{ID: fmt.Sprint(i)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Read(t.Context(), func(r *recordReader) error {
		page, err := r.WorkflowSteps(sessions.RecordQuery{Index: sessions.CoordinationIndexWorkflowSteps, Key: "workflow", Limit: 20})
		if err != nil {
			return err
		}
		if len(page.Records) != 12 {
			t.Fatalf("steps=%d", len(page.Records))
		}
		for i, record := range page.Records {
			if record.Value.ID != fmt.Sprint(i) {
				t.Fatalf("step %d: %+v", i, record)
			}
		}
		snapshot, err := r.SlowSnapshot()
		if err != nil {
			return err
		}
		if len(snapshot.Workflows["workflow"].Steps) != 12 {
			t.Fatal("snapshot omitted steps")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// This supplied optional capability intentionally delegates to the fallback.
// It tests capability selection and errors, never pretends to be indexed.
type repositorySuppliedAccess struct {
	sessions.CoordinationSession // wrong authority, must never be unwrapped
	access                       sessions.CoordinationAccess
	reads, updates               int
	afterCommit                  error
	retry                        bool
}

func (a *repositorySuppliedAccess) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	a.reads++
	return a.access.ReadRecords(ctx, fn)
}
func (a *repositorySuppliedAccess) UpdateRecords(ctx context.Context, fn func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	a.updates++
	commit, err := a.access.UpdateRecords(ctx, func(tx sessions.CoordinationTx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if a.retry {
			return fn(tx)
		}
		return nil
	})
	if err == nil && a.afterCommit != nil {
		return commit, a.afterCommit
	}
	return commit, err
}

func TestRecordRepositorySuppliedAuthorityAndUncertainResult(t *testing.T) {
	t.Parallel()
	fixture := repositoryFixture()
	access, err := sessions.OpenCoordinationAccess(fixture)
	if err != nil {
		t.Fatal(err)
	}
	wrong := repositoryFixture()
	supplied := &repositorySuppliedAccess{CoordinationSession: wrong, access: access}
	repository := repositoryFor(t, supplied)
	if repository.Capabilities() != (sessions.CoordinationCapabilities{}) {
		t.Fatal("functional wrapper advertised native storage")
	}
	calls := 0
	veto := errors.New("callback veto")
	commit, err := repository.Update(t.Context(), func(tx *recordTx) error {
		calls++
		if err := tx.PutMember(&Member{ID: "member"}); err != nil {
			return err
		}
		return veto
	})
	if calls != 1 || !errors.Is(err, veto) || !reflect.DeepEqual(commit, sessions.CoordinationCommit{}) || len(fixture.state.Records["member"]) != 0 {
		t.Fatalf("veto: calls=%d commit=%+v err=%v", calls, commit, err)
	}
	supplied.retry = true
	calls = 0
	_, err = repository.Update(t.Context(), func(tx *recordTx) error { calls++; return tx.PutMember(&Member{ID: "member"}) })
	if calls != 1 || !errors.Is(err, sessions.ErrCoordinationRequest) || len(fixture.state.Records["member"]) != 0 {
		t.Fatalf("retry: calls=%d err=%v", calls, err)
	}
	supplied.retry = false
	supplied.afterCommit = errors.New("commit reply lost")
	calls = 0
	commit, err = repository.Update(t.Context(), func(tx *recordTx) error { calls++; return tx.PutMember(&Member{ID: "member"}) })
	if calls != 1 || !errors.Is(err, supplied.afterCommit) || !reflect.DeepEqual(commit, sessions.CoordinationCommit{}) {
		t.Fatalf("uncertain result: calls=%d commit=%+v err=%v", calls, commit, err)
	}
	if err := repository.Read(t.Context(), func(r *recordReader) error {
		member, err := r.Member("member")
		if member == nil {
			t.Fatal("durable result not inspectable")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if wrong.reads != 0 || wrong.updates != 0 || supplied.reads != 1 || supplied.updates != 3 {
		t.Fatal("supplied authority bypassed")
	}
}

func TestRecordRepositoryLegacyAtomicParity(t *testing.T) {
	t.Parallel()
	for _, factory := range sessiontest.Factories() {
		t.Run(factory.Name, func(t *testing.T) {
			fixture := factory.New(t)
			store := fixture.Open()
			parent, err := store.Acquire(t.Context(), "root", sessions.AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { parent.Close() })
			legacy := repositoryLegacyOnly{parent.(sessions.CoordinationSession)}
			if err := legacy.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
				raw.Records["format"] = map[string]json.RawMessage{"swarm": json.RawMessage(`{"version":2}`)}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			repository := repositoryFor(t, legacy)
			if repository.Capabilities() != (sessions.CoordinationCapabilities{}) {
				t.Fatal("forced fallback advertised native storage")
			}
			veto := errors.New("veto")
			calls := 0
			update := func(tx *recordTx) error {
				calls++
				identity, err := tx.Identity()
				if err != nil {
					return err
				}
				if err := tx.PutTask(&Task{ID: "task", Result: map[string]any{"value": "owned"}}); err != nil {
					return err
				}
				return tx.Effects(sessions.CoordinationEffects{ExpectedSequence: &identity.Sequence, Append: []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "atomic"}}, Transcript: &sessions.TranscriptIdentity{Execution: "execution", Generation: 4}})
			}
			if _, err := repository.Update(t.Context(), func(tx *recordTx) error {
				if err := update(tx); err != nil {
					return err
				}
				return veto
			}); !errors.Is(err, veto) {
				t.Fatal(err)
			}
			history, err := parent.GetHistory(t.Context())
			if err != nil || len(history) != 0 || calls != 1 {
				t.Fatalf("veto history=%v calls=%d err=%v", history, calls, err)
			}
			undo := fixture.FailCommit()
			_, err = repository.Update(t.Context(), update)
			undo()
			if err == nil || calls != 2 {
				t.Fatalf("commit failure: calls=%d err=%v", calls, err)
			}
			if err := repository.Read(t.Context(), func(r *recordReader) error {
				task, err := r.Task("task")
				if task != nil {
					t.Fatal("failed transaction wrote task")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			commit, err := repository.Update(t.Context(), update)
			if err != nil || calls != 3 || commit.Sequence != 1 || len(commit.Changed) == 0 {
				t.Fatalf("success: calls=%d commit=%+v err=%v", calls, commit, err)
			}
			view, err := store.(sessions.ViewStore).ReadView(t.Context(), sessions.ViewTarget{ID: legacy.ViewID()}, "")
			if err != nil {
				t.Fatal(err)
			}
			if view.Sequence != 1 || view.Metadata.Transcript == nil || view.Metadata.Transcript.Execution != "execution" || view.Metadata.Transcript.Generation != 4 || view.Metadata.Transcript.Sequence != 1 {
				t.Fatalf("atomic identity: %+v", view)
			}
			history, err = parent.GetHistory(t.Context())
			if err != nil || len(history) != 1 || history[0].Content != "atomic" {
				t.Fatalf("history=%v err=%v", history, err)
			}
		})
	}
}

func TestRecordRepositoryCancellationAndInvalidRequests(t *testing.T) {
	t.Parallel()
	for _, handle := range []any{nil, (*repositoryMemoryFixture)(nil), struct{}{}} {
		if _, err := openRecordRepository(handle); !errors.Is(err, sessions.ErrCoordinationUnsupported) {
			t.Fatal(err)
		}
	}
	fixture := repositoryFixture()
	repository := repositoryFor(t, fixture)
	if err := repository.Read(t.Context(), nil); !errors.Is(err, sessions.ErrCoordinationRequest) {
		t.Fatal(err)
	}
	if _, err := repository.Update(t.Context(), nil); !errors.Is(err, sessions.ErrCoordinationRequest) {
		t.Fatal(err)
	}
	veto := errors.New("read veto")
	if err := repository.Read(t.Context(), func(*recordReader) error { return veto }); !errors.Is(err, veto) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	_, err := repository.Update(ctx, func(tx *recordTx) error {
		calls++
		if err := tx.PutMember(&Member{ID: "cancelled"}); err != nil {
			return err
		}
		cancel()
		if _, err := tx.Member("cancelled"); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || calls != 1 || len(fixture.state.Records["member"]) != 0 {
		t.Fatalf("cancelled write: calls=%d err=%v", calls, err)
	}
	if err := repository.Read(ctx, func(*recordReader) error { t.Fatal("cancelled read called back"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := repository.Update(ctx, func(*recordTx) error { t.Fatal("cancelled update called back"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRecordRepositoryQueryContinuationAndDelete(t *testing.T) {
	t.Parallel()
	repository := repositoryFor(t, repositoryFixture())
	if _, err := repository.Update(t.Context(), func(tx *recordTx) error {
		for _, mail := range []*Mail{
			{ID: "later", To: "actor", Posted: time.Unix(2, 0)},
			{ID: "b", To: "actor", Posted: time.Unix(1, 0)},
			{ID: "a", To: "actor", Posted: time.Unix(1, 0)},
			{ID: "unrelated", To: "other"},
		} {
			if err := tx.PutMail(mail); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Read(t.Context(), func(r *recordReader) error {
		query := sessions.RecordQuery{Index: sessions.CoordinationIndexMailPending, Key: "actor", Limit: 2, IncludeTotal: true}
		first, err := r.Mails(query)
		if err != nil {
			return err
		}
		if first.Total != 3 || first.Next == "" || len(first.Records) != 2 || first.Records[0].Value.ID != "a" || first.Records[1].Value.ID != "b" {
			t.Fatalf("first=%+v", first)
		}
		query.After = first.Next
		last, err := r.Mails(query)
		if err != nil {
			return err
		}
		if last.Total != 3 || last.Next != "" || len(last.Records) != 1 || last.Records[0].Value.ID != "later" {
			t.Fatalf("last=%+v", last)
		}
		revisions, err := r.Revisions([]sessions.RevisionScope{{Index: query.Index, Key: query.Key}})
		if err != nil {
			return err
		}
		if len(revisions) != 1 || revisions[0].Token == "" {
			t.Fatal("missing revision")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Update(t.Context(), func(tx *recordTx) error {
		if err := tx.Delete(sessions.RecordKey{Kind: "mail", ID: "a"}); err != nil {
			return err
		}
		mail, err := tx.Mail("a")
		if mail != nil {
			t.Fatal("delete not visible inside transaction")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecordRepositoryExplicitDependencies(t *testing.T) {
	t.Parallel()
	fixture := repositoryFixture()
	repository := repositoryFor(t, fixture)
	if _, err := repository.Update(t.Context(), func(tx *recordTx) error {
		for _, put := range []func() error{
			func() error {
				return tx.PutMember(&Member{ID: "actor", Context: "context", Task: "task", Execution: "execution"})
			},
			func() error { return tx.PutContext(&ExecutionContext{ID: "context", Owner: "actor"}) },
			func() error {
				return tx.PutExecution(&Execution{ID: "execution", Member: "actor", Run: "run", Base: "snapshot", Workflow: "workflow"})
			},
			func() error { return tx.PutRun(&Run{ID: "run"}) },
			func() error {
				return tx.PutTask(&Task{ID: "task", Owner: "actor", Execution: "execution", Dependencies: []string{"dependency", "absent", "dependency"}})
			},
			func() error { return tx.PutTask(&Task{ID: "dependency", Dependencies: []string{"not-transitive"}}) },
			func() error { return tx.PutSnapshot(&worktree.Snapshot{ID: "snapshot", Commit: "retained"}) },
			func() error { return tx.PutWorkflowHeader(&workflow.ReportHeader{ID: "workflow"}) },
			func() error { return tx.PutMail(&Mail{ID: "mail", To: "actor", Task: "task", Workflow: "workflow"}) },
			func() error { return tx.PutPublication(&Publication{ID: "publication"}) },
		} {
			if err := put(); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A poisoned unrelated kind makes a whole snapshot fail, but must not be
	// visited by exact operation dependencies (including absent direct deps).
	fixture.state.Records["preview"] = map[string]json.RawMessage{"unrelated": json.RawMessage(`{`)}
	if err := repository.Read(t.Context(), func(r *recordReader) error {
		slice, err := r.ExecutionSlice("execution")
		if err != nil {
			return err
		}
		if slice.Member.ID != "actor" || slice.Context.ID != "context" || slice.Run.ID != "run" || slice.Workflow.ID != "workflow" || slice.Base.Commit != "retained" || len(slice.Task.Dependencies) != 2 {
			t.Fatalf("slice=%+v", slice)
		}
		if dependency, found := slice.Task.Dependencies["absent"]; !found || dependency != nil {
			t.Fatal("lost exact missing dependency")
		}
		bundle, err := r.Checkpoint("execution", admissionReceipts{MailIDs: []string{"mail", "mail", "missing"}, PublicationIDs: []string{"publication"}})
		if err != nil {
			return err
		}
		if bundle.Actor.ID != "actor" || bundle.Execution.ID != "execution" || len(bundle.Mail) != 2 || bundle.Mail["missing"] != nil || len(bundle.Tasks) != 1 || len(bundle.Publications) != 1 || len(bundle.Workflows) != 1 {
			t.Fatalf("checkpoint=%+v", bundle)
		}
		if _, err := r.SlowSnapshot(); err == nil {
			t.Fatal("poisoned full snapshot unexpectedly succeeded")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecordRepositorySlowBootstrapAndWorkflowSuppression(t *testing.T) {
	t.Parallel()
	fixture := repositoryFixture()
	state, err := decodeState(fixture.state)
	if err != nil {
		t.Fatal(err)
	}
	state.Members["member"] = &Member{ID: "member", Context: "context", Task: "task", Execution: "execution", ReadOnly: true}
	state.Contexts["context"] = &ExecutionContext{ID: "context", Owner: "member"}
	state.Executions["execution"] = &Execution{ID: "execution", Member: "member", Status: "waiting"}
	state.Tasks["task"] = &Task{ID: "task", Owner: "member", Execution: "execution", Status: "running", Requirement: RequirementDelivered}
	state.Workflows["workflow"] = &workflow.Report{ID: "workflow", CallID: "call", Status: "completed", Output: "delivered output"}
	state.Messages["notice"] = &Mail{ID: "notice", To: "actor", From: "workflow", Workflow: "workflow", Text: "completed"}
	if err := encodeState(fixture.state, state); err != nil {
		t.Fatal(err)
	}
	repository := repositoryFor(t, fixture)
	if err := repository.Read(t.Context(), func(r *recordReader) error {
		bootstrap, err := r.SlowExecutionBootstrap("execution")
		if err != nil {
			return err
		}
		if bootstrap.Slice.Member.ID != "member" || bootstrap.Roster != compactRoster(state) || bootstrap.WaitState != waitState(state, "member") || bootstrap.Requirement != RequirementDelivered {
			t.Fatalf("bootstrap=%+v", bootstrap)
		}
		batch, err := r.SlowAdmission("", nil)
		if err != nil {
			return err
		}
		if len(batch.Messages) != 1 || !strings.Contains(batch.Messages[0].Content, "delivered output") {
			t.Fatal("workflow dependency output omitted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	call := messages.ChatMessageToolCall{ID: "call", Name: "workflow_run"}
	result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolName: "workflow_run", ToolCallID: "call", Content: "completed", Metadata: map[string]any{"tool_data": map[string]any{"kind": workflowDeliveryKind, "workflow": "workflow", "callID": "call", "status": "completed"}}}
	staged := &workflowDeliveries{}
	staged.stage(call, result)
	callbacks := &llm.AgentCallbacks{}
	(&Runtime{}).bindCheckpoint(fixture, "", 0, 0, callbacks, nil, nil)
	callbacks.OnToolResult(call, result)
	want, err := callbacks.AdmitInput(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Read(t.Context(), func(r *recordReader) error {
		batch, err := r.SlowAdmission("", staged)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(batch.Messages, want) || len(batch.Messages) != 0 || len(batch.Receipts.MailIDs) != 0 {
			t.Fatalf("staged workflow notice was not suppressed: %+v", batch)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecordRepositorySlowAdmissionParity(t *testing.T) {
	t.Parallel()
	for _, parent := range []bool{false, true} {
		for _, mailCount := range []int{0, 1, 20} {
			t.Run(fmt.Sprintf("parent=%v/mail=%d", parent, mailCount), func(t *testing.T) {
				fixture := repositoryFixture()
				state, err := decodeState(fixture.state)
				if err != nil {
					t.Fatal(err)
				}
				execution := "execution"
				if parent {
					execution = ""
				} else {
					state.Members["actor"] = &Member{ID: "actor", Execution: "execution", Publications: PublicationMark{Posted: time.Unix(5, 0)}}
					state.Executions["execution"] = &Execution{ID: "execution", Member: "actor", Run: "run"}
				}
				state.Parents["actor"] = &ParentRecord{Publications: PublicationMark{Posted: time.Unix(5, 0)}}
				state.Snapshots["one"] = &worktree.Snapshot{ID: "one", Commit: "same-retained-commit"}
				state.Snapshots["two"] = &worktree.Snapshot{ID: "two", Commit: "same-retained-commit"}
				state.Publications["old"] = &Publication{ID: "old", Author: "author", Run: "run", Scope: "snapshot", Key: "fact", Value: "old", Snapshot: "one", Posted: time.Unix(1, 0)}
				state.Publications["promoted"] = &Publication{ID: "promoted", Author: "peer", Run: "run", Scope: "snapshot", Key: "fact", Value: "old", Snapshot: "two", DuplicateOf: "old", Posted: time.Unix(2, 0)}
				state.Publications["coalesced"] = &Publication{ID: "coalesced", Author: "peer2", Run: "run", Scope: "snapshot", Key: "fact", Value: "old", Snapshot: "one", DuplicateOf: "old", Posted: time.Unix(3, 0)}
				state.Publications["correction"] = &Publication{ID: "correction", Author: "author", Run: "run", Scope: "snapshot", Key: "fact", Value: "new", Snapshot: "one", Supersedes: "old", Posted: time.Unix(6, 0)}
				for i := 0; i < mailCount; i++ {
					id := fmt.Sprintf("mail-%02d", i)
					state.Messages[id] = &Mail{ID: id, To: "actor", From: "peer", Text: strings.Repeat("body", 200), Posted: time.Unix(int64(i), 0)}
				}
				if mailCount == 1 {
					mail := state.Messages["mail-00"]
					mail.Text = strings.Repeat("oversized ordinary mail ", 4000)
					mail.Task, mail.Revision, mail.Execution, mail.Workflow = "task", 2, "result-execution", "workflow"
					state.Tasks["task"] = &Task{ID: "task", Revision: 2, Execution: "result-execution", Result: "task output"}
					state.Workflows["workflow"] = &workflow.Report{ID: "workflow", Output: "workflow output"}
					state.Followups[mail.ID] = &FollowupCall{Phase: "launched", Base: "one", BaseOrigin: "parent", Task: "task", Execution: "result-execution"}
				}
				if err := encodeState(fixture.state, state); err != nil {
					t.Fatal(err)
				}
				callbacks := &llm.AgentCallbacks{}
				(&Runtime{}).bindCheckpoint(fixture, execution, 0, 0, callbacks, nil, nil)
				want, err := callbacks.AdmitInput(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				var batch admissionBatch
				if err := repositoryFor(t, fixture).Read(t.Context(), func(r *recordReader) error { var err error; batch, err = r.SlowAdmission(execution, nil); return err }); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(batch.Messages, want) {
					t.Fatalf("admission diverged:\ngot=%+v\nwant=%+v", batch.Messages, want)
				}
				if len(batch.Messages) != 1 || len(batch.Messages[0].Content) > admissionBytes {
					t.Fatalf("invalid envelope: %+v", batch)
				}
				if mailCount == 20 && (len(batch.Receipts.MailIDs) != admissionMessages || len(batch.Receipts.PublicationIDs) != 0) {
					t.Fatal("mail priority or count cap changed")
				}
				if mailCount < admissionMessages && !reflect.DeepEqual(batch.Receipts.PublicationIDs, []string{"promoted", "correction"}) {
					t.Fatalf("historical survivor or snapshot coalescing changed: %+v", batch.Receipts)
				}
				if len(batch.Receipts.PublicationIDs) > 0 {
					batch.Receipts.PublicationIDs[0] = "changed"
					if batch.Messages[0].Metadata[messages.MetadataKeySwarmPublications].([]string)[0] != "promoted" {
						t.Fatal("receipt IDs alias message metadata")
					}
				}
			})
		}
	}
}
