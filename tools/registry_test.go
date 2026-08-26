package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/alexschlessinger/pollytool/schema"
)

type testTool struct {
	name   string
	source string
}

func TestRegistryReturnsToolsAndSchemasInLexicalOrder(t *testing.T) {
	first := NewToolRegistry([]Tool{
		&testTool{name: "zebra"},
		&testTool{name: "alpha"},
		&testTool{name: "middle"},
	})
	second := NewToolRegistry([]Tool{
		&testTool{name: "middle"},
		&testTool{name: "zebra"},
		&testTool{name: "alpha"},
	})

	names := func(registry *ToolRegistry) []string {
		all := registry.All()
		out := make([]string, len(all))
		for i, tool := range all {
			out[i] = tool.GetName()
		}
		return out
	}
	want := []string{"alpha", "middle", "zebra"}
	if got := names(first); !reflect.DeepEqual(got, want) {
		t.Fatalf("All() names = %#v, want %#v", got, want)
	}
	if got := names(second); !reflect.DeepEqual(got, want) {
		t.Fatalf("All() names after different registration order = %#v, want %#v", got, want)
	}

	firstJSON, err := json.Marshal(first.GetSchemas())
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second.GetSchemas())
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("schema bytes differ by registration order:\n%s\n%s", firstJSON, secondJSON)
	}
}

func (t *testTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(t.name, "Test tool", nil)
}

func (t *testTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	return "test result", nil
}

func (t *testTool) GetName() string {
	return t.name
}

func (t *testTool) GetType() string {
	return "test"
}

func (t *testTool) GetSource() string {
	if t.source != "" {
		return t.source
	}
	return "test-source"
}

func TestRegistryRemove(t *testing.T) {
	registry := NewToolRegistry([]Tool{})
	tool := &testTool{name: "removable"}
	registry.Register(tool)

	_, exists := registry.Get("removable")
	if !exists {
		t.Error("Expected tool to exist before removal")
	}

	registry.Remove("removable")

	_, exists = registry.Get("removable")
	if exists {
		t.Error("Expected tool to not exist after removal")
	}
}
