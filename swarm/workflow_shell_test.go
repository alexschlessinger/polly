package swarm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/workflow"
)

func TestWorkflowRealShellCancellationAndIncompleteCapture(t *testing.T) {
	skipIfWindows(t)
	for _, outcome := range []string{"cancel", "incomplete-zero", "incomplete-nonzero"} {
		t.Run(outcome, func(t *testing.T) {
			r := runtimeTest(t, nilModel(), 1, 1)
			if _, err := r.config.Registry.LoadToolAuto("bash"); err != nil {
				t.Fatal(err)
			}
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			quoted := "'" + strings.ReplaceAll(pidFile, "'", "'\"'\"'") + "'"
			ending := "wait"
			if outcome == "incomplete-zero" {
				ending = "exit 0"
			} else if outcome == "incomplete-nonzero" {
				ending = "exit 7"
			}
			command := "printf 'workflow-prefix\\n'; sleep 30 & printf '%s\\n' \"$!\" > " + quoted + "; " + ending
			source := `polly.defineWorkflow({name:"shell-lifecycle",inputSchema:polly.schema.object({command:polly.schema.string()}),async run(input){
 const context=await polly.context({readOnly:true});
 return polly.scope({context}, w=>w.exec(input.command,{check:false}));
}});`
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				report *workflow.Report
				err    error
			}
			done := make(chan result, 1)
			go func() {
				report, err := r.RunWorkflow(ctx, source, map[string]any{"command": command})
				done <- result{report, err}
			}()
			// The PID receipt is written after the real shell launches its child.
			deadline := time.NewTimer(10 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			var child *os.Process
			for child == nil {
				data, err := os.ReadFile(pidFile)
				if err == nil && strings.HasSuffix(string(data), "\n") {
					pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
					if err == nil && pid > 0 {
						child, err = os.FindProcess(pid)
						if err != nil {
							t.Fatal(err)
						}
						defer child.Release()
						defer child.Kill()
						break
					}
				}
				select {
				case <-tick.C:
				case early := <-done:
					t.Fatalf("workflow failed before shell startup: %v", early.err)
				case <-deadline.C:
					t.Fatal("workflow shell did not start")
				}
			}
			if outcome == "cancel" {
				cancel()
			}
			var got result
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("workflow did not drain its real shell call")
			}
			want := "failed"
			if outcome == "cancel" {
				want = "interrupted"
				if !errors.Is(got.err, context.Canceled) {
					t.Fatalf("cancellation identity lost: %v", got.err)
				}
			} else {
				var failure *workflow.Error
				if !errors.As(got.err, &failure) || failure.Code != "tool_failed" || !strings.Contains(failure.Message, "command output incomplete") {
					t.Fatalf("check:false swallowed incomplete capture: %v", got.err)
				}
				if !strings.Contains(fmt.Sprint(failure.Result), "workflow-prefix") {
					t.Fatalf("partial output lost: %+v", failure)
				}
			}
			if got.report == nil || got.report.Status != want || got.report.Finished.IsZero() {
				t.Fatalf("workflow result: %+v", got)
			}
			state, err := r.State(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			saved := state.Workflows[got.report.ID]
			if saved.Status != want || len(saved.Steps) != 2 || saved.Steps[1].Status == "running" {
				t.Fatalf("shell outcome not durably recorded: %+v", saved)
			}
		})
	}
}
