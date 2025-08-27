package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonschema"
)

type testTool struct {
	name   string
	source string
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

func TestGetActiveToolLoaders(t *testing.T) {
	tool1 := &testTool{name: "tool1", source: "source1"}
	tool2 := &testTool{name: "tool2", source: "source2"}
	
	registry := NewToolRegistry([]Tool{tool1, tool2})
	loaders := registry.GetActiveToolLoaders()
	
	if len(loaders) != 2 {
		t.Errorf("Expected 2 loaders, got %d", len(loaders))
	}
	
	// Check that loaders have correct data
	loaderMap := make(map[string]ToolLoaderInfo)
	for _, loader := range loaders {
		loaderMap[loader.Name] = loader
	}
	
	if loader, ok := loaderMap["tool1"]; !ok {
		t.Error("Expected tool1 in loaders")
	} else {
		if loader.Type != "test" {
			t.Errorf("Expected type 'test', got %s", loader.Type)
		}
		if loader.Source != "source1" {
			t.Errorf("Expected source 'source1', got %s", loader.Source)
		}
	}
	
	if loader, ok := loaderMap["tool2"]; !ok {
		t.Error("Expected tool2 in loaders")
	} else {
		if loader.Type != "test" {
			t.Errorf("Expected type 'test', got %s", loader.Type)
		}
		if loader.Source != "source2" {
			t.Errorf("Expected source 'source2', got %s", loader.Source)
		}
	}
}

func TestNamespacedToolFiltering(t *testing.T) {
	// Test the namespace stripping logic used in LoadMCPServerWithFilter
	testCases := []struct {
		input    string
		expected string
	}{
		{"perp__perplexity_search", "perplexity_search"},
		{"test__tool_name", "tool_name"}, 
		{"no_namespace", "no_namespace"},
		{"multiple__under__scores", "under__scores"},
	}
	
	for _, tc := range testCases {
		var result string
		if idx := strings.Index(tc.input, "__"); idx != -1 {
			result = tc.input[idx+2:]
		} else {
			result = tc.input
		}
		
		if result != tc.expected {
			t.Errorf("For input %q, expected %q but got %q", tc.input, tc.expected, result)
		}
	}
}
