package tools

import (
	"context"

	"github.com/alexschlessinger/pollytool/schema"
)

// Func is a declarative tool definition. It implements Tool.
type Func struct {
	LongRunning bool
	Exclusive   bool
	Name        string
	Desc        string
	Params      schema.Params
	Required    []string
	Strict      bool
	Source      string // defaults to "builtin"
	Run         func(ctx context.Context, args Args) (string, error)
}

func (f *Func) Untimed() bool        { return f.LongRunning }
func (f *Func) ExclusiveBatch() bool { return f.Exclusive }

func (f *Func) GetName() string { return f.Name }
func (f *Func) GetType() string { return "native" }

func (f *Func) GetSource() string {
	if f.Source != "" {
		return f.Source
	}
	return "builtin"
}

func (f *Func) GetSchema() *schema.ToolSchema {
	toolSchema := schema.Tool(f.Name, f.Desc, f.Params, f.Required...)
	toolSchema.Strict = f.Strict
	return toolSchema
}

func (f *Func) Execute(ctx context.Context, args map[string]any) (string, error) {
	return f.Run(ctx, Args(args))
}
