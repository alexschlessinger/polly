package tools

import (
	"context"

	"github.com/alexschlessinger/pollytool/schema"
)

// Func is a declarative tool definition. It implements Tool.
type Func struct {
	Name     string
	Desc     string
	Params   schema.Params
	Required []string
	Source   string // defaults to "builtin"
	Run      func(ctx context.Context, args Args) (string, error)
}

func (f *Func) GetName() string { return f.Name }
func (f *Func) GetType() string { return "native" }

func (f *Func) GetSource() string {
	if f.Source != "" {
		return f.Source
	}
	return "builtin"
}

func (f *Func) GetSchema() *schema.ToolSchema {
	return schema.Tool(f.Name, f.Desc, f.Params, f.Required...)
}

func (f *Func) Execute(ctx context.Context, args map[string]any) (string, error) {
	return f.Run(ctx, Args(args))
}
