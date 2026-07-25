package tools

import (
	"context"
	"slices"
	"testing"

	"github.com/alexschlessinger/pollytool/schema"
)

type testTool struct {
	name   string
	source string
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

// Every provider's prompt cache keys on an exact request prefix, and the agent
// loop rebuilds the tool list on every iteration. Ranging over the map here used
// to ship a differently ordered tool block each turn, so the prefix never
// repeated and the cache could not hit once in a conversation.
func TestToolOrderIsStableAcrossCalls(t *testing.T) {
	r := NewToolRegistry(nil)
	for _, name := range []string{"search_tree", "read_file", "git_log", "finish", "alpha"} {
		r.Register(&Func{Name: name, Desc: name})
	}
	want := []string{"alpha", "finish", "git_log", "read_file", "search_tree"}
	for i := range 20 {
		var got []string
		for _, tool := range r.All() {
			got = append(got, tool.GetName())
		}
		if !slices.Equal(got, want) {
			t.Fatalf("call %d: tool order %v, want %v", i, got, want)
		}
		var schemas []string
		for _, s := range r.GetSchemas() {
			schemas = append(schemas, s.Title())
		}
		if !slices.Equal(schemas, want) {
			t.Fatalf("call %d: schema order %v, want %v", i, schemas, want)
		}
	}
}
