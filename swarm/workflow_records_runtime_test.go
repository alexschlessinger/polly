package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

type workflowRuntimeSession struct {
	sessions.Session
	*workflowRecordSession
}

func TestWorkflowRecordsRuntimeUsesDeltas(t *testing.T) {
	t.Parallel()
	for _, steps := range []int{16, 128} {
		t.Run(fmt.Sprint(steps), func(t *testing.T) {
			var counted *workflowRecordSession
			r := runtimeTestWithParent(t, nilModel(), 1, 1, func(parent sessions.Session) sessions.Session {
				access, err := sessions.OpenCoordinationAccess(parent)
				if err != nil {
					t.Fatal(err)
				}
				counted = &workflowRecordSession{CoordinationSession: parent.(sessions.CoordinationSession), access: access}
				return &workflowRuntimeSession{Session: parent, workflowRecordSession: counted}
			})
			// Measure recording separately from automatic workspace release.
			// RunWorkflow still performs its real preparation and final teardown.
			suspendAutoRelease(t, r)
			counted.resetCounts()
			source := fmt.Sprintf(`polly.workflow("native",polly.schema.obj({}),async()=>{for(let i=0;i<%d;i++)await polly.log(String(i));return "done";});`, steps)
			report, err := r.RunWorkflow(t.Context(), source, map[string]any{})
			if err != nil || report == nil || len(report.Steps) != steps || report.Status != "completed" {
				t.Fatalf("runtime workflow: report=%+v err=%v", report, err)
			}
			counts := counted.snapshot()
			// A full-report recorder reads the growing prefix on every save. The
			// production host must instead perform bounded work per checkpoint.
			// Controller teardown and cold format bootstrap use record
			// transactions too; no phase may fall back to the broad family API.
			if counts.callbacks != 2*steps+5 || counts.broad != 0 || counts.gets > 6*steps+10 {
				t.Fatalf("runner did not use native deltas: steps=%d counts=%+v", steps, counts)
			}
			writes := 0
			for _, key := range counts.written {
				if key.Kind == "workflow_step" {
					writes++
				}
			}
			if writes != 2*steps {
				t.Fatalf("step writes=%d want=%d", writes, 2*steps)
			}
			saved := workflowRecordsState(t, counted).Workflows[report.ID]
			if saved == nil || len(saved.Steps) != steps || saved.Status != "completed" || saved.Output != "done" {
				t.Fatalf("runtime lost complete saved report: %+v", saved)
			}
			t.Logf("production runtime: steps=%d record callbacks=%d gets=%d step writes=%d; broad lifecycle calls=%d (outside per-checkpoint bound)", steps, counts.callbacks, counts.gets, writes, counts.broad)
		})
	}
}

func TestWorkflowRecordsCancellationDrainsReceipt(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			r, session := workflowRecordsRuntime(t, legacy)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			host := &workflowPersistenceHost{Runtime: r}
			host.call = func(ctx context.Context, op workflow.Operation) (any, error) {
				// This observation runs at the actual host-effect boundary, after
				// the runner's synchronous intent save, not inside its transaction.
				err := session.access.ReadRecords(ctx, func(reader sessions.CoordinationReader) error {
					record, found, err := reader.Get(sessions.RecordKey{Kind: "workflow_step", ID: "w/0"})
					if err != nil {
						return err
					}
					if !found {
						return errors.New("effect started without durable intent")
					}
					var step workflow.Step
					if err := json.Unmarshal(record.Payload, &step); err != nil {
						return err
					}
					if step.Status != "running" || step.ID != op.ID || step.Kind != op.Kind {
						return fmt.Errorf("wrong durable intent: %+v", step)
					}
					return nil
				})
				close(started)
				if err != nil {
					return nil, err
				}
				<-ctx.Done()
				close(canceled)
				<-release
				return "committed receipt", nil
			}
			done := make(chan struct{})
			var report *workflow.Report
			var runErr error
			go func() {
				defer close(done)
				runner := workflow.Runner{Host: host, Config: workflow.Config{RunID: "w"}}
				report, runErr = runner.Run(ctx, `polly.workflow("cancel",polly.schema.obj({}),async()=>await polly.log("effect"));`, map[string]any{})
			}()
			// Always release the host, including a failed assertion, so cleanup
			// cannot leave a blocked effect holding the test's storage handle.
			releaseEffect := sync.OnceFunc(func() { close(release) })
			defer func() { cancel(); releaseEffect(); <-done }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("host effect did not start")
			}
			cancel()
			select {
			case <-canceled:
			case <-done:
				t.Fatalf("host effect failed before cancellation: %v", runErr)
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not reach host")
			}
			before := workflowRecordsState(t, session)
			if before.Workflows["w"].Status != "running" || len(before.Messages) != 0 {
				t.Fatal("terminal checkpoint preceded the in-flight receipt")
			}
			releaseEffect()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("runner did not drain completed receipt")
			}
			saved := workflowRecordsState(t, session)
			w := saved.Workflows["w"]
			if !errors.Is(runErr, context.Canceled) || report == nil || report.Status != "interrupted" ||
				w.Status != "interrupted" || len(w.Steps) != 1 || w.Steps[0].Status != "completed" ||
				w.Steps[0].Value != "committed receipt" || len(saved.Messages) != 1 || host.calls.Load() != 1 {
				t.Fatalf("cancellation lost/replayed receipt: report=%+v saved=%+v err=%v", report, w, runErr)
			}
		})
	}
}
