package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/artifacts"
)

func TestReadArtifactToolBoundsAndNumbersText(t *testing.T) {
	store := newTestArtifactStore()
	var content strings.Builder
	for i := 1; i <= 600; i++ {
		fmt.Fprintf(&content, "line %03d\n", i)
	}
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Name: "lines.txt", Data: []byte(content.String())})
	tool := testReadArtifactTool(store, ref)

	output, err := tool.ExecuteOutput(context.Background(), map[string]any{"id": ref.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.Text, "1: line 001\n") || !strings.Contains(output.Text, "200: line 200") || strings.Contains(output.Text, "201: line 201") {
		t.Fatalf("default bounded read = %q", output.Text)
	}
	if len(output.Text) > artifactReadMaxBytes {
		t.Fatalf("default output has %d bytes, max %d", len(output.Text), artifactReadMaxBytes)
	}

	output, err = tool.ExecuteOutput(context.Background(), map[string]any{"id": ref.ID, "offset": 10, "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.Text, "10: line 010") || !strings.Contains(output.Text, "11: line 011") || strings.Contains(output.Text, "12: line 012") {
		t.Fatalf("offset read = %q", output.Text)
	}
}

func TestReadArtifactToolLiteralSearchIsCaseSensitive(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, Data: []byte("Alpha\nalpha\nAlpha beta\n")})
	tool := testReadArtifactTool(store, ref)

	output, err := tool.ExecuteOutput(context.Background(), map[string]any{"id": ref.ID, "offset": 2, "query": "Alpha", "limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.Text, "2: alpha") || output.Text != "3: Alpha beta\n" {
		t.Fatalf("literal search = %q", output.Text)
	}

	output, err = tool.ExecuteOutput(context.Background(), map[string]any{"id": ref.ID, "query": "missing"})
	if err != nil || !strings.Contains(output.Text, `No matches for "missing"`) {
		t.Fatalf("no-match output = %q, %v", output.Text, err)
	}
}

