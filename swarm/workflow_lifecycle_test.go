package swarm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/workflow"
)

func waitWorkflowIdle(t *testing.T, r *Runtime) *State {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		r.mu.Lock()
		changed := r.notify
		r.mu.Unlock()
		if !r.HasActive() {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("workflow did not settle")
		}
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWorkflowLaunchModesShareDurabilityAndLifetime(t *testing.T) {
	for _, background := range []bool{false, true} {
		for _, finish := range []string{"success", "cancel", "shutdown"} {
			mode := "foreground"
			if background {
				mode = "background"
			}
			t.Run(mode+"/"+finish, func(t *testing.T) {
				started, release := make(chan struct{}), make(chan struct{})
				type contextKey struct{}
				r := runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
					if ctx.Value(contextKey{}) != "launch" {
						t.Error("launch lost context values")
					}
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
					}
					return answer("done")
				}), 1, 1)
				ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "launch"))
				defer cancel()
				source := `polly.defineWorkflow({name:"lifecycle",inputSchema:polly.schema.object({}),async run(){return await polly.agent({label:"Test agent",task:"inspect",readOnly:true});}})`
				type result struct {
					report *workflow.Report
					err    error
				}
				done := make(chan result, 1)
				id := ""
				if background {
					var err error
					id, err = r.StartWorkflow(ctx, source, map[string]any{})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					go func() { report, err := r.RunWorkflow(ctx, source, map[string]any{}); done <- result{report, err} }()
				}
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("workflow did not start")
				}
				s, err := r.State(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(s.Workflows) != 1 || len(s.Members) != 1 || len(s.Executions) != 1 {
					t.Fatalf("launch did not persist: %+v", s)
				}
				for key, report := range s.Workflows {
					if id != "" && id != key {
						t.Fatal("returned another workflow ID")
					}
					id = key
					if report.Status != "running" || report.Source != source || report.Run == "" {
						t.Fatalf("initial report: %+v", report)
					}
				}
				for _, m := range s.Members {
					if m.Controller != id {
						t.Fatal("member was not reserved to attempt")
					}
				}
				if background {
					cancel() // A command ending cannot cancel its background workflow.
					select {
					case <-release:
						t.Fatal("test released work early")
					case <-time.After(20 * time.Millisecond):
					}
					s, err = r.State(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if s.Workflows[id].Status != "running" {
						t.Fatal("background inherited caller cancellation")
					}
				}
				switch finish {
				case "success":
					close(release)
				case "cancel":
					if background {
						if err := r.CancelWorkflow(id); err != nil {
							t.Fatal(err)
						}
					} else {
						cancel()
					}
				case "shutdown":
					if err := r.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if !background {
					select {
					case got := <-done:
						if got.report == nil || got.report.ID != id {
							t.Fatalf("foreground receipt: %+v", got)
						}
						if (got.err == nil) != (finish == "success") {
							t.Fatalf("foreground result: %+v", got)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("foreground did not return")
					}
				}
				s = waitWorkflowIdle(t, r)
				report := s.Workflows[id]
				want := "completed"
				if finish != "success" {
					want = "interrupted"
				}
				if report.Status != want || report.Finished.IsZero() {
					t.Fatalf("terminal report: %+v", report)
				}
				for _, m := range s.Members {
					if finish == "success" && m.Controller != "" {
						t.Fatal("successful attempt retained member reservation")
					}
					if finish != "success" && (m.Controller != id || MemberState(s, m).Lifecycle != LifecyclePaused) {
						t.Fatalf("interrupted reservation: %+v", m)
					}
				}
				if err := r.CancelWorkflow(id); err == nil {
					t.Fatal("finished controller remained registered")
				}
			})
		}
	}
}

func TestWorkflowLaunchFailureDoesNotRegisterOrDispatch(t *testing.T) {
	for _, background := range []bool{false, true} {
		for _, failure := range []string{"input", "canceled", "closed"} {
			t.Run(map[bool]string{false: "foreground", true: "background"}[background]+"/"+failure, func(t *testing.T) {
				r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
					t.Error("unexpected model call")
					return answer("done")
				}), 1, 1)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var input any = map[string]any{}
				switch failure {
				case "input":
					input = func() {}
				case "canceled":
					cancel()
				case "closed":
					r.Close()
				}
				source := `polly.defineWorkflow({name:"never",inputSchema:polly.schema.object({}),async run(){return "unused";}})`
				var err error
				if background {
					_, err = r.StartWorkflow(ctx, source, input)
				} else {
					_, err = r.RunWorkflow(ctx, source, input)
				}
				if err == nil {
					t.Fatal("invalid launch succeeded")
				}
				if failure == "canceled" && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				r.mu.Lock()
				registered := len(r.active) + len(r.workflowCancels)
				r.mu.Unlock()
				if registered > 0 {
					t.Fatal("failed launch retained execution registration")
				}
				s, err := r.State(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(s.Workflows) != 0 || len(s.Members) != 0 {
					t.Fatal("failed launch left a running record")
				}
			})
		}
	}
}
