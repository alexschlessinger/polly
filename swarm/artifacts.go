package swarm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

type publishedArtifactTool struct {
	tools.NativeTool
	session sessions.CoordinationSession
}

func (t *publishedArtifactTool) GetName() string { return "swarm_read_artifact" }
func (t *publishedArtifactTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(t.GetName(), "Read an artifact explicitly published in this swarm. Private artifacts cannot be read by guessing their IDs.", schema.Params{"id": schema.S("Published artifact ID")}, "id")
}
func (t *publishedArtifactTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	out, err := t.ExecuteOutput(ctx, args)
	return out.Text, err
}
func (t *publishedArtifactTool) ExecuteOutput(ctx context.Context, args map[string]any) (tools.ToolOutput, error) {
	reader, err := t.session.OpenPublishedArtifact(ctx, tools.Args(args).String("id"))
	if err != nil {
		return tools.ToolOutput{}, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, (32<<20)+1))
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if len(data) > 32<<20 {
		return tools.ToolOutput{}, fmt.Errorf("published artifact exceeds 32 MiB read limit")
	}
	mime := http.DetectContentType(data)
	if utf8.Valid(data) && !strings.ContainsRune(string(data), 0) && strings.HasPrefix(mime, "text/") {
		return tools.ToolOutput{Text: string(data)}, nil
	}
	return tools.ToolOutput{Text: "Published artifact " + tools.Args(args).String("id"), Media: []tools.ToolMedia{{Data: data, MIMEType: mime}}}, nil
}
