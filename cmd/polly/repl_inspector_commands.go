package main

import "strings"

const inspectorCommandUsage = "/inspect [tools|thoughts|changes|find|maximize]"

func registerInspectorCommands(r *replCommandRegistry) {
	r.register(replCommand{name: "/inspect", usage: inspectorCommandUsage, summary: "inspect conversation tools, changes, or a thought block", busySafe: true, complete: func(_ *replCommandContext, fields []string, prefix string) []string {
		if completionArgPos(fields, prefix) != 1 {
			return nil
		}
		return matchingWords([]string{"changes", "find", "maximize", "thoughts", "tools"}, prefix)
	}, run: func(ctx *replCommandContext, args []string) replCommandResult {
		if ctx.inspectView == nil {
			return replCommandResult{err: ctx.replyLine("inspector is available only in the managed TUI")}
		}
		ctx.inspectView(strings.Join(args[1:], " "))
		return replCommandResult{}
	}})
}

func (r *managedREPL) inspectCommand(arg string) {
	w := r.workspace()
	i := &w.inspector
	switch arg {
	case "find":
		if !i.open {
			r.inspectCommand("")
		}
		if i.open {
			r.inspectorAction("find")
		}
		return
	case "maximize":
		r.inspectorAction(arg)
		return
	case "":
		if len(i.history) > 0 {
			if !i.open {
				i.open = true
				i.generation++
			}
			return
		}
	case "tools", "thoughts", "changes":
	default:
		r.model.appendNoticeLine("usage: " + inspectorCommandUsage)
		return
	}
	if arg == "changes" {
		if _, _, files := sessionChangeStats(r.model.inspections.tools); files == 0 {
			r.model.appendNoticeLine("No file changes to inspect")
			return
		}
		r.openChangesInspector()
		return
	}
	t := tabViewTarget(r.visibleTab())
	s := r.model.inspections
	if arg == "thoughts" {
		if len(s.thoughts) == 0 {
			r.model.appendNoticeLine("No thoughts to inspect")
			return
		}
		t.kind, t.item = thoughtViewKind, s.thoughts[len(s.thoughts)-1].key
	} else {
		if len(s.tools) == 0 {
			r.model.appendNoticeLine("No tools to inspect")
			return
		}
		t.kind, t.item = toolViewKind, s.tools[len(s.tools)-1].key
	}
	r.inspect(t)
}
