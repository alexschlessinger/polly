package llm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

const (
	artifactReadDefaultLines = 200
	artifactReadMaxLines     = 500
	artifactReadMaxBytes     = 40 << 10
	artifactScanMaxLine      = 1 << 20
)

type readArtifactTool struct {
	tools.NativeTool
	store  artifacts.Store
	lookup func(string) (artifacts.Ref, bool)
}

func (t *readArtifactTool) GetName() string { return "read_artifact" }

func (t *readArtifactTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		"read_artifact",
		"Read a bounded section of a text artifact, search it literally, or attach a stored image. IDs must come from this conversation.",
		schema.Params{
			"id":     schema.S("Artifact ID from a tool-result receipt or image reference"),
			"offset": schema.Int("1-based starting line (default 1)"),
			"limit":  schema.Int("Maximum lines or matches (default 200, maximum 500)"),
			"query":  schema.S("Optional case-sensitive literal search"),
		},
		"id",
	)
}

func (t *readArtifactTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	output, err := t.ExecuteOutput(ctx, args)
	return output.Text, err
}

func (t *readArtifactTool) ExecuteOutput(ctx context.Context, raw map[string]any) (tools.ToolOutput, error) {
	args := tools.Args(raw)
	id := strings.TrimSpace(args.String("id"))
	if !artifacts.ValidID(id) {
		return tools.ToolOutput{}, fmt.Errorf("invalid artifact ID")
	}
	ref, ok := t.lookup(id)
	if !ok {
		return tools.ToolOutput{}, fmt.Errorf("artifact %q is not referenced by this conversation", id)
	}
	if ref.Kind == artifacts.KindImage {
		data, err := readArtifactBytes(ctx, t.store, ref.ID, ref.Bytes)
		if err != nil {
			return tools.ToolOutput{}, err
		}
		return tools.ToolOutput{
			Text:  capArtifactReadText(fmt.Sprintf("Attached image artifact %s (%s, reference %s, %d bytes).", ref.ID, ref.Name, ref.ImageToken, ref.Bytes)),
			Media: []tools.ToolMedia{{Data: data, MIMEType: ref.MIMEType, Name: ref.Name, Reference: ref.ImageToken}},
		}, nil
	}
	if ref.Kind != artifacts.KindText {
		return tools.ToolOutput{Text: capArtifactReadText(fmt.Sprintf("Artifact %s is %s (%s, %d bytes); binary payloads are not inserted into model context.", ref.ID, ref.Kind, ref.MIMEType, ref.Bytes))}, nil
	}

	offset := args.Int("offset", 1)
	if offset < 1 {
		return tools.ToolOutput{}, fmt.Errorf("offset must be at least 1")
	}
	limit := args.Int("limit", artifactReadDefaultLines)
	if limit < 1 || limit > artifactReadMaxLines {
		return tools.ToolOutput{}, fmt.Errorf("limit must be between 1 and %d", artifactReadMaxLines)
	}
	r, err := t.store.Open(ctx, id)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	text, readErr := boundedArtifactText(ctx, r, offset, limit, args.String("query"))
	closeErr := r.Close()
	if readErr != nil || closeErr != nil {
		return tools.ToolOutput{}, errors.Join(readErr, closeErr)
	}
	return tools.ToolOutput{Text: capArtifactReadText(text)}, nil
}

func boundedArtifactText(ctx context.Context, r io.Reader, offset, limit int, query string) (string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), artifactScanMaxLine)
	var out bytes.Buffer
	lineNumber := 0
	emitted := 0
	truncated := false
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		lineNumber++
		if lineNumber < offset {
			continue
		}
		line := scanner.Text()
		if query != "" && !strings.Contains(line, query) {
			continue
		}
		entry := fmt.Sprintf("%d: %s\n", lineNumber, line)
		if out.Len()+len(entry) > artifactReadMaxBytes {
			remaining := artifactReadMaxBytes - out.Len()
			if remaining > 0 {
				out.WriteString(entry[:min(remaining, len(entry))])
			}
			truncated = true
			break
		}
		out.WriteString(entry)
		emitted++
		if emitted >= limit {
			truncated = scanner.Scan()
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan artifact: %w", err)
	}
	if out.Len() == 0 {
		if query != "" {
			return capArtifactReadText(fmt.Sprintf("No matches for %q at or after line %d.", query, offset)), nil
		}
		return fmt.Sprintf("Artifact has no content at or after line %d.", offset), nil
	}
	if truncated {
		note := "\n[bounded artifact output truncated]"
		if out.Len()+len(note) <= artifactReadMaxBytes {
			out.WriteString(note)
		}
	}
	return capArtifactReadText(out.String()), nil
}

func capArtifactReadText(text string) string {
	if len(text) > artifactReadMaxBytes {
		text = text[:safeUTF8Boundary(text, artifactReadMaxBytes)]
	}
	// Artifact text may contain arbitrary tool bytes. Keep the provider-facing
	// response valid UTF-8 without increasing its bounded byte size.
	return string(bytes.ToValidUTF8([]byte(text), []byte("?")))
}
