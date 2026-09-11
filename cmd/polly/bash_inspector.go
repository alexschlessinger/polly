package main

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	rw "github.com/mattn/go-runewidth"
	"mvdan.cc/sh/v3/syntax"
)

// Immutable display data. The original tool call remains the source of truth.
type bashInspectorCommand struct {
	setup, directory string
	variables        int
	formatted        string
	compact          string
}

func newBashInspectorCommand(command string) *bashInspectorCommand {
	b := &bashInspectorCommand{}
	parse := func(text string) (*syntax.File, error) {
		return syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(true)).Parse(strings.NewReader(text), "")
	}
	if len(command) <= 64<<10 {
		if file, err := parse(command); err == nil && len(file.Stmts) > 0 {
			// Only peel a leading, conditional setup chain. Pipelines, ||,
			// background jobs, redirections and compound commands keep their
			// scope and execution order in the main command.
			var stages []*syntax.Stmt
			var operators []syntax.Pos
			var flatten func(*syntax.Stmt)
			flatten = func(stmt *syntax.Stmt) {
				if binary, ok := stmt.Cmd.(*syntax.BinaryCmd); ok && binary.Op == syntax.AndStmt && plainSetupStatement(stmt) {
					flatten(binary.X)
					operators = append(operators, binary.OpPos)
					flatten(binary.Y)
				} else {
					stages = append(stages, stmt)
				}
			}
			flatten(file.Stmts[0])
			folded := 0
			for folded < len(stages)-1 {
				directory, variables, ok := bashSetupStatement(command, stages[folded])
				if !ok {
					break
				}
				if directory != "" {
					b.directory = directory
				}
				b.variables += variables
				folded++
			}
			if folded > 0 {
				end := int(operators[folded-1].Offset())
				b.setup = formatBashCommand(command[:end]) + " &&"
				// Start after the operator, retaining comments before the next
				// command and all subsequent top-level statements verbatim.
				command = strings.TrimSpace(command[end+2:])
			}
		}
	}
	b.formatted = bashCommandFence(formatBashCommand(command))
	if file, err := parse(command); err == nil && len(file.Stmts) == 1 && compactBashPipeline(file.Stmts[0]) && len(file.Last) == 0 {
		var out bytes.Buffer
		if syntax.NewPrinter(syntax.SingleLine(true)).Print(&out, file) == nil {
			candidate := strings.TrimRight(out.String(), "\n")
			if !strings.Contains(candidate, "\n") {
				b.compact = candidate
			}
		}
	}
	return b
}

func plainSetupStatement(stmt *syntax.Stmt) bool {
	return !stmt.Negated && !stmt.Background && !stmt.Coprocess && len(stmt.Redirs) == 0
}

func bashSetupStatement(source string, stmt *syntax.Stmt) (directory string, variables int, ok bool) {
	if !plainSetupStatement(stmt) || len(stmt.Comments) > 0 {
		return "", 0, false
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.CallExpr:
		if len(cmd.Assigns) == 0 && len(cmd.Args) == 2 && cmd.Args[0].Lit() == "cd" && simpleSetupWord(cmd.Args[1]) {
			path := bashSource(source, cmd.Args[1])
			if !strings.HasPrefix(path, "-") {
				return path, 0, true
			}
		}
	case *syntax.DeclClause:
		if cmd.Variant.Value != "export" || len(cmd.Args) == 0 {
			break
		}
		for _, arg := range cmd.Args {
			if arg.Name == nil || arg.Naked || arg.Append || arg.Index != nil || arg.Array != nil || !simpleSetupWord(arg.Value) {
				return "", 0, false
			}
		}
		return "", len(cmd.Args), true
	}
	return "", 0, false
}

func simpleSetupWord(word *syntax.Word) bool {
	if word == nil {
		return true // an empty export value
	}
	ok := true
	syntax.Walk(word, func(node syntax.Node) bool {
		switch n := node.(type) {
		case nil, *syntax.Word, *syntax.Lit, *syntax.SglQuoted, *syntax.DblQuoted:
		case *syntax.ParamExp:
			if n.Index != nil || n.Exp != nil || n.Slice != nil || n.Repl != nil {
				ok = false
			}
		default:
			ok = false
		}
		return ok
	})
	return ok
}

func compactBashPipeline(stmt *syntax.Stmt) bool {
	if stmt.Background || stmt.Coprocess || len(stmt.Comments) > 0 {
		return false
	}
	for _, redirect := range stmt.Redirs {
		if redirect.Hdoc != nil {
			return false
		}
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.CallExpr:
		return true
	case *syntax.BinaryCmd:
		return (cmd.Op == syntax.Pipe || cmd.Op == syntax.PipeAll) && compactBashPipeline(cmd.X) && compactBashPipeline(cmd.Y)
	}
	return false
}

func bashCommandFence(command string) string {
	return strings.Join(markdown.RenderFence("command", markdown.HighlightCodeLines(command, "bash")), "\n")
}

func (b *bashInspectorCommand) commandAtWidth(width int) string {
	if b.compact != "" && rw.StringWidth(b.compact)+2 <= width {
		return bashCommandFence(b.compact)
	}
	return b.formatted
}

func (b *bashInspectorCommand) setupLabel(width int, expanded bool) string {
	glyph := "▸"
	if expanded {
		glyph = "▾"
	}
	suffix := ""
	if b.variables > 0 {
		suffix = fmt.Sprintf(" · %d variable", b.variables)
		if b.variables != 1 {
			suffix += "s"
		}
	}
	label := "setup"
	if b.directory != "" {
		path := bashSummaryLine(b.directory)
		parts := strings.Split(strings.TrimRight(path, "/"), "/")
		if len(parts) > 3 {
			path = "…/" + strings.Join(parts[len(parts)-2:], "/")
		}
		budget := width - rw.StringWidth(glyph+" "+label+" · "+suffix)
		if budget > 0 {
			label += " · " + fitToolPath(path, budget)
		}
	}
	return style.Styled(glyph, "accent", "bold") + " " + style.Styled(rw.Truncate(label+suffix, max(0, width-2), "…"), "muted", "")
}

func (m *replModel) setBashSetupExpanded(expanded bool) {
	if m.bashSetupExpanded != expanded {
		m.bashSetupExpanded = expanded
		m.visual.invalidate()
	}
}
