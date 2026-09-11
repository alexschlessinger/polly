package main

import (
	"bytes"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// formatBashCommand formats a display copy. Shell word contents are never
// split to find operators: the parser owns the structural boundaries.
func formatBashCommand(command string) string {
	parse := func(text string) (*syntax.File, error) {
		return syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(true)).Parse(strings.NewReader(text), "")
	}
	file, err := parse(command)
	if err != nil {
		return command
	}
	var breaks []int
	heredoc := false
	add := func(offset int) {
		before := strings.TrimRight(command[:offset], " \t")
		after := strings.TrimLeft(command[offset:], " \t")
		if before != "" && !strings.HasSuffix(before, "\n") && !strings.HasPrefix(after, "\n") {
			breaks = append(breaks, offset)
		}
	}
	body := func(stmts []*syntax.Stmt, close syntax.Pos) {
		if len(stmts) > 0 {
			add(int(stmts[0].Pos().Offset()))
			if close.IsValid() {
				add(int(close.Offset()))
			}
		}
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Word:
			return false // quoted strings and substitutions keep their own layout
		case *syntax.Redirect:
			heredoc = heredoc || n.Hdoc != nil
		case *syntax.BinaryCmd:
			add(int(n.OpPos.Offset()) + len(n.Op.String()))
		case *syntax.ForClause:
			body(n.Do, n.DonePos)
		case *syntax.WhileClause:
			body(n.Do, n.DonePos)
		case *syntax.IfClause:
			body(n.Then, n.FiPos)
			if n.Else != nil {
				add(int(n.Else.Position.Offset()))
			}
		case *syntax.Block:
			body(n.Stmts, n.Rbrace)
		case *syntax.Subshell:
			body(n.Stmts, n.Rparen)
		}
		return true
	})
	// A newline can begin a heredoc body. Let the standard printer handle
	// those programs without introducing additional parse boundaries.
	if !heredoc && len(breaks) > 0 {
		slices.Sort(breaks)
		breaks = slices.Compact(breaks)
		var expanded strings.Builder
		start := 0
		for _, at := range breaks {
			expanded.WriteString(command[start:at])
			expanded.WriteByte('\n')
			start = at
		}
		expanded.WriteString(command[start:])
		if formatted, err := parse(expanded.String()); err == nil {
			file = formatted
		}
	}
	var out bytes.Buffer
	if err := syntax.NewPrinter(syntax.Indent(2)).Print(&out, file); err != nil {
		return command
	}
	formatted := strings.TrimRight(out.String(), "\n")
	if !heredoc {
		if printed, err := parse(formatted); err == nil {
			formatted = alignBashContinuations(formatted, printed)
		}
	}
	return formatted
}

// The shell printer can give the last stage of a mixed &&/pipeline chain an
// extra indent. Align simple-command continuations within the same expression;
// compound commands retain the printer's block layout. Parser positions keep
// indentation changes outside quoted words and other literal contents.
func alignBashContinuations(text string, file *syntax.File) string {
	lines := strings.Split(text, "\n")
	var collect func(*syntax.Stmt, *[]*syntax.Stmt) bool
	collect = func(stmt *syntax.Stmt, stages *[]*syntax.Stmt) bool {
		switch cmd := stmt.Cmd.(type) {
		case *syntax.BinaryCmd:
			return collect(cmd.X, stages) && collect(cmd.Y, stages)
		case *syntax.CallExpr:
			*stages = append(*stages, stmt)
			return true
		}
		return false
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		if _, ok := node.(*syntax.Word); ok {
			return false
		}
		cmd, ok := node.(*syntax.BinaryCmd)
		if !ok {
			return true
		}
		var stages []*syntax.Stmt
		if !collect(cmd.X, &stages) || !collect(cmd.Y, &stages) {
			return true
		}
		first := stages[0].Pos().Line() - 1
		indent := len(lines[first]) - len(strings.TrimLeft(lines[first], " ")) + 2
		for _, stage := range stages[1:] {
			pos := stage.Pos()
			line := pos.Line() - 1
			body := strings.TrimLeft(lines[line], " ")
			if line > first && int(pos.Col()-1) == len(lines[line])-len(body) {
				lines[line] = strings.Repeat(" ", indent) + body
			}
		}
		return false
	})
	return strings.Join(lines, "\n")
}
