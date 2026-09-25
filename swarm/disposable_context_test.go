package swarm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

// A workflow's check copy can be left holding build outputs: a compiler
// writes its binary beside the sources. An ordinary copy that changed is
// retained, because what it holds might be work. A copy declared disposable
// when it was created is released whatever it holds, and in exchange nothing
// in it can become work: it cannot be captured, cannot seed an agent or
// another copy, and no agent request can ask for one.
func TestDisposableContextReleasesWithLeftoverFiles(t *testing.T) {
	t.Parallel()
	r := scratchRuntime(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), true)
	if _, err := r.config.Registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"disposable",inputSchema:polly.schema.object({}),async run(){
  const kept = await polly.context({});
  const scratch = await polly.context({disposable: true});
  const orphan = await polly.context({disposable: true}); // never released by the script
  const roots = {};
  for (const [name, c] of Object.entries({kept, scratch, orphan})) {
    roots[name] = (await polly.exec("echo built > leftover.bin && pwd -P", {context: c})).text.trim();
  }
  const refused = {};
  for (const [name, attempt] of Object.entries({
    snapshot: () => polly.snapshot(scratch),
    seedContext: () => polly.context({context: scratch}),
    seedAgent: () => polly.research("seeded", "look", {context: scratch}),
    agentFlag: () => polly.research("flagged", "look", {disposable: true}),
    notBoolean: () => polly.context({disposable: "yes"}),
  })) {
    try { await attempt(); refused[name] = "accepted"; } catch (e) { refused[name] = e.message; }
  }
  let keptError = "released";
  try { await polly.release(kept); } catch (e) { keptError = e.code; }
  return {kept, scratch, orphan, roots, refused, keptError, released: await polly.release(scratch)};
}})`, map[string]any{})
	if err != nil {
		t.Fatalf("workflow: %v %+v", err, report)
	}
	output := report.Output.(map[string]any)
	if output["keptError"] != "unintegrated_changes" {
		t.Fatalf("an ordinary changed copy was not retained: %#v", output["keptError"])
	}
	scratch := output["scratch"].(string)
	if released := output["released"].(map[string]any); released["released"] != scratch {
		t.Fatalf("disposable copy was not released: %#v", released)
	}
	for name, want := range map[string]string{
		"snapshot": "cannot be captured", "seedContext": "cannot seed", "seedAgent": "cannot seed",
		"agentFlag": "disposable", "notBoolean": "must be a boolean",
	} {
		if got, _ := output["refused"].(map[string]any)[name].(string); !strings.Contains(got, want) {
			t.Errorf("%s: %q, want a refusal naming %q", name, got, want)
		}
	}
	roots := output["roots"].(map[string]any)
	if _, err := os.Stat(roots["scratch"].(string)); !os.IsNotExist(err) {
		t.Fatalf("disposable copy's files remain: %v", err)
	}
	if _, err := os.Stat(roots["kept"].(string)); err != nil {
		t.Fatalf("retained copy's files are gone: %v", err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Contexts[scratch] != nil || s.Contexts[output["kept"].(string)] == nil {
		t.Fatalf("contexts after release: %+v", s.Contexts)
	}
	// No refused attempt left a member or a copy behind.
	if len(s.Members) != 0 || len(s.Contexts) != 2 {
		t.Fatalf("%d members, %d contexts", len(s.Members), len(s.Contexts))
	}
	// A copy the script never released is cleaned up the same way: the
	// declaration lives on its record, not in the release call.
	orphan := output["orphan"].(string)
	if !s.Contexts[orphan].Disposable {
		t.Fatalf("orphan record lost its declaration: %+v", s.Contexts[orphan])
	}
	if err := r.Cleanup(ctx, output["kept"].(string)); err == nil {
		t.Fatal("cleanup removed an ordinary changed copy")
	}
	if err := r.Cleanup(ctx, orphan); err != nil {
		t.Fatalf("cleanup of a disposable copy: %v", err)
	}
	if _, err := os.Stat(roots["orphan"].(string)); !os.IsNotExist(err) {
		t.Fatalf("orphaned disposable copy's files remain: %v", err)
	}
}
