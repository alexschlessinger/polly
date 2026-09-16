// Package textdiff computes line-oriented unified diffs without external
// dependencies. It exists so tools can describe what they changed to a user
// interface; it is not a patch engine.
package textdiff

import (
	"fmt"
	"strings"
)

// Result is a unified diff and its line counts. Additions and Deletions are
// always complete; Diff is empty and Truncated is set when the inputs were
// too large to diff within maxLines.
type Result struct {
	Diff      string
	Additions int
	Deletions int
	Truncated bool
}

// Unified diffs old and new line by line and renders "--- oldName" and
// "+++ newName" headers followed by hunks with context lines of context.
// Lines keep their own terminators, so CRLF content round-trips, and a final
// line without a terminator is followed by "\ No newline at end of file" as
// git prints it. When the number of differing lines exceeds maxLines (when
// positive), the result carries counts only.
func Unified(oldName, newName, old, new string, context, maxLines int) Result {
	if old == new {
		return Result{}
	}
	a, b := splitLines(old), splitLines(new)
	prefix, suffix := commonAffixes(a, b)
	midA, midB := a[prefix:len(a)-suffix], b[prefix:len(b)-suffix]
	if maxLines > 0 && len(midA)+len(midB) > maxLines {
		return Result{Additions: len(midB), Deletions: len(midA), Truncated: true}
	}
	ops := make([]byte, 0, prefix+suffix+len(midA)+len(midB))
	for range prefix {
		ops = append(ops, opEqual)
	}
	ops = myers(midA, midB, ops)
	for range suffix {
		ops = append(ops, opEqual)
	}
	return render(oldName, newName, a, b, ops, context)
}

// Counts returns the number of added and deleted lines between old and new
// without computing an edit script: lines outside the common prefix and
// suffix count as replaced.
func Counts(old, new string) (adds, dels int) {
	if old == new {
		return 0, 0
	}
	a, b := splitLines(old), splitLines(new)
	prefix, suffix := commonAffixes(a, b)
	return len(b) - prefix - suffix, len(a) - prefix - suffix
}

const (
	opEqual  = '='
	opDelete = '-'
	opInsert = '+'
)

// splitLines splits s into lines that keep their terminators. "a\nb" yields
// ["a\n", "b"]; "" yields no lines.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := make([]string, 0, strings.Count(s, "\n")+1)
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines = append(lines, s)
			break
		}
		lines = append(lines, s[:i+1])
		s = s[i+1:]
	}
	return lines
}

func commonAffixes(a, b []string) (prefix, suffix int) {
	n := min(len(a), len(b))
	for prefix < n && a[prefix] == b[prefix] {
		prefix++
	}
	for suffix < n-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	return prefix, suffix
}

// myers appends the edit script turning a into b using linear-space Myers.
func myers(a, b []string, ops []byte) []byte {
	if len(a) == 0 {
		for range b {
			ops = append(ops, opInsert)
		}
		return ops
	}
	if len(b) == 0 {
		for range a {
			ops = append(ops, opDelete)
		}
		return ops
	}
	prefix, suffix := commonAffixes(a, b)
	for range prefix {
		ops = append(ops, opEqual)
	}
	a2, b2 := a[prefix:len(a)-suffix], b[prefix:len(b)-suffix]
	if len(a2) > 0 && len(b2) > 0 {
		x, y, u, v := middleSnake(a2, b2)
		ops = myers(a2[:x], b2[:y], ops)
		for range u - x {
			ops = append(ops, opEqual)
		}
		ops = myers(a2[u:], b2[v:], ops)
	} else {
		ops = myers(a2, b2, ops)
	}
	for range suffix {
		ops = append(ops, opEqual)
	}
	return ops
}

