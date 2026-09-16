package textdiff

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnifiedIdentical(t *testing.T) {
	r := Unified("a", "b", "x\ny\n", "x\ny\n", 3, 0)
	if r.Diff != "" || r.Additions != 0 || r.Deletions != 0 || r.Truncated {
		t.Fatalf("identical: %+v", r)
	}
}

func TestUnifiedSingleLineChange(t *testing.T) {
	old := "1\n2\n3\n4\n5\n"
	new := "1\n2\nthree\n4\n5\n"
	r := Unified("a/f", "b/f", old, new, 3, 0)
	want := "--- a/f\n+++ b/f\n@@ -1,5 +1,5 @@\n 1\n 2\n-3\n+three\n 4\n 5\n"
	if r.Diff != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", r.Diff, want)
	}
	if r.Additions != 1 || r.Deletions != 1 {
		t.Fatalf("counts %+v", r)
	}
}

func TestUnifiedAllAddedAndDeleted(t *testing.T) {
	r := Unified("/dev/null", "b/f", "", "a\nb\n", 3, 0)
	if want := "--- /dev/null\n+++ b/f\n@@ -0,0 +1,2 @@\n+a\n+b\n"; r.Diff != want || r.Additions != 2 || r.Deletions != 0 {
		t.Fatalf("added: %q %+v", r.Diff, r)
	}
	r = Unified("a/f", "/dev/null", "a\nb\n", "", 3, 0)
	if want := "--- a/f\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-a\n-b\n"; r.Diff != want || r.Additions != 0 || r.Deletions != 2 {
		t.Fatalf("deleted: %q %+v", r.Diff, r)
	}
}

func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	return b.String()
}

func replaceLine(s string, n int, with string) string {
	lines := strings.SplitAfter(s, "\n")
	lines[n-1] = with + "\n"
	return strings.Join(lines, "")
}

func TestUnifiedSeparateHunks(t *testing.T) {
	old := numbered(60)
	new := replaceLine(replaceLine(old, 2, "two"), 50, "fifty")
	r := Unified("a", "b", old, new, 3, 0)
	if strings.Count(r.Diff, "@@ -") != 2 {
		t.Fatalf("expected two hunks:\n%s", r.Diff)
	}
	if !strings.Contains(r.Diff, "@@ -1,5 +1,5 @@\n") || !strings.Contains(r.Diff, "@@ -47,7 +47,7 @@\n") {
		t.Fatalf("hunk headers:\n%s", r.Diff)
	}
}

func TestUnifiedHunksMerge(t *testing.T) {
	old := numbered(20)
	// Changes at 5 and 11: five equal lines between them, within 2*context.
	new := replaceLine(replaceLine(old, 5, "five"), 11, "eleven")
	r := Unified("a", "b", old, new, 3, 0)
	if strings.Count(r.Diff, "@@ -") != 1 || !strings.Contains(r.Diff, "@@ -2,13 +2,13 @@\n") {
		t.Fatalf("expected one merged hunk:\n%s", r.Diff)
	}
	// Changes at 5 and 12: six equal lines between them, exactly 2*context, still one hunk.
	new = replaceLine(replaceLine(old, 5, "five"), 12, "twelve")
	if r := Unified("a", "b", old, new, 3, 0); strings.Count(r.Diff, "@@ -") != 1 {
		t.Fatalf("gap of 2*context should merge:\n%s", r.Diff)
	}
	// Seven equal lines between: two hunks.
	new = replaceLine(replaceLine(old, 5, "five"), 13, "thirteen")
	if r := Unified("a", "b", old, new, 3, 0); strings.Count(r.Diff, "@@ -") != 2 {
		t.Fatalf("gap over 2*context should split:\n%s", r.Diff)
	}
}

