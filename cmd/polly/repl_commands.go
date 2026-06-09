package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/alexschlessinger/pollytool/tools"
)

type replCommandResult struct {
	quit bool
	err  error
}

type replCommandFunc func(*replCommandContext, []string) replCommandResult

type replCommandCompleteFunc func(*replCommandContext, []string, string) []string

type replCommand struct {
	name     string
	aliases  []string
	usage    string
	summary  string
	run      replCommandFunc
	complete replCommandCompleteFunc
}

type replCommandRegistry struct {
	commands []replCommand
	byName   map[string]int
}

type replCommandContext struct {
	config          *Config
	state           *conversationState
	registry        *replCommandRegistry
	reply           func(string) error
	clearTranscript func() error
}

var (
	defaultReplCommands = newDefaultReplCommandRegistry()
	slashCommands       = defaultReplCommands.commandNames()
)

func newDefaultReplCommandRegistry() *replCommandRegistry {
	r := newReplCommandRegistry()
	r.register(replCommand{
		name:    "/clear",
		usage:   "/clear",
		summary: "clear conversation history",
		run:     replClearCommand,
	})
	r.register(replCommand{
		name:    "/context",
		aliases: []string{"/stats"},
		usage:   "/context",
		summary: "session tokens, capacity, counts",
		run:     replContextCommand,
	})
	r.register(replCommand{
		name:    "/exit",
		aliases: []string{"/quit"},
		usage:   "/exit",
		summary: "leave the REPL",
		run: func(*replCommandContext, []string) replCommandResult {
			return replCommandResult{quit: true}
		},
	})
	r.register(replCommand{
		name:     "/get",
		usage:    "/get <key|all>",
		summary:  "show effective settings",
		run:      replGetCommand,
		complete: completeGetCommand,
	})
	r.register(replCommand{
		name:    "/help",
		usage:   "/help [command]",
		summary: "show this help",
		run:     replHelpCommand,
	})
	r.register(replCommand{
		name:    "/skills",
		usage:   "/skills",
		summary: "list loaded skills",
		run:     replSkillsCommand,
	})
	r.register(replCommand{
		name:     "/tools",
		usage:    "/tools [list [namespace]|show <name>]",
		summary:  "inspect loaded tools",
		run:      replToolsCommand,
		complete: completeToolsCommand,
	})
	return r
}

func newReplCommandRegistry() *replCommandRegistry {
	return &replCommandRegistry{byName: make(map[string]int)}
}

func (r *replCommandRegistry) register(cmd replCommand) {
	idx := len(r.commands)
	r.commands = append(r.commands, cmd)
	r.byName[cmd.name] = idx
	for _, alias := range cmd.aliases {
		r.byName[alias] = idx
	}
}

func (r *replCommandRegistry) get(name string) (replCommand, bool) {
	idx, ok := r.byName[name]
	if !ok {
		return replCommand{}, false
	}
	return r.commands[idx], true
}

func (r *replCommandRegistry) dispatch(line string, ctx *replCommandContext) (handled, quit bool, err error) {
	args := strings.Fields(strings.TrimSpace(line))
	if len(args) == 0 {
		return false, false, nil
	}
	cmd, ok := r.get(args[0])
	if !ok {
		return false, false, nil
	}
	if ctx == nil {
		ctx = &replCommandContext{}
	}
	if ctx.registry == nil {
		ctx.registry = r
	}
	res := cmd.run(ctx, args)
	return true, res.quit, res.err
}

func (r *replCommandRegistry) commandNames() []string {
	var names []string
	for _, cmd := range r.commands {
		names = append(names, cmd.name)
		names = append(names, cmd.aliases...)
	}
	sort.Strings(names)
	return names
}

func (r *replCommandRegistry) helpLines() []string {
	lines := []string{"commands:"}
	type row struct {
		names   string
		summary string
	}
	rows := make([]row, 0, len(r.commands))
	width := 0
	for _, cmd := range r.commands {
		names := strings.Join(append([]string{cmd.name}, cmd.aliases...), ", ")
		if len(names) > width {
			width = len(names)
		}
		rows = append(rows, row{names: names, summary: cmd.summary})
	}
	for _, row := range rows {
		lines = append(lines, fmt.Sprintf("  %-*s  %s", width, row.names, row.summary))
	}
	lines = append(lines, keyHelpLines()...)
	return lines
}

func (r *replCommandRegistry) helpFor(name string) []string {
	cmd, ok := r.get(name)
	if !ok {
		return []string{"unknown command: " + name + " (try /help)"}
	}
	names := append([]string{cmd.name}, cmd.aliases...)
	return []string{
		"usage: " + cmd.usage,
		"aliases: " + strings.Join(names, ", "),
		"summary: " + cmd.summary,
	}
}

