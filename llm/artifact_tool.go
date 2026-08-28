package llm

import (
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
	artifactReadDefaultLines = tools.PageDefaultLines
	artifactReadMaxLines     = tools.PageMaxLines
	artifactReadMaxBytes     = tools.PageMaxBytes
	artifactScanMaxLine      = tools.PageScanMaxLine

	artifactListPageEntries = 50
	// artifactListMaxBytes plus the continuation footer must stay under
	// toolInlineTokenLimit*4 bytes so a catalog page is never externalized to
	// an artifact at birth; the 50-entry page keeps worst-case output well
	// below this cap regardless.
	artifactListMaxBytes = 36 << 10
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
		"Read a bounded section of a text artifact, search it literally, page raw bytes, or attach a stored image. IDs must come from this conversation.",
		schema.Params{
			"id":          schema.S("Artifact ID from a tool-result receipt or image reference"),
			"offset":      schema.Int("1-based starting line (default 1)"),
			"limit":       schema.Int("Maximum lines or matches (default 200, maximum 500)"),
			"query":       schema.S("Optional case-sensitive literal search"),
			"byte_offset": schema.Int("0-based byte position; returns a raw byte window instead of numbered lines (for artifacts with very long lines)"),
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

	if _, hasByteOffset := raw["byte_offset"]; hasByteOffset {
		for _, key := range []string{"offset", "limit", "query"} {
			if _, conflict := raw[key]; conflict {
				return tools.ToolOutput{}, fmt.Errorf("byte_offset cannot be combined with offset, limit, or query")
			}
		}
		byteOffset := int64(args.Int("byte_offset", -1))
		if byteOffset < 0 {
			return tools.ToolOutput{}, fmt.Errorf("byte_offset must be at least 0")
		}
		if byteOffset >= ref.Bytes {
			return tools.ToolOutput{Text: fmt.Sprintf("Artifact has no content at or after byte %d.", byteOffset)}, nil
		}
		r, err := t.store.Open(ctx, id)
		if err != nil {
			return tools.ToolOutput{}, err
		}
		text, readErr := byteWindowArtifactText(ctx, r, ref, byteOffset)
		closeErr := r.Close()
		if readErr != nil || closeErr != nil {
			return tools.ToolOutput{}, errors.Join(readErr, closeErr)
		}
		return tools.ToolOutput{Text: text}, nil
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

// boundedArtifactText renders numbered artifact lines via the shared pager;
// see tools.PageLines for the paging and truncation contract.
func boundedArtifactText(ctx context.Context, r io.Reader, offset, limit int, query string) (string, error) {
	return tools.PageLines(ctx, r, "artifact", offset, limit, query)
}

// byteWindowArtifactText returns a raw byte window of a text artifact; paging
// with the reported next byte_offset recovers any content exactly, regardless
// of line structure.
func byteWindowArtifactText(ctx context.Context, r io.Reader, ref artifacts.Ref, byteOffset int64) (string, error) {
	text, err := tools.PageByteWindow(ctx, r, "artifact", ref.ID, ref.Bytes, byteOffset)
	if errors.Is(err, tools.ErrPageSizeMismatch) {
		return "", fmt.Errorf("stored size does not match transcript reference")
	}
	return text, err
}

type listArtifactsTool struct {
	tools.NativeTool
	list func() []artifacts.Ref
}

func (t *listArtifactsTool) GetName() string { return "list_artifacts" }

func (t *listArtifactsTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		"list_artifacts",
		"List the artifacts referenced by this conversation, in the order first referenced, including ones from context that is no longer shown. Read one with read_artifact.",
		schema.Params{
			"offset": schema.Int("1-based starting position in the list (default 1)"),
		},
	)
}

func (t *listArtifactsTool) Execute(ctx context.Context, raw map[string]any) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	offset := tools.Args(raw).Int("offset", 1)
	if offset < 1 {
		return "", fmt.Errorf("offset must be at least 1")
	}
	refs := t.list()
	if len(refs) == 0 {
		return "No artifacts are referenced by this conversation.", nil
	}
	if offset > len(refs) {
		return fmt.Sprintf("Artifact list has no entries at or after offset %d (total %d).", offset, len(refs)), nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%d artifact(s) referenced by this conversation, in the order first referenced:\n", len(refs))
	next := 0
	for i := offset - 1; i < len(refs); i++ {
		entry := artifactListEntry(i+1, refs[i])
		if i-(offset-1) >= artifactListPageEntries || out.Len()+len(entry) > artifactListMaxBytes {
			next = i + 1
			break
		}
		out.WriteString(entry)
	}
	if next > 0 {
		fmt.Fprintf(&out, "[more entries; continue with offset=%d]", next)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

func artifactListEntry(position int, ref artifacts.Ref) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d. %s; %s; %d bytes", position, ref.ID, ref.Kind, ref.Bytes)
	if ref.Kind == artifacts.KindText && ref.Lines > 0 {
		fmt.Fprintf(&b, "; %d lines", ref.Lines)
	}
	if name := sanitizeArtifactDescriptor(ref.Name); name != "" {
		b.WriteString("; " + name)
	}
	if ref.Kind == artifacts.KindImage {
		if token := sanitizeArtifactDescriptor(ref.ImageToken); token != "" {
			b.WriteString("; reference " + token)
		}
	}
	b.WriteByte('\n')
	return b.String()
}

func capArtifactReadText(text string) string {
	return tools.CapPageText(text)
}