func TestUnifiedNoTrailingNewline(t *testing.T) {
	marker := "\\ No newline at end of file\n"
	r := Unified("a", "b", "x\ny\n", "x\ny", 3, 0)
	if want := "--- a\n+++ b\n@@ -1,2 +1,2 @@\n x\n-y\n+y\n" + marker; r.Diff != want {
		t.Fatalf("lost newline:\n%s", r.Diff)
	}
	r = Unified("a", "b", "x\ny", "x\nz", 3, 0)
	if want := "--- a\n+++ b\n@@ -1,2 +1,2 @@\n x\n-y\n" + marker + "+z\n" + marker; r.Diff != want {
		t.Fatalf("both without newline:\n%s", r.Diff)
	}
	r = Unified("a", "b", "x\ny", "q\ny", 3, 0)
	if want := "--- a\n+++ b\n@@ -1,2 +1,2 @@\n-x\n+q\n y\n" + marker; r.Diff != want {
		t.Fatalf("context without newline:\n%s", r.Diff)
	}
}

func TestUnifiedCRLFPreserved(t *testing.T) {
	r := Unified("a", "b", "x\r\ny\r\n", "x\r\nz\r\n", 3, 0)
	if want := "--- a\n+++ b\n@@ -1,2 +1,2 @@\n x\r\n-y\r\n+z\r\n"; r.Diff != want {
		t.Fatalf("crlf:\n%q", r.Diff)
	}
}

func TestUnifiedContextZero(t *testing.T) {
	old := numbered(5)
	r := Unified("a", "b", old, replaceLine(old, 3, "three"), 0, 0)
	if want := "--- a\n+++ b\n@@ -3 +3 @@\n-3\n+three\n"; r.Diff != want {
		t.Fatalf("context 0:\n%s", r.Diff)
	}
	r = Unified("a", "b", old, "1\n2\n3\nx\n4\n5\n", 0, 0)
	if want := "--- a\n+++ b\n@@ -3,0 +4 @@\n+x\n"; r.Diff != want {
		t.Fatalf("insertion with context 0:\n%s", r.Diff)
	}
}

func TestUnifiedMaxEditsFallback(t *testing.T) {
	old := numbered(3000)
	new := strings.ReplaceAll(old, "\n", "!\n")
	r := Unified("a", "b", old, new, 3, 1000)
	if !r.Truncated || r.Diff != "" || r.Additions != 3000 || r.Deletions != 3000 {
		t.Fatalf("fallback: %+v", r)
	}
	// A large region with few edits still diffs: the cap is on edits.
	new = replaceLine(replaceLine(old, 2, "two"), 2999, "late")
	r = Unified("a", "b", old, new, 3, 1000)
	if r.Truncated || r.Additions != 2 || r.Deletions != 2 {
		t.Fatalf("few edits: %+v", r)
	}
	// Edit distance exactly at the cap is still computed.
	new = replaceLine(replaceLine(old, 2, "two"), 2999, "late")
	if r = Unified("a", "b", old, new, 3, 4); r.Truncated {
		t.Fatalf("at cap: %+v", r)
	}
	if r = Unified("a", "b", old, new, 3, 3); !r.Truncated {
		t.Fatalf("over cap: %+v", r)
	}
	// A region over maxRegionLines falls back regardless of edits.
	huge := strings.Repeat("x\n", maxRegionLines)
	if r = Unified("a", "b", "a\n"+huge+"b\n", "c\n"+huge+"d\n", 3, 0); !r.Truncated || r.Additions != maxRegionLines+2 {
		t.Fatalf("huge region: %+v", r)
	}
}

func TestCounts(t *testing.T) {
	old := numbered(10)
	if a, d := Counts(old, "1\n2\n3\nnew\n4\n5\n6\n7\n8\n9\n10\n"); a != 1 || d != 0 {
		t.Fatalf("insert: %d %d", a, d)
	}
	if a, d := Counts(old, old); a != 0 || d != 0 {
		t.Fatalf("identical: %d %d", a, d)
	}
	if a, d := Counts("", "x\n"); a != 1 || d != 0 {
		t.Fatalf("created: %d %d", a, d)
	}
}

func TestSplitLines(t *testing.T) {
	cases := map[string][]string{
		"":         nil,
		"a":        {"a"},
		"a\n":      {"a\n"},
		"a\nb":     {"a\n", "b"},
		"a\r\nb\n": {"a\r\n", "b\n"},
		"\n\n":     {"\n", "\n"},
	}
	for in, want := range cases {
		got := splitLines(in)
		if strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
			t.Fatalf("%q: %q", in, got)
		}
	}
}

