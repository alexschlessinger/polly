package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/alexschlessinger/pollytool/tools"
)

func replToolsCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) == 1 {
		res := replListTools(ctx, "")
		if res.err == nil {
			if skills := skillsSection(ctx); len(skills) > 0 {
				res.err = ctx.replyLines(skills)
			}
		}
		return res
	}
	switch args[1] {
	case "list":
		namespace := ""
		if len(args) > 2 {
			namespace = args[2]
		}
		return replListTools(ctx, namespace)
	case "show":
		if len(args) < 3 {
			return replCommandResult{err: ctx.replyLine("usage: /tools show <name>")}
		}
		return replShowTool(ctx, args[2])
	default:
		return replCommandResult{err: ctx.replyLine("usage: /tools [list [namespace]|show <name>]")}
	}
}

func completeToolsCommand(ctx *replCommandContext, fields []string, prefix string) []string {
	switch completionArgPos(fields, prefix) {
	case 1:
		return matchingWords([]string{"list", "show"}, prefix)
	case 2:
		switch fields[1] {
		case "show":
			return matchingWords(loadedToolNames(ctx), prefix)
		case "list":
			return matchingWords(loadedToolNamespaces(ctx), prefix)
		}
	}
	return nil
}

// tools is the conversation's loaded registry, or nil outside one.
func (ctx *replCommandContext) tools() *tools.ToolRegistry {
	if ctx == nil || ctx.state == nil {
		return nil
	}
	return ctx.state.effectiveTools()
}

func loadedToolNames(ctx *replCommandContext) []string {
	reg := ctx.tools()
	if reg == nil {
		return nil
	}
	var names []string
	for _, t := range reg.All() {
		names = append(names, t.GetName())
	}
	return names
}

func loadedToolNamespaces(ctx *replCommandContext) []string {
	seen := make(map[string]bool)
	var namespaces []string
	for _, name := range loadedToolNames(ctx) {
		if ns, _, ok := strings.Cut(name, "__"); ok && !seen[ns] {
			seen[ns] = true
			namespaces = append(namespaces, ns)
		}
	}
	return namespaces
}

func replListTools(ctx *replCommandContext, namespace string) replCommandResult {
	var all []tools.Tool
	if reg := ctx.tools(); reg != nil {
		all = reg.All()
	}
	if len(all) == 0 {
		return replCommandResult{err: ctx.replyLine("no tools loaded")}
	}
	var names []string
	for _, t := range all {
		name := t.GetName()
		if namespace != "" && !strings.HasPrefix(name, namespace+"__") {
			continue
		}
		if badge := sandboxListBadge(tools.SandboxDetails(t)); badge != "" {
			name += " " + badge
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return replCommandResult{err: ctx.replyLine("no tools in namespace: " + namespace)}
	}
	lines := []string{fmt.Sprintf("tools (%d):", len(names))}
	for _, name := range names {
		lines = append(lines, "  "+name)
	}
	return replCommandResult{err: ctx.replyLines(lines)}
}

func replShowTool(ctx *replCommandContext, name string) replCommandResult {
	reg := ctx.tools()
	if reg == nil {
		return replCommandResult{err: ctx.replyLine("tool not found: " + name)}
	}
	tool, ok := reg.Get(name)
	if !ok {
		return replCommandResult{err: ctx.replyLine("tool not found: " + name)}
	}
	schema := tool.GetSchema()
	lines := []string{
		"name: " + tool.GetName(),
		"type: " + tool.GetType(),
		"source: " + tool.GetSource(),
	}
	if info := tools.SandboxDetails(tool); info.Capable {
		lines = append(lines, fmt.Sprintf("sandboxed: %t", info.Active))
		if detail := sandboxShowDetail(info); detail != "" {
			lines = append(lines, "sandbox: "+detail)
		}
	}
	if desc := schema.Description(); desc != "" {
		lines = append(lines, "description: "+desc)
	}
	required := schema.Required()
	if len(required) == 0 {
		lines = append(lines, "required: []")
	} else {
		lines = append(lines, "required: "+strings.Join(required, ", "))
	}
	return replCommandResult{err: ctx.replyLines(lines)}
}

// skillsSection lists the loaded skills under /tools; nothing when none.
func skillsSection(ctx *replCommandContext) []string {
	var list []string
	if ctx != nil && ctx.state != nil && ctx.state.skillCatalog != nil {
		for _, s := range ctx.state.skillCatalog.List() {
			line := "  " + s.Name
			if s.Description != "" {
				line += " — " + s.Description
			}
			list = append(list, line)
		}
	}
	if len(list) == 0 {
		return nil
	}
	return append([]string{fmt.Sprintf("skills (%d):", len(list))}, list...)
}