func keyHelpLines() []string {
	return []string{
		"keys:",
		"  Enter             send message",
		"  Ctrl-J            newline (multi-line input)",
		"  Type /            list commands; Tab completes",
		"  Ctrl-C            interrupt turn (twice to quit)",
		"  Up / Down         move line; recall history at top/bottom",
		"  Ctrl-R            reverse-search history",
		"  PgUp / PgDn       scroll transcript",
		"  Ctrl-A / Ctrl-E   line start / end",
		"  Ctrl-U / Ctrl-K   clear before / after cursor",
		"  Ctrl-W            delete previous word",
		"  Alt-B / Alt-F     word left / right",
		"  Alt-D             delete next word",
		"  y / n / a         answer approval prompts",
	}
}

func (ctx *replCommandContext) configOrDefault() *Config {
	if ctx != nil && ctx.config != nil {
		return ctx.config
	}
	return &Config{}
}

func (ctx *replCommandContext) replyLine(line string) error {
	if ctx != nil && ctx.reply != nil {
		return ctx.reply(line)
	}
	return nil
}

func (ctx *replCommandContext) replyLines(lines []string) error {
	for _, line := range lines {
		if err := ctx.replyLine(line); err != nil {
			return err
		}
	}
	return nil
}

func newManagedReplCommandContext(r *managedREPL) *replCommandContext {
	cfg := &Config{}
	if r.config != nil {
		cfg = r.config
	}
	return &replCommandContext{
		config:   cfg,
		state:    r.state,
		registry: defaultReplCommands,
		reply: func(line string) error {
			r.model.appendNoticeLine(line)
			return nil
		},
		clearTranscript: func() error {
			r.model.transcript = nil
			r.model.currentAssistant = -1
			r.model.activeTools = nil
			r.model.runningTools = 0
			r.model.stream.Reset()
			r.model.invalidateFlat()
			r.model.appendNoticeLine("context cleared")
			return nil
		},
	}
}

func newWriterReplCommandContext(config *Config, state *conversationState, w io.Writer) *replCommandContext {
	return &replCommandContext{
		config:   config,
		state:    state,
		registry: defaultReplCommands,
		reply: func(line string) error {
			_, err := fmt.Fprintln(w, line)
			return err
		},
	}
}

func helpLines() []string {
	return defaultReplCommands.helpLines()
}

func (m *replModel) appendHelp() {
	for _, line := range helpLines() {
		m.appendNoticeLine(line)
	}
}

func (r *managedREPL) runCommand(line string) (handled, quit bool) {
	handled, quit, err := defaultReplCommands.dispatch(line, newManagedReplCommandContext(r))
	if err != nil {
		r.model.appendNoticeLine("Error: " + err.Error())
		return true, false
	}
	return handled, quit
}

func completeSlash(input string) (completed string, matches []string, ok bool) {
	return defaultReplCommands.complete(input, nil)
}

func (r *replCommandRegistry) complete(input string, ctx *replCommandContext) (completed string, matches []string, ok bool) {
	if !strings.HasPrefix(input, "/") || strings.Contains(input, "\t") {
		return "", nil, false
	}
	if ctx == nil {
		ctx = &replCommandContext{registry: r}
	}
	endsSpace := strings.HasSuffix(input, " ")
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return "", nil, false
	}
	if len(fields) == 1 && !endsSpace {
		for _, name := range r.commandNames() {
			if strings.HasPrefix(name, input) {
				matches = append(matches, name)
			}
		}
		if len(matches) == 0 {
			return "", nil, false
		}
		return longestCommonPrefix(matches), matches, true
	}
	if len(fields) > 2 || (len(fields) == 1 && !endsSpace) {
		return "", nil, false
	}
	cmd, ok := r.get(fields[0])
	if !ok || cmd.complete == nil {
		return "", nil, false
	}
	prefix := ""
	if !endsSpace && len(fields) == 2 {
		prefix = fields[1]
	}
	matches = cmd.complete(ctx, fields, prefix)
	if len(matches) == 0 {
		return "", nil, false
	}
	for i, match := range matches {
		matches[i] = fields[0] + " " + match
	}
	return longestCommonPrefix(matches), matches, true
}

func longestCommonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	prefix := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	return prefix
}

func replHelpCommand(ctx *replCommandContext, args []string) replCommandResult {
	if ctx == nil || ctx.registry == nil {
		return replCommandResult{}
	}
	if len(args) > 1 {
		return replCommandResult{err: ctx.replyLines(ctx.registry.helpFor(args[1]))}
	}
	return replCommandResult{err: ctx.replyLines(ctx.registry.helpLines())}
}

func replClearCommand(ctx *replCommandContext, args []string) replCommandResult {
	if ctx != nil && ctx.state != nil && ctx.state.session != nil {
		if err := ctx.state.session.Clear(); err != nil {
			return replCommandResult{err: ctx.replyLine(fmt.Sprintf("failed to clear context: %v", err))}
		}
	}
	if ctx != nil && ctx.clearTranscript != nil {
		return replCommandResult{err: ctx.clearTranscript()}
	}
	return replCommandResult{err: ctx.replyLine("context cleared")}
}

