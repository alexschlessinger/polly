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

func TestSetToolLockedClosesOrphanedClient(t *testing.T) {
	registry := NewToolRegistry(nil)
	first := &MCPClient{}
	second := &MCPClient{}
	registry.mu.Lock()
	registry.setToolLocked("srv__a", &testTool{name: "srv__a"}, first)
	registry.setToolLocked("srv__b", &testTool{name: "srv__b"}, first)
	// Replacing one of two tools keeps the shared client alive.
	registry.setToolLocked("srv__a", &testTool{name: "srv__a"}, second)
	if first.Closed() {
		t.Fatal("client closed while another tool still used it")
	}
	// Replacing the last tool that used it closes it.
	registry.setToolLocked("srv__b", &testTool{name: "srv__b"}, nil)
	registry.mu.Unlock()
	if !first.Closed() {
		t.Fatal("orphaned client was not closed")
	}
	if second.Closed() {
		t.Fatal("live client was closed")
	}
	if _, ok := registry.toolClients["srv__b"]; ok {
		t.Fatal("non-MCP replacement left a client mapping behind")
	}
}

func TestSetToolLockedKeepsPendingClientAlive(t *testing.T) {
	registry := NewToolRegistry(nil)
	client := &MCPClient{}
	registry.mu.Lock()
	registry.setToolLocked("srv__a", &testTool{name: "srv__a"}, client)
	registry.pendingToolClients["srv__c"] = client
	registry.setToolLocked("srv__a", &testTool{name: "srv__a"}, nil)
	registry.mu.Unlock()
	if client.Closed() {
		t.Fatal("client referenced by a pending tool was closed")
	}
}

func TestDropServerToolsLockedClosesPreviousClient(t *testing.T) {
	registry := NewToolRegistry(nil)
	old := &MCPClient{}
	registry.mu.Lock()
	registry.setToolLocked("srv__a", &testTool{name: "srv__a"}, old)
	registry.setToolLocked("srv__b", &testTool{name: "srv__b"}, old)
	registry.serverTools["/cfg.json#srv"] = []string{"srv__a", "srv__b"}
	registry.dropServerToolsLocked("/cfg.json#srv")
	registry.mu.Unlock()
	if !old.Closed() {
		t.Fatal("previous server client was not closed")
	}
	if len(registry.tools) != 0 || len(registry.toolClients) != 0 || len(registry.serverTools) != 0 {
		t.Fatalf("stale registration left behind: tools=%d clients=%d servers=%d", len(registry.tools), len(registry.toolClients), len(registry.serverTools))
	}
}

func TestMCPClientCloseIsIdempotent(t *testing.T) {
	client := &MCPClient{}
	if client.Closed() {
		t.Fatal("new client reports closed")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if !client.Closed() {
		t.Fatal("client does not report closed")
	}
}
