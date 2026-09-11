package main

import (
	"strings"
	"unicode"

	rw "github.com/mattn/go-runewidth"
	"mvdan.cc/sh/v3/syntax"
)

// bashSummary is a display projection, parsed once when a call arrives. The
// original arguments remain the source for execution, inspection and copying.
type bashSummary struct {
	parts []bashSummaryPart
}

type bashSummaryPart struct {
	text     string
	short    string
	operator bool
}

func newBashSummary(command string) *bashSummary {
	s := &bashSummary{}
	// Bound parsing work for large generated scripts. Even the fallback marks
	// omitted lines, instead of silently presenting the first line as the call.
	if len(command) <= 64<<10 {
		file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
		if err == nil && len(file.Stmts) > 0 {
			for i, stmt := range file.Stmts {
				if i > 0 && !file.Stmts[i-1].Background {
					s.parts = append(s.parts, bashSummaryPart{text: ";", operator: true})
				}
				s.appendStmt(command, stmt)
			}
			for i := range s.parts {
				if !s.parts[i].operator {
					s.parts[i].short = shortenBashPaths(s.parts[i].text)
				}
			}
			return s
		}
	}
	s.parts = []bashSummaryPart{{text: bashSummaryLine(command)}}
	return s
}

func bashSummaryLine(text string) string {
	first, _, more := strings.Cut(strings.TrimSpace(text), "\n")
	first = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, first)
	if more {
		first += " …"
	}
	return first
}

func bashSource(command string, node syntax.Node) string {
	return command[node.Pos().Offset():node.End().Offset()]
}

func bashStatementSource(command string, stmt *syntax.Stmt) string {
	end := stmt.End().Offset()
	if stmt.Semicolon.IsValid() && !stmt.Background && !stmt.Coprocess {
		end = stmt.Semicolon.Offset()
	}
	return command[stmt.Pos().Offset():end]
}

func (s *bashSummary) appendStmt(command string, stmt *syntax.Stmt) {
	// Keep redirections, backgrounding and compound constructs together. They
	// must not disappear while simplifying the command nested inside them.
	if stmt.Negated || stmt.Background || stmt.Coprocess || len(stmt.Redirs) > 0 {
		s.parts = append(s.parts, bashSummaryPart{text: bashSummaryLine(bashStatementSource(command, stmt))})
		return
	}
	if binary, ok := stmt.Cmd.(*syntax.BinaryCmd); ok {
		left, simple := binary.X.Cmd.(*syntax.CallExpr)
		if binary.Op == syntax.AndStmt && simple && len(left.Args) == 2 && left.Args[0].Lit() == "cd" &&
			len(left.Assigns) == 0 && len(binary.X.Redirs) == 0 && !binary.X.Negated && !binary.X.Background {
			s.parts = append(s.parts, bashSummaryPart{text: "…"})
		} else {
			s.appendStmt(command, binary.X)
		}
		s.parts = append(s.parts, bashSummaryPart{text: binary.Op.String(), operator: true})
		s.appendStmt(command, binary.Y)
		return
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		s.parts = append(s.parts, bashSummaryPart{text: bashSummaryLine(bashStatementSource(command, stmt))})
		return
	}
	args := call.Args
	folded := len(call.Assigns) > 0
	// Only the plain env NAME=value form is setup. Options such as env -i,
	// wrappers, substitutions and quoted command names remain visible.
	if args[0].Lit() == "env" {
		n := 1
		for n < len(args)-1 {
			name, _, assignment := strings.Cut(args[n].Lit(), "=")
			if !assignment || !syntax.ValidName(name) {
				break
			}
			n++
		}
		if n > 1 {
			args, folded = args[n:], true
		}
	}
	var words []string
	if folded {
		words = append(words, "…")
	}
	for _, word := range args {
		words = append(words, bashSummaryLine(bashSource(command, word)))
	}
	s.parts = append(s.parts, bashSummaryPart{text: strings.Join(words, " ")})
}

func (s *bashSummary) fit(width int) string {
	if width < 1 {
		return ""
	}
	parts := make([]string, len(s.parts))
	for i, part := range s.parts {
		parts[i] = part.text
	}
	joined := strings.Join(parts, " ")
	if rw.StringWidth(joined) <= width {
		return joined
	}
	// Shorten only obvious path tokens; quoted values and shell expressions
	// are left intact. A middle ellipsis retains the filename being acted on.
	for i, part := range s.parts {
		if part.short != "" {
			parts[i] = part.short
		}
	}
	// Share the budget across commands, preserving the chain's operators and
	// short stages such as "tail -40" while shrinking its longest stage first.
	budgets := make([]int, len(parts))
	total := max(0, len(parts)-1)
	for i := range parts {
		budgets[i] = rw.StringWidth(parts[i])
		total += budgets[i]
	}
	for total > width {
		longest := -1
		for i, part := range s.parts {
			if !part.operator && budgets[i] > 8 && (longest < 0 || budgets[i] > budgets[longest]) {
				longest = i
			}
		}
		if longest < 0 {
			break
		}
		cut := min(budgets[longest]-8, max(1, (total-width)/max(1, len(parts))))
		budgets[longest] -= cut
		total -= cut
	}
	for i := range parts {
		parts[i] = rw.Truncate(parts[i], budgets[i], "…")
	}
	return rw.Truncate(strings.Join(parts, " "), width, "…")
}

func shortenBashPaths(text string) string {
	// Cache a path-shortened variant using shell word boundaries; whitespace
	// splitting would mistake pieces of a quoted argument for paths.
	var spans [][2]int
	for word, parseErr := range syntax.NewParser().WordsSeq(strings.NewReader(text)) {
		if parseErr != nil {
			return text
		}
		raw := bashSource(text, word)
		if word.Lit() == raw && len(raw) > 20 && strings.Count(raw, "/") >= 2 &&
			!strings.ContainsAny(raw, ":*?[]{}\\$=\"'") {
			spans = append(spans, [2]int{int(word.Pos().Offset()), int(word.End().Offset())})
		}
	}
	for i := len(spans) - 1; i >= 0; i-- {
		span := spans[i]
		path := text[span[0]:span[1]]
		first, last := strings.IndexByte(path, '/'), strings.LastIndexByte(path, '/')
		short := path[:first+1] + "…" + path[last:]
		if len(short) < len(path) {
			text = text[:span[0]] + short + text[span[1]:]
		}
	}
	return text
}