// middleSnake finds a middle snake (x,y)-(u,v) of a shortest edit script
// for non-empty a and b, following Myers 1986.
func middleSnake(a, b []string) (x, y, u, v int) {
	n, m := len(a), len(b)
	delta := n - m
	odd := delta&1 != 0
	limit := (n+m+1)/2 + 1
	size := 2*limit + 2
	vf := make([]int, size)
	vb := make([]int, size)
	at := func(k int) int { return k + limit + 1 }
	for d := 0; d <= limit; d++ {
		for k := -d; k <= d; k += 2 {
			var px int
			if k == -d || (k != d && vf[at(k-1)] < vf[at(k+1)]) {
				px = vf[at(k+1)]
			} else {
				px = vf[at(k-1)] + 1
			}
			py := px - k
			cx, cy := px, py
			for cx < n && cy < m && a[cx] == b[cy] {
				cx++
				cy++
			}
			vf[at(k)] = cx
			if odd {
				if kb := delta - k; kb >= -(d-1) && kb <= d-1 && cx+vb[at(kb)] >= n {
					return px, py, cx, cy
				}
			}
		}
		for k := -d; k <= d; k += 2 {
			var px int
			if k == -d || (k != d && vb[at(k-1)] < vb[at(k+1)]) {
				px = vb[at(k+1)]
			} else {
				px = vb[at(k-1)] + 1
			}
			py := px - k
			cx, cy := px, py
			for cx < n && cy < m && a[n-1-cx] == b[m-1-cy] {
				cx++
				cy++
			}
			vb[at(k)] = cx
			if !odd {
				if kf := delta - k; kf >= -d && kf <= d && vf[at(kf)]+cx >= n {
					return n - cx, m - cy, n - px, m - py
				}
			}
		}
	}
	panic("textdiff: no middle snake")
}

type hunk struct {
	aStart, aLen, bStart, bLen int
	lines                      []string
}

// render turns an edit script into unified hunks with context lines and
// git-style headers.
func render(oldName, newName string, a, b []string, ops []byte, context int) Result {
	var res Result
	for _, op := range ops {
		switch op {
		case opInsert:
			res.Additions++
		case opDelete:
			res.Deletions++
		}
	}
	var sb strings.Builder
	sb.WriteString("--- ")
	sb.WriteString(oldName)
	sb.WriteString("\n+++ ")
	sb.WriteString(newName)
	sb.WriteByte('\n')

	// Walk the script, opening a hunk at the first change and closing it
	// when a run of equal lines longer than 2*context follows.
	ai, bi := 0, 0
	var h *hunk
	var pending int // equal lines seen since the last change inside a hunk
	flush := func() {
		if h == nil {
			return
		}
		// Drop trailing context beyond `context`.
		if pending > context {
			extra := pending - context
			h.lines = h.lines[:len(h.lines)-extra]
			h.aLen -= extra
			h.bLen -= extra
		}
		writeHunk(&sb, h)
		h = nil
		pending = 0
	}
	for i, op := range ops {
		if op == opEqual {
			if h != nil {
				h.lines = append(h.lines, " "+lineText(a, ai))
				h.aLen++
				h.bLen++
				pending++
				if pending > 2*context {
					flush()
				}
			}
			ai++
			bi++
			continue
		}
		if h == nil {
			h = &hunk{}
			lead := 0
			for j := i - 1; j >= 0 && lead < context && ops[j] == opEqual; j-- {
				lead++
			}
			h.aStart, h.bStart = ai-lead, bi-lead
			for j := lead; j > 0; j-- {
				h.lines = append(h.lines, " "+lineText(a, ai-j))
			}
			h.aLen, h.bLen = lead, lead
		}
		pending = 0
		if op == opDelete {
			h.lines = append(h.lines, "-"+lineText(a, ai))
			h.aLen++
			ai++
		} else {
			h.lines = append(h.lines, "+"+lineText(b, bi))
			h.bLen++
			bi++
		}
	}
	flush()
	res.Diff = sb.String()
	return res
}

// lineText returns line i for output: a line without a trailing newline
// gains one plus the no-newline marker.
func lineText(lines []string, i int) string {
	line := lines[i]
	if strings.HasSuffix(line, "\n") {
		return line
	}
	return line + "\n\\ No newline at end of file\n"
}

func writeHunk(sb *strings.Builder, h *hunk) {
	sb.WriteString("@@ -")
	sb.WriteString(rangeText(h.aStart, h.aLen))
	sb.WriteString(" +")
	sb.WriteString(rangeText(h.bStart, h.bLen))
	sb.WriteString(" @@\n")
	for _, line := range h.lines {
		sb.WriteString(line)
	}
}

// rangeText formats a hunk range the way git does: a single line omits the
// length, and an empty range reports the line it follows.
func rangeText(start, length int) string {
	switch length {
	case 0:
		return fmt.Sprintf("%d,0", start)
	case 1:
		return fmt.Sprintf("%d", start+1)
	default:
		return fmt.Sprintf("%d,%d", start+1, length)
	}
}