// gitDiff returns the hunk body of git diff --no-index with the Myers
// algorithm, or "" when the files are identical.
func gitDiff(t *testing.T, old, new string) string {
	t.Helper()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte(new), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-c", "diff.noprefix=false", "-c", "core.autocrlf=false", "diff", "--no-index", "--no-color", "--diff-algorithm=myers", "--no-indent-heuristic", "-U3", "--", a, b)
	out, _ := cmd.Output()
	return stripHeaders(string(out))
}

func stripHeaders(diff string) string {
	var lines []string
	for _, line := range strings.SplitAfter(diff, "\n") {
		if strings.HasPrefix(line, "diff --git") || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "")
}

func TestUnifiedMatchesGit(t *testing.T) {
	old := numbered(30)
	cases := []string{
		replaceLine(old, 7, "seven"),
		strings.Replace(old, "10\n", "", 1),
		strings.Replace(old, "10\n", "10\nten\n", 1),
		replaceLine(replaceLine(old, 3, "a"), 28, "b"),
		"1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15\n16\n17\n18\n19\n20\n21\n22\n23\n24\n25\n26\n27\n28\n29\n30",
	}
	for _, new := range cases {
		ours := stripHeaders(Unified("a/a", "b/b", old, new, 3, 0).Diff)
		if git := gitDiff(t, old, new); ours != git {
			t.Fatalf("mismatch with git for %q:\nours:\n%s\ngit:\n%s", new, ours, git)
		}
	}
}

func TestUnifiedRandomAgreesWithGitCounts(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	words := []string{"a", "b", "c", "d", "e"}
	for i := 0; i < 20; i++ {
		var oldLines, newLines []string
		for j := 0; j < 40; j++ {
			oldLines = append(oldLines, words[rng.Intn(len(words))])
		}
		for _, l := range oldLines {
			switch rng.Intn(6) {
			case 0:
			case 1:
				newLines = append(newLines, words[rng.Intn(len(words))])
			case 2:
				newLines = append(newLines, l, words[rng.Intn(len(words))])
			default:
				newLines = append(newLines, l)
			}
		}
		old, new := strings.Join(oldLines, "\n")+"\n", strings.Join(newLines, "\n")+"\n"
		r := Unified("a", "b", old, new, 3, 0)
		git := gitDiff(t, old, new)
		adds, dels := strings.Count(git, "\n+")+boolInt(strings.HasPrefix(git, "+")), strings.Count(git, "\n-")+boolInt(strings.HasPrefix(git, "-"))
		if r.Additions != adds || r.Deletions != dels {
			t.Fatalf("case %d counts ours +%d -%d git +%d -%d\nours:\n%s\ngit:\n%s", i, r.Additions, r.Deletions, adds, dels, r.Diff, git)
		}
		// The script must reproduce new from old.
		if applied := apply(old, r.Diff); applied != new {
			t.Fatalf("case %d apply mismatch:\n%s", i, r.Diff)
		}
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func parseRange(s string) (start, length int) {
	length = 1
	if _, err := fmt.Sscanf(s, "%d,%d", &start, &length); err != nil {
		fmt.Sscanf(s, "%d", &start)
	}
	return start, length
}

// apply replays a unified diff produced by Unified onto old.
func apply(old, diff string) string {
	src := splitLines(old)
	var out strings.Builder
	pos := 0
	lines := strings.SplitAfter(diff, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !strings.HasPrefix(line, "@@ ") {
			continue
		}
		aStart, aLen := parseRange(strings.Fields(line)[1][1:])
		start := aStart - 1
		if aLen == 0 {
			start = aStart
		}
		for ; pos < start; pos++ {
			out.WriteString(src[pos])
		}
		for i++; i < len(lines) && !strings.HasPrefix(lines[i], "@@ "); i++ {
			switch {
			case strings.HasPrefix(lines[i], "\\"):
			case strings.HasPrefix(lines[i], " "):
				out.WriteString(lines[i][1:])
				pos++
			case strings.HasPrefix(lines[i], "-"):
				pos++
			case strings.HasPrefix(lines[i], "+"):
				out.WriteString(lines[i][1:])
			}
		}
		i--
	}
	for ; pos < len(src); pos++ {
		out.WriteString(src[pos])
	}
	return out.String()
}
