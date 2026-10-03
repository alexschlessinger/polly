package tools

import (
	"context"
	"strings"
	"testing"
)

// The byte window leaves room for its header and continuation note however
// long the name in the header, so paging can always go on.
func TestPageByteWindowLongNameKeepsContinuation(t *testing.T) {
	content := strings.Repeat("x", 2*PageMaxBytes)
	name := "/" + strings.Repeat("deeply/nested/", 200) + "file.txt"
	window, err := PageByteWindow(context.Background(), strings.NewReader(content), "file", name, int64(len(content)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(window) > PageMaxBytes || !strings.Contains(window, "next byte_offset=") {
		t.Fatalf("byte window is %d bytes (cap %d) or has no continuation", len(window), PageMaxBytes)
	}
}

// An agent lowers the page cap through the context; it never rises above
// PageMaxBytes.
func TestPagesHonorTheContextsPageCap(t *testing.T) {
	text := strings.Repeat("a line of text\n", 10_000)
	ctx := WithPageBytes(context.Background(), 2_000)
	page, err := PageLines(ctx, strings.NewReader(text), "file", 1, PageMaxLines, "")
	if err != nil || len(page) > 2_000 || !strings.Contains(page, "offset=") {
		t.Fatalf("page = %d bytes, %v; want at most 2000 with a continuation", len(page), err)
	}
	if got := PageBytes(WithPageBytes(context.Background(), PageMaxBytes*2)); got != PageMaxBytes {
		t.Fatalf("raised page cap = %d", got)
	}
	if got := CapPageText(ctx, strings.Repeat("b", 5_000)); len(got) != 2_000 {
		t.Fatalf("capped text = %d bytes", len(got))
	}
}
