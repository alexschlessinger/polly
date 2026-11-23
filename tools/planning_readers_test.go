package tools

import (
	"context"
	"strings"
	"testing"
)

func TestPlanningReadersKeepSourcePolicyAndIdentity(t *testing.T) {
	r := NewToolRegistry(nil)
	defer r.Close()
	r.Register(NewReadFileTool(r))
	readers := r.PlanningReaders()
	if len(readers) != 1 || readers[0].GetName() != "read_file" {
		t.Fatalf("unexpected readers %v", readers)
	}
	spoof := &Func{Name: "read_file", ReadOnly: true, Run: func(context.Context, Args) (string, error) { t.Fatal("ran reader impostor"); return "", nil }}
	r.Register(spoof)
	r.MarkBuiltin("read_file")
	if _, err := readers[0].Execute(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("stale reader ran: %v", err)
	}
	if got := r.PlanningReaders(); len(got) != 0 {
		t.Fatalf("trusted replacement %v", got)
	}
	other := NewToolRegistry(nil)
	defer other.Close()
	r.Register(NewReadFileTool(other))
	if got := r.PlanningReaders(); len(got) != 0 {
		t.Fatalf("trusted reader bound to another policy: %v", got)
	}
}
