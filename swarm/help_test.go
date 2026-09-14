package swarm

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestHelpToolsAreStatelessAndKeepDefinitions(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		t.Error("help called the model")
		return answer("unexpected call")
	}), 1, 1)
	r.RegisterParentTools(r.config.Registry)
	before, err := r.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := json.Marshal(r.config.Registry.GetSchemas())
	if err != nil {
		t.Fatal(err)
	}
	for name, guide := range map[string]string{"swarm_help": coordinationGuide, "workflow_help": workflowGuide} {
		t.Run(name, func(t *testing.T) {
			helper, exists, allowed := r.config.Registry.GetIfAllowed(name)
			if !exists || !allowed {
				t.Fatalf("parent has no %s tool", name)
			}
			if s := helper.GetSchema(); len(s.Properties()) != 0 || len(s.Required()) != 0 {
				t.Fatalf("help takes arguments: %+v", s)
			}
			for range 2 {
				result, err := helper.Execute(context.Background(), nil)
				if err != nil || result != guide || result == "" || json.Valid([]byte(result)) {
					t.Fatalf("help did not return the same plain text: %q, %v", result, err)
				}
			}
		})
	}
	if !strings.Contains(coordinationGuide, "workflow_help()") || strings.Contains(coordinationGuide, "polly.defineWorkflow") || !strings.Contains(workflowGuide, "polly.defineWorkflow") {
		t.Fatal("coordination help must point to the separate workflow reference")
	}
	after, err := r.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("help changed coordination state")
	}
	current, err := json.Marshal(r.config.Registry.GetSchemas())
	if err != nil || string(current) != string(schemas) {
		t.Fatalf("help changed tool definitions: %v", err)
	}

	// Both guides also work with no runtime or session attached at all.
	registry := tools.NewToolRegistry(nil)
	defer registry.Close()
	registerHelpTools(registry)
	for name, guide := range map[string]string{"swarm_help": coordinationGuide, "workflow_help": workflowGuide} {
		helper, _ := registry.Get(name)
		if result, err := helper.Execute(context.Background(), nil); err != nil || result != guide {
			t.Fatalf("%s depends on runtime state: %q, %v", name, result, err)
		}
	}
}
