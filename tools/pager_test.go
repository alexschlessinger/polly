package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestWithPageBytesOnlyLowersTheBound(t *testing.T) {
	ctx := context.Background()
	if got := PageBytes(ctx); got != PageMaxBytes {
		t.Fatalf("default bound = %d, want %d", got, PageMaxBytes)
	}
	lowered := WithPageBytes(ctx, 8<<10)
	for name, tc := range map[string]struct {
		ctx  context.Context
		want int
	}{
		"lowered":         {lowered, 8 << 10},
		"raised again":    {WithPageBytes(lowered, 16<<10), 8 << 10},
		"lowered further": {WithPageBytes(lowered, 4<<10), 4 << 10},
		"below minimum":   {WithPageBytes(ctx, 100), PageMinBytes},
		"above maximum":   {WithPageBytes(ctx, 1<<20), PageMaxBytes},
		"zero":            {WithPageBytes(lowered, 0), 8 << 10},
	} {
		if got := PageBytes(tc.ctx); got != tc.want {
			t.Errorf("%s: bound = %d, want %d", name, got, tc.want)
		}
	}
}

func TestPagersHonorTheContextBound(t *testing.T) {
	var b strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&b, "line %d %s\n", i, strings.Repeat("y", 50))
	}
	content := b.String()
	const bound = 4 << 10
	ctx := WithPageBytes(context.Background(), bound)

	lines, err := PageLines(ctx, strings.NewReader(content), "file", 1, PageMaxLines, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) > bound || !strings.Contains(lines, "continue with byte_offset=") {
		t.Fatalf("line page is %d bytes (bound %d) or has no continuation:\n%s", len(lines), bound, lines[max(0, len(lines)-120):])
	}

	window, err := PageByteWindow(ctx, strings.NewReader(content), "file", "f", int64(len(content)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(window) > bound || !strings.Contains(window, "next byte_offset=") {
		t.Fatalf("byte window is %d bytes (bound %d) or has no continuation", len(window), bound)
	}

	if capped := CapPageTextContext(ctx, content); len(capped) > bound {
		t.Fatalf("capped text is %d bytes, bound %d", len(capped), bound)
	}
}

// The byte window leaves room for its header and continuation note however
// long the name in the header, so paging can always go on.
func TestPageByteWindowLongNameKeepsContinuation(t *testing.T) {
	content := strings.Repeat("x", 4_000)
	name := "/" + strings.Repeat("deeply/nested/", 20) + "file.txt"
	ctx := WithPageBytes(context.Background(), PageMinBytes)
	window, err := PageByteWindow(ctx, strings.NewReader(content), "file", name, int64(len(content)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(window) > PageMinBytes || !strings.Contains(window, "next byte_offset=") {
		t.Fatalf("byte window is %d bytes (bound %d) or has no continuation:\n%s", len(window), PageMinBytes, window)
	}
}
