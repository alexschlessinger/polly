package tools

import (
	"context"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonschema"
)

type testTool struct {
	name string
}

func (t *testTool) GetSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Title:       t.name,
		Description: "Test tool",
		Type:        "object",
	}
}

func (t *testTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	return "test result", nil
}

func TestNewToolRegistry(t *testing.T) {
	tools := []Tool{
		&testTool{name: "tool1"},
		&testTool{name: "tool2"},
	}

	registry := NewToolRegistry(tools)

	if registry == nil {
		t.Fatal("Expected registry to be created")
	}

	if len(registry.tools) != 2 {
		t.Errorf("Expected 2 tools, got %d", len(registry.tools))
	}
}

func TestRegistryGet(t *testing.T) {
	registry := NewToolRegistry([]Tool{})
	tool := &testTool{name: "test-tool"}
	registry.Register(tool)

	retrieved, exists := registry.Get("test-tool")
	if !exists {
		t.Error("Expected tool to exist")
	}

	if retrieved != tool {
		t.Error("Expected to get the same tool instance")
	}

	_, exists = registry.Get("non-existent")
	if exists {
		t.Error("Expected non-existent tool to not exist")
	}
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

func TestRegistryAll(t *testing.T) {
	tool1 := &testTool{name: "tool1"}
	tool2 := &testTool{name: "tool2"}

	registry := NewToolRegistry([]Tool{tool1, tool2})
	allTools := registry.All()

	if len(allTools) != 2 {
		t.Errorf("Expected 2 tools, got %d", len(allTools))
	}
}

func TestRegistryGetSchemas(t *testing.T) {
	tool1 := &testTool{name: "tool1"}
	tool2 := &testTool{name: "tool2"}

	registry := NewToolRegistry([]Tool{tool1, tool2})
	schemas := registry.GetSchemas()

	if len(schemas) != 2 {
		t.Errorf("Expected 2 schemas, got %d", len(schemas))
	}

	for _, schema := range schemas {
		if schema == nil {
			t.Error("Expected non-nil schema")
			continue
		}
		if schema.Title != "tool1" && schema.Title != "tool2" {
			t.Errorf("Unexpected schema title: %s", schema.Title)
		}
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	registry := NewToolRegistry([]Tool{})

	// Number of goroutines for each operation
	numGoroutines := 100
	done := make(chan bool)

	// Concurrent writes (Register)
	for i := range numGoroutines {
		go func(id int) {
			tool := &testTool{name: fmt.Sprintf("tool%d", id)}
			registry.Register(tool)
			done <- true
		}(i)
	}

	// Concurrent reads (Get)
	for i := range numGoroutines {
		go func(id int) {
			registry.Get(fmt.Sprintf("tool%d", id))
			done <- true
		}(i)
	}

	// Concurrent All() calls
	for range numGoroutines {
		go func() {
			registry.All()
			done <- true
		}()
	}

	// Concurrent GetSchemas() calls
	for range numGoroutines {
		go func() {
			registry.GetSchemas()
			done <- true
		}()
	}

	// Concurrent removes
	for i := range numGoroutines / 2 {
		go func(id int) {
			registry.Remove(fmt.Sprintf("tool%d", id))
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for range numGoroutines*4 + numGoroutines/2 {
		<-done
	}

	// Verify registry is still functional
	tools := registry.All()
	// Just verify we can still call All() without panic
	_ = tools

}

func TestRegistryConcurrentReadWrite(t *testing.T) {
	registry := NewToolRegistry([]Tool{
		&testTool{name: "initial"},
	})

	done := make(chan bool)
	errors := make(chan error, 1000)

	// Readers
	for range 50 {
		go func() {
			for j := range 100 {
				_, exists := registry.Get("initial")
				if !exists && j == 0 {
					errors <- fmt.Errorf("initial tool should exist")
				}
				registry.All()
				registry.GetSchemas()
			}
			done <- true
		}()
	}

	// Writers
	for i := range 10 {
		go func(id int) {
			toolName := fmt.Sprintf("concurrent%d", id)
			tool := &testTool{name: toolName}

			for range 50 {
				registry.Register(tool)
				registry.Remove(toolName)
				registry.Register(tool)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for range 60 {
		<-done
	}

	close(errors)

	// Check for any errors
	for err := range errors {
		t.Error(err)
	}
}
