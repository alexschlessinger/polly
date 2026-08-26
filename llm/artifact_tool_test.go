package llm

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
)

func TestReadArtifactToolBoundsAndNumbersText(t *testing.T) {
	store := artifacts.NewMemoryStore()
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
	store := artifacts.NewMemoryStore()
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
	store := artifacts.NewMemoryStore()
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
	store := artifacts.NewMemoryStore()
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
