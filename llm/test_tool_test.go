package llm

import (
	"context"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

// testTool is a minimal tools.Tool for request-shape tests.
type testTool struct {
	name   string
	schema *schema.ToolSchema
}

func (t testTool) GetSchema() *schema.ToolSchema { return t.schema }
func (t testTool) Execute(context.Context, map[string]any) (string, error) {
	return "", nil
}
func (t testTool) GetName() string   { return t.name }
func (t testTool) GetType() string   { return "native" }
func (t testTool) GetSource() string { return "test" }

var _ tools.Tool = testTool{}
