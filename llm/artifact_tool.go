package llm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

const (
	artifactReadDefaultLines = 200
	artifactReadMaxLines     = 500
	artifactReadMaxBytes     = 40 << 10
	artifactScanMaxLine      = 1 << 20

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
		defer r.Close()
		text, err := byteWindowArtifactText(ctx, r, ref, byteOffset)
		if err != nil {
			return tools.ToolOutput{}, err
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

// boundedArtifactText renders numbered lines and cannot fail on content shape:
// physical lines longer than artifactScanMaxLine become bounded placeholders
// that point at their byte_offset instead of aborting the scan, and mid-line
// response-cap truncation reports the exact continuation byte_offset.
func boundedArtifactText(ctx context.Context, r io.Reader, offset, limit int, query string) (string, error) {
	// Reserved room guarantees a truncation note always fits under the cap.
	const noteReserve = 64
	br := bufio.NewReaderSize(r, 64<<10)
	var out bytes.Buffer
	lineNumber := 0
	emitted := 0
	var lineStart int64
	truncated := false
	continueAt := int64(-1)
	for {
		line, err := readPhysicalLine(ctx, br, artifactScanMaxLine, query)
		if err != nil {
			return "", fmt.Errorf("scan artifact: %w", err)
		}
		if !line.readAny {
			break
		}
		lineNumber++
		start := lineStart
		lineStart += line.rawLen
		if line.sawNewline {
			lineStart++
		}
		if lineNumber < offset || (query != "" && !line.matched) {
			if !line.sawNewline {
				break
			}
			continue
		}
		overlong := int64(len(line.held)) < line.rawLen
		var entry string
		partialAllowed := false
		switch {
		case overlong && query != "":
			entry = fmt.Sprintf("%d: [query matches inside %d-byte line; read with byte_offset=%d]\n", lineNumber, line.rawLen, start)
		case overlong:
			entry = fmt.Sprintf("%d: [line is %d bytes; exceeds inline limit; read with byte_offset=%d]\n", lineNumber, line.rawLen, start)
		default:
			display := line.held
			if len(display) > 0 && display[len(display)-1] == '\r' {
				display = display[:len(display)-1]
			}
			entry = fmt.Sprintf("%d: %s\n", lineNumber, display)
			partialAllowed = true
		}
		if out.Len()+len(entry) > artifactReadMaxBytes-noteReserve {
			prefix := len(fmt.Sprintf("%d: ", lineNumber))
			continueAt = start
			if room := artifactReadMaxBytes - noteReserve - out.Len(); partialAllowed && room > prefix {
				// Back a cut that splits a rune up to its boundary so the
				// character renders on the continuation page instead of as a
				// replacement on both; binary content keeps the raw cut so
				// paging still advances.
				cut := safeUTF8Boundary(entry, room)
				if room-cut >= utf8.UTFMax {
					cut = room
				}
				if cut > prefix {
					out.WriteString(entry[:cut])
					continueAt = start + int64(cut-prefix)
				}
			}
			truncated = true
			break
		}
		out.WriteString(entry)
		emitted++
		if emitted >= limit {
			if line.sawNewline {
				if _, peekErr := br.Peek(1); peekErr == nil {
					truncated = true
				}
			}
			break
		}
		if !line.sawNewline {
			break
		}
	}
	if out.Len() == 0 {
		if query != "" {
			return capArtifactReadText(fmt.Sprintf("No matches for %q at or after line %d.", query, offset)), nil
		}
		return fmt.Sprintf("Artifact has no content at or after line %d.", offset), nil
	}
	if truncated {
		if continueAt >= 0 {
			fmt.Fprintf(&out, "\n[output truncated; continue with byte_offset=%d]", continueAt)
		} else {
			out.WriteString("\n[bounded artifact output truncated]")
		}
	}
	return capArtifactReadText(out.String()), nil
}

type physicalLine struct {
	held       []byte
	rawLen     int64
	sawNewline bool
	matched    bool
	readAny    bool
}

// readPhysicalLine reads one newline-terminated physical line, holding at most
// keep bytes and stream-discarding the rest while counting exact byte lengths.
// The query is matched against the complete line via a carry search, so a hit
// past the held window or spanning chunk boundaries is still found.
func readPhysicalLine(ctx context.Context, br *bufio.Reader, keep int, query string) (physicalLine, error) {
	var line physicalLine
	var carry []byte
	for {
		if err := ctx.Err(); err != nil {
			return physicalLine{}, err
		}
		chunk, readErr := br.ReadSlice('\n')
		if len(chunk) > 0 {
			line.readAny = true
			segment := chunk
			if segment[len(segment)-1] == '\n' {
				line.sawNewline = true
				segment = segment[:len(segment)-1]
			}
			line.rawLen += int64(len(segment))
			if len(line.held) < keep {
				take := min(keep-len(line.held), len(segment))
				line.held = append(line.held, segment[:take]...)
			}
			if query != "" && !line.matched && len(segment) > 0 {
				probe := segment
				if len(carry) > 0 {
					probe = append(append([]byte(nil), carry...), segment...)
				}
				line.matched = bytes.Contains(probe, []byte(query))
				if overlap := len(query) - 1; !line.matched && overlap > 0 {
					if len(probe) > overlap {
						probe = probe[len(probe)-overlap:]
					}
					carry = append(carry[:0], probe...)
				}
			}
		}
		if line.sawNewline || readErr == io.EOF {
			return line, nil
		}
		if readErr != nil && readErr != bufio.ErrBufferFull {
			return physicalLine{}, readErr
		}
	}
}

// byteWindowArtifactText returns a raw byte window of a text artifact; paging
// with the reported next byte_offset recovers any content exactly, regardless
// of line structure.
func byteWindowArtifactText(ctx context.Context, r io.Reader, ref artifacts.Ref, byteOffset int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := io.CopyN(io.Discard, r, byteOffset); err != nil {
		if err == io.EOF {
			return "", fmt.Errorf("stored size does not match transcript reference")
		}
		return "", err
	}
	window := int64(artifactReadMaxBytes - 256)
	data, err := io.ReadAll(io.LimitReader(r, window))
	if err != nil {
		return "", err
	}
	end := byteOffset + int64(len(data))
	if end < ref.Bytes && int64(len(data)) < window {
		return "", fmt.Errorf("stored size does not match transcript reference")
	}
	if end < ref.Bytes && len(data) > 0 {
		// Trim a rune split by the window edge so text pages cleanly; binary
		// content is left whole so paging always advances.
		if r, size := utf8.DecodeLastRune(data); r == utf8.RuneError && size == 1 {
			cut := len(data)
			for cut > 0 && len(data)-cut < utf8.UTFMax && (data[cut-1]&0xc0) == 0x80 {
				cut--
			}
			if cut > 0 && len(data)-cut < utf8.UTFMax && (data[cut-1]&0xc0) == 0xc0 {
				cut--
			}
			if len(data)-cut < utf8.UTFMax {
				data = data[:cut]
				end = byteOffset + int64(len(data))
			}
		}
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "[artifact %s; bytes %d-%d of %d; raw window]\n", ref.ID, byteOffset, end, ref.Bytes)
	out.Write(data)
	if end < ref.Bytes {
		fmt.Fprintf(&out, "\n[artifact continues; next byte_offset=%d]", end)
	}
	return capArtifactReadText(out.String()), nil
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
	if len(text) > artifactReadMaxBytes {
		text = text[:safeUTF8Boundary(text, artifactReadMaxBytes)]
	}
	// Artifact text may contain arbitrary tool bytes. Keep the provider-facing
	// response valid UTF-8 without increasing its bounded byte size.
	return string(bytes.ToValidUTF8([]byte(text), []byte("?")))
}