func TestReadArtifactToolEnforcesLineAndByteCaps(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, Data: []byte(strings.Repeat("x", 100_000) + "\n")})
	tool := testReadArtifactTool(store, ref)

	for _, args := range []map[string]any{
		{"id": ref.ID, "offset": 0},
		{"id": ref.ID, "limit": 0},
		{"id": ref.ID, "limit": artifactReadMaxLines + 1},
	} {
		if _, err := tool.ExecuteOutput(context.Background(), args); err == nil {
			t.Fatalf("args %#v unexpectedly succeeded", args)
		}
	}
	output, err := tool.ExecuteOutput(context.Background(), map[string]any{"id": ref.ID, "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Text) > artifactReadMaxBytes {
		t.Fatalf("byte-capped output has %d bytes, max %d", len(output.Text), artifactReadMaxBytes)
	}

	output, err = tool.ExecuteOutput(context.Background(), map[string]any{
		"id": ref.ID, "query": strings.Repeat("missing", artifactReadMaxBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Text) > artifactReadMaxBytes {
		t.Fatalf("no-match response has %d bytes, max %d", len(output.Text), artifactReadMaxBytes)
	}
}

func TestReadArtifactToolReturnsTypedImageAndBinaryDescriptor(t *testing.T) {
	store := newTestArtifactStore()
	imageRef := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "pixel.png", Reference: "[image #7]", Data: []byte("image bytes")})
	binaryRef := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindBinary, MIMEType: "application/octet-stream", Name: "data.bin", Data: []byte("binary bytes")})
	refs := map[string]artifacts.Ref{imageRef.ID: imageRef, binaryRef.ID: binaryRef}
	tool := &readArtifactTool{store: store, lookup: func(id string) (artifacts.Ref, bool) {
		ref, ok := refs[id]
		return ref, ok
	}}

	imageOutput, err := tool.ExecuteOutput(context.Background(), map[string]any{"id": imageRef.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(imageOutput.Media) != 1 || string(imageOutput.Media[0].Data) != "image bytes" || imageOutput.Media[0].Reference != "[image #7]" {
		t.Fatalf("image output = %#v", imageOutput)
	}

	binaryOutput, err := tool.ExecuteOutput(context.Background(), map[string]any{"id": binaryRef.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(binaryOutput.Media) != 0 || !strings.Contains(binaryOutput.Text, "binary payloads are not inserted") {
		t.Fatalf("binary output = %#v", binaryOutput)
	}

	if _, err := tool.ExecuteOutput(context.Background(), map[string]any{"id": "sha256:" + strings.Repeat("0", 64)}); err == nil || !strings.Contains(err.Error(), "not referenced by this conversation") {
		t.Fatalf("unknown artifact error = %v", err)
	}
}

func testReadArtifactTool(store artifacts.Store, refs ...artifacts.Ref) *readArtifactTool {
	byID := make(map[string]artifacts.Ref, len(refs))
	for _, ref := range refs {
		byID[ref.ID] = ref
	}
	return &readArtifactTool{store: store, lookup: func(id string) (artifacts.Ref, bool) {
		ref, ok := byID[id]
		return ref, ok
	}}
}

func TestListArtifactsPaginationAndOrder(t *testing.T) {
	refs := make([]artifacts.Ref, 0, 120)
	for i := 0; i < 120; i++ {
		refs = append(refs, artifacts.Ref{
			ID: fmt.Sprintf("sha256:%064x", i), Kind: artifacts.KindText, Bytes: 100, Lines: 5, Name: "tool.txt",
		})
	}
	tool := &listArtifactsTool{list: func() []artifacts.Ref { return refs }}

	first, err := tool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "120 artifact(s) referenced by this conversation, in the order first referenced:") {
		t.Fatalf("catalog header = %q", first[:min(120, len(first))])
	}
	wantEntry := "1. " + refs[0].ID + "; text; 100 bytes; 5 lines; tool.txt"
	if !strings.Contains(first, wantEntry) {
		t.Fatalf("catalog first entry missing %q in %q", wantEntry, first[:min(300, len(first))])
	}
	if strings.Contains(first, "\n51. ") || !strings.HasSuffix(first, "[more entries; continue with offset=51]") {
		t.Fatalf("catalog page boundary wrong: %q", first[len(first)-min(120, len(first)):])
	}

	second, err := tool.Execute(context.Background(), map[string]any{"offset": 51})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, "\n51. "+refs[50].ID) || !strings.HasSuffix(second, "[more entries; continue with offset=101]") {
		t.Fatalf("second page = %q", second[len(second)-min(200, len(second)):])
	}

	last, err := tool.Execute(context.Background(), map[string]any{"offset": float64(101)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(last, "\n120. "+refs[119].ID) || strings.Contains(last, "more entries") {
		t.Fatalf("final page = %q", last[len(last)-min(200, len(last)):])
	}

	if estimatedStringTokens(first) > toolInlineTokenLimit {
		t.Fatalf("catalog page is large enough to be externalized: %d tokens", estimatedStringTokens(first))
	}

	past, err := tool.Execute(context.Background(), map[string]any{"offset": 500})
	if err != nil || past != "Artifact list has no entries at or after offset 500 (total 120)." {
		t.Fatalf("past-end catalog = %q, %v", past, err)
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"offset": 0}); err == nil {
		t.Fatal("offset 0 was accepted")
	}

	empty := &listArtifactsTool{list: func() []artifacts.Ref { return nil }}
	none, err := empty.Execute(context.Background(), map[string]any{})
	if err != nil || none != "No artifacts are referenced by this conversation." {
		t.Fatalf("empty catalog = %q, %v", none, err)
	}
}

func TestListArtifactsEntryFormats(t *testing.T) {
	image := artifacts.Ref{
		ID: "sha256:" + strings.Repeat("a", 64), Kind: artifacts.KindImage, Bytes: 2048,
		Name: "  chart   final.png ", ImageToken: "[image #3]",
	}
	binary := artifacts.Ref{ID: "sha256:" + strings.Repeat("b", 64), Kind: artifacts.KindBinary, Bytes: 9, MIMEType: "application/zip"}
	tool := &listArtifactsTool{list: func() []artifacts.Ref { return []artifacts.Ref{image, binary} }}
	out, err := tool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1. "+image.ID+"; image; 2048 bytes; chart final.png; reference [image #3]") {
		t.Fatalf("image entry = %q", out)
	}
	if !strings.Contains(out, "2. "+binary.ID+"; binary; 9 bytes") || strings.Contains(out, "lines") {
		t.Fatalf("binary entry = %q", out)
	}
}

func TestReadArtifactOverlongLinePlaceholderAndBytePaging(t *testing.T) {
	store := newTestArtifactStore()
	long := strings.Repeat(`{"k":"v"},`, 300_000) // 3,000,000 bytes, one physical line
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Data: []byte(long)})
	tool := testReadArtifactTool(store, ref)

	lines, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID})
	if err != nil {
		t.Fatalf("line mode failed on overlong line: %v", err)
	}
	if !strings.HasPrefix(lines, "1: [line is 3000000 bytes; exceeds inline limit; read with byte_offset=0]") {
		t.Fatalf("placeholder = %q", lines[:min(150, len(lines))])
	}

	var rebuilt strings.Builder
	nextPattern := regexp.MustCompile(`next byte_offset=(\d+)\]$`)
	offset := 0
	for i := 0; i < 200; i++ {
		out, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "byte_offset": offset})
		if err != nil {
			t.Fatal(err)
		}
		header, body, found := strings.Cut(out, "\n")
		if !found || !strings.HasPrefix(header, fmt.Sprintf("[artifact %s; bytes %d-", ref.ID, offset)) {
			t.Fatalf("window header = %q", header)
		}
		match := nextPattern.FindStringSubmatch(body)
		if match == nil {
			rebuilt.WriteString(body)
			break
		}
		rebuilt.WriteString(strings.TrimSuffix(body, "\n[artifact continues; next byte_offset="+match[1]+"]"))
		offset, _ = strconv.Atoi(match[1])
	}
	if rebuilt.String() != long {
		t.Fatalf("byte paging rebuilt %d bytes, want %d", rebuilt.Len(), len(long))
	}
}

