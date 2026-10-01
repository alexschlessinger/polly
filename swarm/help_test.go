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
	t.Parallel()
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
	for name, guide := range map[string]string{"swarm_help": coordinationGuide, "workflow_help": workflowReference} {
		t.Run(name, func(t *testing.T) {
			helper, exists, allowed := r.config.Registry.GetIfAllowed(name)
			if !exists || !allowed {
				t.Fatalf("parent has no %s tool", name)
			}
			// The only argument is workflow_help's optional example selector;
			// a bare call always returns the reference.
			optional := 0
			if name == "workflow_help" {
				optional = 1
			}
			if s := helper.GetSchema(); len(s.Properties()) != optional || len(s.Required()) != 0 {
				t.Fatalf("help takes required arguments: %+v", s)
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
	registry := tools.NewToolRegistry(nil, tools.WithNativeTools())
	defer registry.Close()
	registerHelpTools(registry)
	for name, guide := range map[string]string{"swarm_help": coordinationGuide, "workflow_help": workflowReference} {
		helper, _ := registry.Get(name)
		if result, err := helper.Execute(context.Background(), nil); err != nil || result != guide {
			t.Fatalf("%s depends on runtime state: %q, %v", name, result, err)
		}
	}
}

func TestMemberHelpIsStatelessAndKeepsDefinitions(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		t.Error("help called the model")
		return answer("unexpected call")
	}), 1, 1)
	registry := tools.NewToolRegistry(nil)
	defer registry.Close()
	r.registerMemberTools(registry, "child")
	helper, exists, allowed := registry.GetIfAllowed("swarm_help")
	if !exists || !allowed {
		t.Fatal("member has no swarm_help tool")
	}
	if s := helper.GetSchema(); len(s.Properties()) != 0 || len(s.Required()) != 0 {
		t.Fatalf("member help takes arguments: %+v", s)
	}
	for _, name := range childCoordinationTools {
		if !strings.Contains(memberCoordinationGuide, name) {
			t.Fatalf("member help omits %s", name)
		}
	}
	for _, names := range [][]string{parentCoordinationTools, removedCoordinationTools} {
		for _, name := range names {
			if _, exists := registry.Get(name); !exists && strings.Contains(memberCoordinationGuide, name) {
				t.Fatalf("member help suggests unavailable tool %s", name)
			}
		}
	}
	before, err := r.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := json.Marshal(registry.GetSchemas())
	if err != nil {
		t.Fatal(err)
	}
	// Arguments cannot select the parent's guide; the role is bound at registration.
	for _, args := range []map[string]any{nil, {"actor": r.ID, "role": "parent"}, nil} {
		result, err := helper.Execute(context.Background(), args)
		if err != nil || result != memberCoordinationGuide || result == "" || json.Valid([]byte(result)) {
			t.Fatalf("member help did not return its plain-text guide: %q, %v", result, err)
		}
	}
	after, err := r.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("member help changed coordination state")
	}
	current, err := json.Marshal(registry.GetSchemas())
	if err != nil || string(current) != string(schemas) {
		t.Fatalf("member help changed tool definitions: %v", err)
	}

	// The embedded member guide also works without a runtime or session.
	standalone := tools.NewToolRegistry(nil)
	defer standalone.Close()
	registerSwarmHelp(standalone, memberCoordinationGuide)
	helper, _ = standalone.Get("swarm_help")
	if result, err := helper.Execute(context.Background(), nil); err != nil || result != memberCoordinationGuide {
		t.Fatalf("member help depends on runtime state: %q, %v", result, err)
	}
}
