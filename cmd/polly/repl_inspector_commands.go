package main

import "strings"

const inspectorCommandUsage = "/inspect [tools|thoughts|close|prev|next|back|forward|find|maximize|wider|narrower]"

func registerInspectorCommands(r *replCommandRegistry) {
	r.register(replCommand{name: "/inspect", usage: inspectorCommandUsage, summary: "inspect a tool result or thought block", busySafe: true, run: func(ctx *replCommandContext, args []string) replCommandResult {
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
	case "close", "prev", "next", "back", "forward", "maximize", "wider", "narrower":
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
	case "tools", "thoughts":
	default:
		r.model.appendNoticeLine("usage: " + inspectorCommandUsage)
		return
	}
	t := tabViewTarget(r.visibleTab())
	s := r.model.inspections
	if arg == "thoughts" {
		if len(s.thoughts) == 0 {
			r.model.appendNoticeLine("no thoughts to inspect")
			return
		}
		t.kind, t.item = thoughtViewKind, s.thoughts[len(s.thoughts)-1].key
	} else {
		if len(s.tools) == 0 {
			r.model.appendNoticeLine("no tools to inspect")
			return
		}
		t.kind, t.item = toolViewKind, s.tools[len(s.tools)-1].key
	}
	r.inspect(t)
}