func TestReadArtifactLinesAfterOverlongLineRemainReachable(t *testing.T) {
	store := newTestArtifactStore()
	content := "before\n" + strings.Repeat("x", 2<<20) + "\nafter overlong\nlast"
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Data: []byte(content)})
	tool := testReadArtifactTool(store, ref)

	out, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"1: before\n",
		"2: [line is 2097152 bytes; exceeds inline limit; read with byte_offset=7]\n",
		"3: after overlong\n",
		"4: last",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("line view missing %q: %q", want, out)
		}
	}

	paged, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "offset": 3})
	if err != nil || !strings.HasPrefix(paged, "3: after overlong") {
		t.Fatalf("offset paging past overlong line = %q, %v", paged[:min(80, len(paged))], err)
	}
}

func TestReadArtifactQueryMatchesInsideOverlongLine(t *testing.T) {
	store := newTestArtifactStore()
	// The needle spans the 1MiB hold boundary, so only a full-line carry search finds it.
	long := strings.Repeat("a", artifactScanMaxLine-3) + "NEEDLE" + strings.Repeat("b", 500)
	content := "plain NEEDLE here\n" + long + "\nno hit line"
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Data: []byte(content)})
	tool := testReadArtifactTool(store, ref)

	out, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "query": "NEEDLE"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1: plain NEEDLE here\n") {
		t.Fatalf("normal query match missing: %q", out)
	}
	if !strings.Contains(out, "2: [query matches inside "+fmt.Sprint(len(long))+"-byte line; read with byte_offset=18]") {
		t.Fatalf("overlong query match missing: %q", out)
	}
	if strings.Contains(out, "no hit line") {
		t.Fatalf("non-matching line leaked into query output: %q", out)
	}
}

func TestReadArtifactByteOffsetValidation(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Data: []byte("tiny content")})
	tool := testReadArtifactTool(store, ref)

	if _, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "byte_offset": 0, "query": "x"}); err == nil ||
		!strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("mode mixing error = %v", err)
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "byte_offset": -1}); err == nil {
		t.Fatal("negative byte_offset was accepted")
	}
	past, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "byte_offset": 100})
	if err != nil || past != "Artifact has no content at or after byte 100." {
		t.Fatalf("past-end byte read = %q, %v", past, err)
	}
	window, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "byte_offset": 5})
	if err != nil || !strings.Contains(window, "bytes 5-12 of 12; raw window]\ncontent") || strings.Contains(window, "continues") {
		t.Fatalf("final window = %q, %v", window, err)
	}
}

