package main

import (
	"slices"
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
			// Shell options set before the first command are setup for the
			// script, like a leading cd: the display opens with the work.
			stmts := file.Stmts
			for len(stmts) > 1 && isBashOptionsPreamble(stmts[0]) {
				stmts = stmts[1:]
			}
			for i, stmt := range stmts {
				if i > 0 && !stmts[i-1].Background {
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

// isBashCDPreamble reports a plain "cd <one word>" statement: no assignments,
// redirections, negation or backgrounding, so only the directory changes.
func isBashCDPreamble(stmt *syntax.Stmt) bool {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	return ok && len(call.Args) == 2 && call.Args[0].Lit() == "cd" &&
		len(call.Assigns) == 0 && len(stmt.Redirs) == 0 && !stmt.Negated && !stmt.Background
}

// isBashOptionsPreamble reports a plain "set" of shell options, such as
// "set -euo pipefail": no positional parameters, assignments, redirections,
// negation or backgrounding, so only how the script fails or echoes changes.
// A bare "set" or "set -o" prints instead, and stays.
func isBashOptionsPreamble(stmt *syntax.Stmt) bool {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) < 2 || call.Args[0].Lit() != "set" ||
		len(call.Assigns) > 0 || len(stmt.Redirs) > 0 || stmt.Negated || stmt.Background || stmt.Coprocess {
		return false
	}
	for i := 1; i < len(call.Args); i++ {
		option := call.Args[i].Lit()
		if len(option) < 2 || (option[0] != '-' && option[0] != '+') || option[1] == '-' {
			return false
		}
		// -o and +o take the option's name as the next word.
		if option[len(option)-1] == 'o' {
			i++
			if i == len(call.Args) || !syntax.ValidName(call.Args[i].Lit()) {
				return false
			}
		}
	}
	return true
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
		// A leading "cd <dir>" is setup for the rest of the line, not the call
		// the line is about. Drop it and its operator, so the display does not
		// open with "… &&" on every command that names its directory first.
		// Shell options are setup in the same way.
		if binary.Op == syntax.AndStmt && (isBashCDPreamble(binary.X) || isBashOptionsPreamble(binary.X)) {
			s.appendStmt(command, binary.Y)
			return
		}
		s.appendStmt(command, binary.X)
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
	// Every command shown keeps room to be read: a third of the row, at most
	// bashStageRoom columns, or all of a shorter command. The commands a row
	// cannot give that much are left out, an ellipsis in their place after
	// the operator that led to them, so a long script shows how it begins
	// rather than a stub of each of its commands.
	room := min(bashStageRoom, max(bashStageStub, width/3))
	budgets, floors := make([]int, len(parts)), make([]int, len(parts))
	need := max(0, len(parts)-1) // spaces between parts
	for i, part := range s.parts {
		budgets[i] = rw.StringWidth(parts[i])
		floors[i] = budgets[i]
		if !part.operator {
			floors[i] = min(budgets[i], room)
		}
		need += floors[i]
	}
	keep := len(parts)
	for need > width {
		// Drop the last command shown, and keep the operator before it.
		last := keep - 1
		for last > 0 && s.parts[last].operator {
			last--
		}
		if last == 0 {
			break // the first command is always shown, however little of it fits
		}
		if keep == len(parts) {
			need += 2 // reserve the ellipsis and the space before it once
		}
		for _, floor := range floors[last:keep] {
			need -= floor + 1
		}
		keep = last
	}
	parts, budgets = parts[:keep], budgets[:keep]
	whole := slices.Clone(budgets)
	total := max(0, keep-1)
	for _, budget := range budgets {
		total += budget
	}
	if keep < len(s.parts) {
		total += 2
	}
	// Share what is left across the commands shown, preserving the chain's
	// operators and short stages such as "tail -40" while shrinking its
	// longest stage first.
	for total > width {
		longest := -1
		for i := range parts {
			if budgets[i] > floors[i] && (longest < 0 || budgets[i] > budgets[longest]) {
				longest = i
			}
		}
		if longest < 0 {
			break
		}
		cut := min(budgets[longest]-floors[longest], max(1, (total-width)/max(1, len(parts))))
		budgets[longest] -= cut
		total -= cut
	}
	// A command left a few columns short of whole takes them from the one
	// cut the most: one ellipsis fewer on the row, at the cost of the tail
	// of a command that was cut already.
	for i := range parts {
		short := whole[i] - budgets[i]
		if short < 1 || short > bashSliver {
			continue
		}
		donor := -1
		for j := range parts {
			if j != i && whole[j]-budgets[j] > bashSliver && budgets[j]-short >= floors[j] && (donor < 0 || budgets[j] > budgets[donor]) {
				donor = j
			}
		}
		if donor >= 0 {
			budgets[donor] -= short
			budgets[i] = whole[i]
		}
	}
	for i := range parts {
		parts[i] = rw.Truncate(parts[i], budgets[i], "…")
	}
	if keep < len(s.parts) {
		parts = append(parts, "…")
	}
	return rw.Truncate(strings.Join(parts, " "), width, "…")
}

const (
	// bashStageRoom is the most a row promises one command of a script
	// before it leaves out the commands after it.
	bashStageRoom = 32
	// bashStageStub is the least a command shown is cut to.
	bashStageStub = 8
	// bashSliver is the most columns a command gets back to be shown whole.
	bashSliver = 3
)

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