func replContextCommand(ctx *replCommandContext, args []string) replCommandResult {
	if ctx == nil || ctx.state == nil || ctx.state.session == nil {
		return replCommandResult{err: ctx.replyLine("no active session")}
	}
	cfg := ctx.configOrDefault()
	s := ctx.state.session
	lines := []string{"context: " + s.GetName()}
	if cfg.Model != "" {
		lines = append(lines, "model: "+stripProviderPrefix(cfg.Model))
	}
	if pct := s.GetCapacityPercentage(); pct > 0 {
		lines = append(lines, fmt.Sprintf("tokens: %s (%.0f%% of capacity)", humanizeTokens(s.GetTotalTokens()), pct))
	} else {
		lines = append(lines, "tokens: "+humanizeTokens(s.GetTotalTokens()))
	}
	c := s.GetMessageCounts()
	lines = append(lines, fmt.Sprintf("messages: user %d · assistant %d · tool %d · system %d",
		c["user"], c["assistant"], c["tool"], c["system"]))
	lines = append(lines, fmt.Sprintf("tool calls: %d", s.GetToolCallCount()))
	return replCommandResult{err: ctx.replyLines(lines)}
}

var replSettingKeys = []string{"model", "temp", "maxtokens", "maxcontext", "thinkingeffort", "system", "tooltimeout", "skilldir"}

func completeGetCommand(_ *replCommandContext, _ []string, prefix string) []string {
	return matchingWords(append([]string{"all"}, replSettingKeys...), prefix)
}

func replGetCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) < 2 {
		return replCommandResult{err: ctx.replyLine("usage: /get <key|all>. available: " + strings.Join(append([]string{"all"}, replSettingKeys...), ", "))}
	}
	key := args[1]
	if key == "all" {
		lines := []string{"settings:"}
		for _, k := range replSettingKeys {
			v, _ := replSettingValue(ctx.configOrDefault(), k)
			lines = append(lines, fmt.Sprintf("  %s: %s", k, v))
		}
		return replCommandResult{err: ctx.replyLines(lines)}
	}
	value, ok := replSettingValue(ctx.configOrDefault(), key)
	if !ok {
		return replCommandResult{err: ctx.replyLine("unknown key: " + key + " (available: " + strings.Join(append([]string{"all"}, replSettingKeys...), ", ") + ")")}
	}
	return replCommandResult{err: ctx.replyLine(key + ": " + value)}
}

func replSettingValue(config *Config, key string) (string, bool) {
	switch key {
	case "model":
		return config.Model, true
	case "temp":
		return fmt.Sprintf("%.2f", config.Temperature), true
	case "maxtokens":
		return fmt.Sprintf("%d", config.MaxTokens), true
	case "maxcontext":
		return fmt.Sprintf("%d", config.MaxHistoryTokens), true
	case "thinkingeffort":
		return config.ThinkingEffort, true
	case "system":
		return config.SystemPrompt, true
	case "tooltimeout":
		return config.ToolTimeout.String(), true
	case "skilldir":
		if len(config.SkillDirs) == 0 {
			return "[]", true
		}
		return strings.Join(config.SkillDirs, ", "), true
	default:
		return "", false
	}
}

func replToolsCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) == 1 {
		return replListTools(ctx, "")
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

func completeToolsCommand(_ *replCommandContext, _ []string, prefix string) []string {
	return matchingWords([]string{"list", "show"}, prefix)
}

func replListTools(ctx *replCommandContext, namespace string) replCommandResult {
	var all []tools.Tool
	if ctx != nil && ctx.state != nil && ctx.state.toolRegistry != nil {
		all = ctx.state.toolRegistry.All()
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
	if ctx == nil || ctx.state == nil || ctx.state.toolRegistry == nil {
		return replCommandResult{err: ctx.replyLine("tool not found: " + name)}
	}
	tool, ok := ctx.state.toolRegistry.Get(name)
	if !ok {
		return replCommandResult{err: ctx.replyLine("tool not found: " + name)}
	}
	schema := tool.GetSchema()
	lines := []string{
		"name: " + tool.GetName(),
		"type: " + tool.GetType(),
		"source: " + tool.GetSource(),
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

func replSkillsCommand(ctx *replCommandContext, args []string) replCommandResult {
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
		return replCommandResult{err: ctx.replyLine("no skills loaded")}
	}
	return replCommandResult{err: ctx.replyLines(append([]string{fmt.Sprintf("skills (%d):", len(list))}, list...))}
}

func matchingWords(words []string, prefix string) []string {
	var matches []string
	for _, word := range words {
		if strings.HasPrefix(word, prefix) {
			matches = append(matches, word)
		}
	}
	sort.Strings(matches)
	return matches
}
