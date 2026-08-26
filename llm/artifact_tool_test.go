package llm

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