func TestReadArtifactByteWindowRejectsStoredSizeMismatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stored   string
		declared int64
	}{
		{name: "shorter", stored: "abc", declared: 6},
		{name: "trailing", stored: "abcdef", declared: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestArtifactStore()
			ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Data: []byte(tc.stored)})
			ref.Bytes = tc.declared
			tool := testReadArtifactTool(store, ref)

			if _, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "byte_offset": 0}); err == nil ||
				!strings.Contains(err.Error(), "stored size does not match transcript reference") {
				t.Fatalf("stored %d bytes with declared size %d: error = %v", len(tc.stored), tc.declared, err)
			}
		})
	}
}

func TestReadArtifactByteWindowPropagatesCloseError(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Data: []byte("content")})
	wantErr := errors.New("close artifact")
	tool := testReadArtifactTool(closeErrorArtifactStore{Store: store, err: wantErr}, ref)

	if _, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "byte_offset": 0}); !errors.Is(err, wantErr) {
		t.Fatalf("close error = %v, want %v", err, wantErr)
	}
}

func TestBoundedArtifactTextPropagatesLookaheadError(t *testing.T) {
	wantErr := errors.New("lookahead failed")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "plain storage error", err: wantErr},
		{name: "EOF joined with storage error", err: errors.Join(io.EOF, wantErr)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := io.MultiReader(strings.NewReader("first\n"), artifactToolErrorReader{err: tc.err})
			if _, err := boundedArtifactText(context.Background(), r, 1, 1, ""); !errors.Is(err, wantErr) {
				t.Fatalf("lookahead error = %v, want %v", err, wantErr)
			}
		})
	}
}

type closeErrorArtifactStore struct {
	artifacts.Store
	err error
}

func (s closeErrorArtifactStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	r, err := s.Store.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	return closeErrorReadCloser{ReadCloser: r, err: s.err}, nil
}

type closeErrorReadCloser struct {
	io.ReadCloser
	err error
}

func (r closeErrorReadCloser) Close() error {
	return errors.Join(r.ReadCloser.Close(), r.err)
}

type artifactToolErrorReader struct {
	err error
}

func (r artifactToolErrorReader) Read([]byte) (int, error) {
	return 0, r.err
}

func TestReadArtifactByteWindowAdvancesOnBinaryContent(t *testing.T) {
	store := newTestArtifactStore()
	binary := bytes.Repeat([]byte{0x80, 0xff, 0x00}, 30_000) // 90,000 invalid-UTF8 bytes
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Data: binary})
	tool := testReadArtifactTool(store, ref)

	out, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "byte_offset": 0})
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`next byte_offset=(\d+)\]$`).FindStringSubmatch(out)
	if match == nil {
		t.Fatalf("large binary window did not continue: %q", out[len(out)-min(100, len(out)):])
	}
	next, _ := strconv.Atoi(match[1])
	if next < artifactReadMaxBytes-256-int(utf8.UTFMax) {
		t.Fatalf("binary window advanced only to %d", next)
	}
}

func TestReadArtifactCapCutPreservesRuneBoundary(t *testing.T) {
	store := newTestArtifactStore()
	content := strings.Repeat("a", 40_892) + "б" + strings.Repeat("z", 5_000) + "\nrest"
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Data: []byte(content)})
	tool := testReadArtifactTool(store, ref)

	page, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "?") {
		t.Fatalf("cap cut split a rune into replacement characters: %q", page[len(page)-120:])
	}
	match := regexp.MustCompile(`continue with byte_offset=(\d+)\]$`).FindStringSubmatch(page)
	if match == nil || match[1] != "40892" {
		t.Fatalf("continuation offset = %v, want rune boundary 40892", match)
	}
	window, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "byte_offset": 40_892})
	if err != nil {
		t.Fatal(err)
	}
	if _, body, _ := strings.Cut(window, "\n"); !strings.HasPrefix(body, "б") {
		t.Fatalf("continuation page does not start with the split rune: %q", body[:min(40, len(body))])
	}
}
