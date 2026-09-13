package swarm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowValidationShellExitSemantics(t *testing.T) {
	skipIfWindows(t)
	for _, tc := range []struct {
		name, command string
		check         bool
		failed        bool
		code          int
	}{
		{"required check", "false", true, true, 1},
		{"collected failure", "false", false, false, 1},
		{"failed sequence stops", "false; printf 'later success\\n'", true, true, 1},
		{"failed pipeline stops", "false | cat; printf 'later success\\n'", true, true, 1},
		{"collected sequence remains strict", "false; printf 'later success\\n'", false, false, 1},
		{"collected pipeline remains strict", "false | cat; printf 'later success\\n'", false, false, 1},
		{"successful sequence", "true; printf 'later success\\n'", true, false, 0},
		{"successful pipeline", "printf 'success\\n' | cat", true, false, 0},
		{"sequence opt out", "set +e; false; printf 'later success\\n'", true, false, 0},
		{"pipeline opt out", "set +o pipefail; false | cat; printf 'later success\\n'", true, false, 0},
		{"both options disabled", "set +e; set +o pipefail; false; false | cat; printf 'later success\\n'", true, false, 0},
		{"expected failure", "if false; then exit 2; else printf 'expected\\n'; fi", true, false, 0},
		{"explicit pipeline propagation", "set -o pipefail; false | cat", true, true, 1},
		{"explicit sequence propagation", "false || exit $?; printf 'unreachable\\n'", true, true, 1},
		{"errexit does not check every command", "false && printf 'unreachable\\n'; printf 'later success\\n'", true, false, 0},
		{"portable exact bytes", "set -o pipefail; printf 'features=base,alpha,beta\\n' | cmp - features.txt", true, false, 0},
		{"portable byte inspection", "od -c features.txt", true, false, 0},
		{"grep expected no matches", "if grep -q absent features.txt; then exit 1; else code=$?; if [ \"$code\" -eq 1 ]; then exit 0; fi; exit \"$code\"; fi", true, false, 0},
		{"grep errors are not no matches", "if grep -q absent missing-file; then exit 1; else code=$?; if [ \"$code\" -eq 1 ]; then exit 0; fi; exit \"$code\"; fi", true, true, 2},
		{"stderr is not status", "printf 'error: diagnostic only\\n' >&2", true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runtimeTest(t, nilModel(), 1, 1)
			if err := os.WriteFile(filepath.Join(r.config.Root, "features.txt"), []byte("features=base,alpha,beta\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := r.config.Registry.LoadToolAuto("bash"); err != nil {
				t.Fatal(err)
			}
			source := `polly.defineWorkflow({name:"required-check",inputSchema:polly.schema.object({command:polly.schema.string(),check:polly.schema.boolean()}),async run(input){
 const context=await polly.context({readOnly:true});
 const result=await polly.exec(input.command,{context,check:input.check});
 await polly.log("after-check"); return result;
 }});`
			report, err := r.RunWorkflow(context.Background(), source, map[string]any{"command": tc.command, "check": tc.check})
			if (err != nil) != tc.failed {
				t.Fatalf("workflow=%+v err=%v", report, err)
			}
			var result map[string]any
			if tc.failed {
				if report.Error.Code != "command_failed" || len(report.Steps) != 2 {
					t.Fatalf("required check did not stop subsequent work: %+v", report)
				}
				result = report.Error.Result.(map[string]any)
			} else {
				if len(report.Steps) != 3 {
					t.Fatalf("workflow did not continue after collected result: %+v", report)
				}
				result = report.Output.(map[string]any)
			}
			if result["exitCode"] != float64(tc.code) {
				t.Fatalf("exit result = %#v, want exit %d", result, tc.code)
			}
			if tc.code != 0 && strings.Contains(result["text"].(string), "later success") {
				t.Fatalf("shell continued after required command failed: %#v", result)
			}
		})
	}
}
