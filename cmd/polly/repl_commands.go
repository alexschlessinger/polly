package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

type replCommandResult struct {
	quit bool
	err  error
}

type replCommandFunc func(*replCommandContext, []string) replCommandResult

type replCommandCompleteFunc func(*replCommandContext, []string, string) []string

type replCommand struct {
	name    string
	aliases []string
	usage   string
	summary string
	// busySafe commands run immediately while a turn is in flight instead of
	// queueing behind it; their output may interleave with streaming assistant
	// text. Reserve it for read-only inspection and queue management.
	busySafe bool
	run      replCommandFunc
	complete replCommandCompleteFunc
}

type replCommandRegistry struct {
	commands []replCommand
	byName   map[string]int
}

type replCommandContext struct {
	config   *Config
	state    *conversationState
	registry *replCommandRegistry
	reply    func(string) error

	// Interactive-only operations are callbacks so the command parser stays
	// independent of the managed REPL's queue and turn state. The fallback REPL
	// leaves them nil; handlers report that the operation is unavailable.
	clearTranscript   func() error
	resetConversation func() error
	// queueLines returns raw queued inputs in execution order (oldest first).
	queueLines func() []string
	// dropQueued removes the most recently queued (last) input.
	dropQueued    func() (string, bool)
	clearQueue    func() int
	continueQueue func() error
	retryTurn     func() error
	// settingsApplied lets the interactive REPL refresh UI derived from config
	// (e.g. the status-row model name) after /set mutates it.
	settingsApplied func()
}

var (
	defaultReplCommands = newDefaultReplCommandRegistry()
	slashCommands       = defaultReplCommands.commandNames()
)

func newDefaultReplCommandRegistry() *replCommandRegistry {
	r := newReplCommandRegistry()
	r.register(replCommand{
		name:     "/clear",
		usage:    "/clear",
		summary:  "clear the display (keep conversation history)",
		busySafe: true,
		run:      replClearCommand,
	})
	r.register(replCommand{
		name:     "/context",
		aliases:  []string{"/stats"},
		usage:    "/context",
		summary:  "session tokens, capacity, counts",
		busySafe: true,
		run:      replContextCommand,
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
		busySafe: true,
		run:      replGetCommand,
		complete: completeGetCommand,
	})
	r.register(replCommand{
		name:     "/help",
		usage:    "/help [command]",
		summary:  "show this help",
		busySafe: true,
		run:      replHelpCommand,
		complete: completeHelpCommand,
	})
	r.register(replCommand{
		name:     "/queue",
		usage:    "/queue [list|drop|clear|continue]",
		summary:  "inspect or manage queued input",
		busySafe: true,
		run:      replQueueCommand,
		complete: completeQueueCommand,
	})
	r.register(replCommand{
		name:    "/reset",
		usage:   "/reset confirm",
		summary: "clear durable conversation history",
		run:     replResetCommand,
	})
	r.register(replCommand{
		name:    "/retry",
		usage:   "/retry",
		summary: "retry the last failed or canceled turn",
		run:     replRetryCommand,
	})
	r.register(replCommand{
		name:     "/set",
		usage:    "/set <key> <value>",
		summary:  "change a setting for this session",
		run:      replSetCommand,
		complete: completeSetCommand,
	})
	r.register(replCommand{
		name:     "/skills",
		usage:    "/skills",
		summary:  "list loaded skills",
		busySafe: true,
		run:      replSkillsCommand,
	})
	r.register(replCommand{
		name:     "/tools",
		usage:    "/tools [list [namespace]|show <name>]",
		summary:  "inspect loaded tools",
		busySafe: true,
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
	idx, ok := r.byName[strings.ToLower(name)]
	if !ok {
		return replCommand{}, false
	}
	return r.commands[idx], true
}

// unknownCommandNotice builds the notice for input whose first field is not a
// registered command, suggesting the closest name for near misses.
func (r *replCommandRegistry) unknownCommandNotice(input string) string {
	name := input
	if fields := strings.Fields(input); len(fields) > 0 {
		name = fields[0]
	}
	if suggestion := r.closestCommand(name); suggestion != "" {
		return "unknown command: " + name + " — did you mean " + suggestion + "?"
	}
	return "unknown command: " + name + " (try /help)"
}

// closestCommand returns the registered command or alias nearest to name — a
// unique prefix extension, or the closest name within edit distance 2 — or ""
// when nothing is near enough to suggest. Suggestions are display-only; a near
// miss never dispatches.
func (r *replCommandRegistry) closestCommand(name string) string {
	name = strings.ToLower(name)
	names := r.commandNames()
	var prefixed []string
	for _, cand := range names {
		if strings.HasPrefix(cand, name) {
			prefixed = append(prefixed, cand)
		}
	}
	if len(prefixed) == 1 {
		return prefixed[0]
	}
	best, bestDist := "", 3
	for _, cand := range names {
		if d := editDistance(name, cand); d < bestDist {
			best, bestDist = cand, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between two short strings.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
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

// busySafeCommand reports whether input is a single-line command marked safe
// to run while a turn is in flight (instead of queueing behind it).
func (r *replCommandRegistry) busySafeCommand(input string) bool {
	if strings.Contains(input, "\n") {
		return false
	}
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return false
	}
	cmd, ok := r.get(fields[0])
	return ok && cmd.busySafe
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
		return []string{r.unknownCommandNotice(name)}
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
		"  Ctrl-C / Esc      interrupt turn (Ctrl-C twice to quit)",
		"  Up / Down         move line; recall history at top/bottom",
		"  Ctrl-R            reverse-search history",
		"  PgUp / PgDn       scroll transcript",
		"  Ctrl-A / Ctrl-E   line start / end",
		"  Delete / Ctrl-D   delete next char (Ctrl-D exits when empty)",
		"  Ctrl-U / Ctrl-K   clear before / after cursor",
		"  Ctrl-L            clear display",
		"  Ctrl-W            delete previous word",
		"  Alt-B / Alt-F     word left / right",
		"  Alt-D             delete next word",
		"  y approve · n/Enter/Esc deny · a approve all",
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
			r.model.clearDisplay()
			return nil
		},
		resetConversation: func() error {
			if r.state == nil || r.state.session == nil {
				return fmt.Errorf("no active session")
			}
			if err := r.state.session.Clear(); err != nil {
				// A queued reset is a barrier. If it fails, keep later prompts
				// paused against the old history instead of running them silently.
				if len(r.model.queue) > 0 {
					r.model.queuePaused = true
					r.model.updateQueueHint()
				}
				return err
			}
			r.model.clearDisplay()
			r.model.retryPrompt = ""
			r.model.lastOutcome = turnOutcomeNone
			r.model.lastIn = 0
			r.model.lastOut = 0
			r.model.lastElapsed = 0
			r.model.turnHasOutput = false
			r.model.unsavedLabeled = false
			return nil
		},
		queueLines: func() []string {
			return append([]string(nil), r.model.queue...)
		},
		dropQueued: func() (string, bool) {
			if len(r.model.queue) == 0 {
				return "", false
			}
			last := len(r.model.queue) - 1
			line := r.model.queue[last]
			r.model.queue = r.model.queue[:last]
			if len(r.model.queue) == 0 {
				r.model.queuePaused = false
				r.model.queueResumeAfterTurn = false
			}
			r.model.updateQueueHint()
			return line, true
		},
		clearQueue: func() int {
			count := len(r.model.queue)
			r.model.queue = nil
			r.model.queuePaused = false
			r.model.queueResumeAfterTurn = false
			r.model.updateQueueHint()
			return count
		},
		continueQueue: func() error {
			r.model.queuePaused = false
			if r.model.busy {
				r.model.queueResumeAfterTurn = true
			}
			r.model.updateQueueHint()
			return nil
		},
		retryTurn: func() error {
			m := r.model
			if m.busy {
				return fmt.Errorf("a turn is already running")
			}
			if m.retryPrompt == "" {
				return fmt.Errorf("no failed or canceled turn")
			}
			prompt := m.retryPrompt
			m.appendTurnSeparator()
			m.busy = true
			m.canceling = false
			m.state = turnStateWaiting
			m.runningTools = 0
			m.turnStarted = time.Now()
			m.currentPrompt = prompt
			m.turnHasOutput = false
			m.unsavedLabeled = false
			m.lastOutcome = turnOutcomeNone
			m.lastIn = 0
			m.lastOut = 0
			m.retryingNext = true
			m.followBottom = true
			select {
			case r.pending <- prompt:
				return nil
			default:
				m.busy = false
				m.state = turnStateIdle
				m.retryingNext = false
				return fmt.Errorf("turn queue is unavailable")
			}
		},
		settingsApplied: func() {
			r.model.modelName = stripProviderPrefix(cfg.Model)
		},
	}
}

func newWriterReplCommandContext(config *Config, state *conversationState, w io.Writer) *replCommandContext {
	ctx := &replCommandContext{
		config:   config,
		state:    state,
		registry: defaultReplCommands,
		reply: func(line string) error {
			_, err := fmt.Fprintln(w, line)
			return err
		},
	}
	if state != nil && state.session != nil {
		ctx.resetConversation = state.session.Clear
	}
	return ctx
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
		ctx = &replCommandContext{}
	}
	if ctx.registry == nil {
		ctx.registry = r
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
	// Completing an argument: the trailing partial field, or a fresh one right
	// after a space. Completers see all typed fields so they can complete
	// positionally (e.g. tool names only after "/tools show").
	cmd, okCmd := r.get(fields[0])
	if !okCmd || cmd.complete == nil {
		return "", nil, false
	}
	prefix := ""
	base := fields
	if !endsSpace {
		prefix = fields[len(fields)-1]
		base = fields[:len(fields)-1]
	}
	matches = cmd.complete(ctx, fields, prefix)
	if len(matches) == 0 {
		return "", nil, false
	}
	head := strings.Join(base, " ") + " "
	for i, match := range matches {
		matches[i] = head + match
	}
	return longestCommonPrefix(matches), matches, true
}

// completionArgPos returns which argument (1-based) of a command is being
// completed: fields holds the command name plus any typed fields, prefix the
// partial argument ("" right after a space).
func completionArgPos(fields []string, prefix string) int {
	if prefix != "" {
		return len(fields) - 1
	}
	return len(fields)
}

// slashHintSummaryMax caps how many name matches render with their summaries;
// above it the hint falls back to bare names so the line stays scannable.
const slashHintSummaryMax = 4

// hintFor returns the transient hint line for a composer in the middle of a
// slash command, or "" when the input isn't one. While the command name is
// being typed it lists the matching commands (with summaries once the field
// narrows); once a known command is entered it hints argument keywords via the
// command's completer, falling back to the usage string.
func (r *replCommandRegistry) hintFor(ctx *replCommandContext, input string) string {
	if !strings.HasPrefix(input, "/") || strings.ContainsAny(input, "\n\t") {
		return ""
	}
	if ctx == nil {
		ctx = &replCommandContext{}
	}
	if ctx.registry == nil {
		ctx.registry = r
	}
	endsSpace := strings.HasSuffix(input, " ")
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return ""
	}
	if len(fields) == 1 && !endsSpace {
		return r.nameHint(fields[0])
	}
	cmd, ok := r.get(fields[0])
	if !ok {
		return ""
	}
	if cmd.complete != nil {
		prefix := ""
		if !endsSpace {
			prefix = fields[len(fields)-1]
		}
		matches := cmd.complete(ctx, fields, prefix)
		// A single match the user has already fully typed is noise; show the
		// usage reminder instead.
		if len(matches) == 1 && matches[0] == prefix {
			matches = nil
		}
		if len(matches) > 0 {
			return strings.Join(matches, "  ")
		}
	}
	return "usage: " + cmd.usage
}

// nameHint lists commands (and aliases) matching a partial first field.
func (r *replCommandRegistry) nameHint(prefix string) string {
	type match struct{ name, summary string }
	var matches []match
	for _, cmd := range r.commands {
		for _, name := range append([]string{cmd.name}, cmd.aliases...) {
			if strings.HasPrefix(name, prefix) {
				matches = append(matches, match{name, cmd.summary})
			}
		}
	}
	if len(matches) == 0 {
		return ""
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].name < matches[j].name })
	parts := make([]string, len(matches))
	for i, m := range matches {
		if len(matches) > slashHintSummaryMax {
			parts[i] = m.name
		} else {
			parts[i] = m.name + " — " + m.summary
		}
	}
	return strings.Join(parts, "   ")
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
	if len(args) != 1 {
		return replCommandResult{err: ctx.replyLine("usage: /clear")}
	}
	if ctx == nil || ctx.clearTranscript == nil {
		return replCommandResult{err: ctx.replyLine("display clear unavailable")}
	}
	if err := ctx.clearTranscript(); err != nil {
		return replCommandResult{err: ctx.replyLine(fmt.Sprintf("failed to clear display: %v", err))}
	}
	return replCommandResult{err: ctx.replyLine("display cleared")}
}

func replResetCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) != 2 || args[1] != "confirm" {
		return replCommandResult{err: ctx.replyLine("reset clears durable conversation history; run /reset confirm")}
	}
	if ctx == nil || ctx.resetConversation == nil {
		return replCommandResult{err: ctx.replyLine("conversation reset unavailable")}
	}
	if err := ctx.resetConversation(); err != nil {
		return replCommandResult{err: ctx.replyLine(fmt.Sprintf("failed to reset conversation: %v", err))}
	}
	return replCommandResult{err: ctx.replyLine("conversation reset")}
}

func completeQueueCommand(_ *replCommandContext, fields []string, prefix string) []string {
	if completionArgPos(fields, prefix) != 1 {
		return nil
	}
	return matchingWords([]string{"list", "drop", "clear", "continue"}, prefix)
}

func replQueueCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) > 2 {
		return replCommandResult{err: ctx.replyLine("usage: /queue [list|drop|clear|continue]")}
	}
	action := "list"
	if len(args) == 2 {
		action = args[1]
	}

	switch action {
	case "list":
		if ctx == nil || ctx.queueLines == nil {
			return replCommandResult{err: ctx.replyLine("queue unavailable")}
		}
		lines := ctx.queueLines()
		if len(lines) == 0 {
			return replCommandResult{err: ctx.replyLine("queue empty")}
		}
		out := []string{fmt.Sprintf("queue (%d):", len(lines))}
		for i, line := range lines {
			out = append(out, fmt.Sprintf("  %d. %s", i+1, queueLinePreview(line)))
		}
		return replCommandResult{err: ctx.replyLines(out)}
	case "drop":
		if ctx == nil || ctx.dropQueued == nil {
			return replCommandResult{err: ctx.replyLine("queue drop unavailable")}
		}
		line, ok := ctx.dropQueued()
		if !ok {
			return replCommandResult{err: ctx.replyLine("queue empty")}
		}
		return replCommandResult{err: ctx.replyLine("dropped newest queued input: " + queueLinePreview(line))}
	case "clear":
		if ctx == nil || ctx.clearQueue == nil {
			return replCommandResult{err: ctx.replyLine("queue clear unavailable")}
		}
		count := ctx.clearQueue()
		if count == 0 {
			return replCommandResult{err: ctx.replyLine("queue already empty")}
		}
		return replCommandResult{err: ctx.replyLine(fmt.Sprintf("cleared %d queued input(s)", count))}
	case "continue":
		if ctx == nil || ctx.continueQueue == nil {
			return replCommandResult{err: ctx.replyLine("queue continue unavailable")}
		}
		if err := ctx.continueQueue(); err != nil {
			return replCommandResult{err: ctx.replyLine(fmt.Sprintf("failed to continue queue: %v", err))}
		}
		return replCommandResult{err: ctx.replyLine("queue continued")}
	default:
		return replCommandResult{err: ctx.replyLine("usage: /queue [list|drop|clear|continue]")}
	}
}

func queueLinePreview(line string) string {
	line = strings.TrimSpace(strings.ReplaceAll(line, "\n", " ↵ "))
	if line == "" {
		return "(blank)"
	}
	return truncate(line, 120)
}

func replRetryCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) != 1 {
		return replCommandResult{err: ctx.replyLine("usage: /retry")}
	}
	if ctx == nil || ctx.retryTurn == nil {
		return replCommandResult{err: ctx.replyLine("retry unavailable")}
	}
	if err := ctx.retryTurn(); err != nil {
		return replCommandResult{err: ctx.replyLine(fmt.Sprintf("failed to retry turn: %v", err))}
	}
	return replCommandResult{err: ctx.replyLine("retrying last turn")}
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

var replSettingKeys = []string{"model", "temp", "maxtokens", "maxcontext", "thinking", "system", "tooltimeout", "skilldir", "sandbox"}

func completeGetCommand(_ *replCommandContext, fields []string, prefix string) []string {
	if completionArgPos(fields, prefix) != 1 {
		return nil
	}
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
			v, _ := replSettingValue(ctx, k)
			lines = append(lines, fmt.Sprintf("  %s: %s", k, v))
		}
		return replCommandResult{err: ctx.replyLines(lines)}
	}
	value, ok := replSettingValue(ctx, key)
	if !ok {
		return replCommandResult{err: ctx.replyLine("unknown key: " + key + " (available: " + strings.Join(append([]string{"all"}, replSettingKeys...), ", ") + ")")}
	}
	return replCommandResult{err: ctx.replyLine(key + ": " + value)}
}

// replSettableKeys lists the /set-writable settings. system, skilldir, and
// sandbox stay launch-time only: the system prompt is embedded in session
// history at creation, and skill/sandbox wiring happens during tool loading.
var replSettableKeys = []string{"model", "temp", "maxtokens", "maxcontext", "thinking", "tooltimeout"}

// thinkingEffortWords are the named efforts accepted by llm.ParseThinkingEffort
// (a raw token budget is also accepted).
var thinkingEffortWords = []string{"off", "dynamic", "minimal", "low", "medium", "high", "xhigh", "max"}

func completeSetCommand(_ *replCommandContext, fields []string, prefix string) []string {
	switch completionArgPos(fields, prefix) {
	case 1:
		return matchingWords(replSettableKeys, prefix)
	case 2:
		if fields[1] == "thinking" {
			return matchingWords(thinkingEffortWords, prefix)
		}
	}
	return nil
}

// applyReplSetting validates value for key and writes it onto cfg. Turns build
// their completion request from cfg each time, so a change takes effect on the
// next turn without reconnecting.
func applyReplSetting(cfg *Config, key, value string) error {
	switch key {
	case "model":
		if value == "" {
			return fmt.Errorf("model requires a provider/model value")
		}
		if err := validateModel(value); err != nil {
			return err
		}
		cfg.Model = value
	case "temp":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("temp must be a number, got %q", value)
		}
		if err := validateTemperature(f); err != nil {
			return err
		}
		cfg.Temperature = f
	case "maxtokens":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return fmt.Errorf("maxtokens must be a positive integer, got %q", value)
		}
		cfg.MaxTokens = n
	case "maxcontext":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("maxcontext must be a non-negative integer (0 = unlimited), got %q", value)
		}
		cfg.MaxHistoryTokens = n
	case "thinking":
		if _, err := llm.ParseThinkingEffort(value); err != nil {
			return err
		}
		cfg.ThinkingEffort = value
	case "tooltimeout":
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return fmt.Errorf("tooltimeout must be a positive duration (e.g. 45s), got %q", value)
		}
		cfg.ToolTimeout = d
	default:
		return fmt.Errorf("unknown or read-only key: %s (settable: %s)", key, strings.Join(replSettableKeys, ", "))
	}
	return nil
}

func replSetCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) != 3 {
		return replCommandResult{err: ctx.replyLine("usage: /set <key> <value>. settable: " + strings.Join(replSettableKeys, ", "))}
	}
	if ctx == nil || ctx.config == nil {
		return replCommandResult{err: ctx.replyLine("settings unavailable")}
	}
	key, value := args[1], args[2]
	if err := applyReplSetting(ctx.config, key, value); err != nil {
		return replCommandResult{err: ctx.replyLine(err.Error())}
	}
	// The agent captures the tool timeout at construction; push the change
	// through so it applies to the next turn, not the next launch.
	if key == "tooltimeout" && ctx.state != nil && ctx.state.agent != nil {
		ctx.state.agent.SetToolTimeout(ctx.config.ToolTimeout)
	}
	if ctx.settingsApplied != nil {
		ctx.settingsApplied()
	}
	display, _ := replSettingValue(ctx, key)
	line := key + ": " + display
	if err := persistReplSettings(ctx); err != nil {
		line += " (applied for this run; persisting failed: " + err.Error() + ")"
	}
	return replCommandResult{err: ctx.replyLine(line)}
}

// persistReplSettings writes the resolved settings back to session metadata —
// the same fields updateContextInfo records at startup — so a /set survives
// into the next launch of this context.
func persistReplSettings(ctx *replCommandContext) error {
	if ctx.state == nil || ctx.state.session == nil {
		return nil
	}
	cfg := ctx.configOrDefault()
	s := ctx.state.session
	md := s.GetMetadata()
	if md == nil {
		md = &sessions.Metadata{}
	}
	md.Name = s.GetName()
	md.Model = cfg.Model
	md.Temperature = cfg.Temperature
	md.MaxTokens = cfg.MaxTokens
	md.MaxHistoryTokens = cfg.MaxHistoryTokens
	md.ThinkingEffort = cfg.ThinkingEffort
	md.ToolTimeout = cfg.ToolTimeout
	return s.SetMetadata(md)
}

// sandboxToolSplit partitions sandbox-capable tools by whether they run
// sandboxed. Tools that can't be sandboxed at all (skill helpers, function
// tools) are excluded.
func sandboxToolSplit(reg *tools.ToolRegistry) (sandboxed, unsandboxed []string) {
	for _, t := range reg.All() {
		capable, active := tools.SandboxState(t)
		if !capable {
			continue
		}
		if active {
			sandboxed = append(sandboxed, t.GetName())
		} else {
			unsandboxed = append(unsandboxed, t.GetName())
		}
	}
	sort.Strings(sandboxed)
	sort.Strings(unsandboxed)
	return sandboxed, unsandboxed
}

type sandboxPostureState int

const (
	sandboxPostureDisabled sandboxPostureState = iota
	sandboxPostureUnavailable
	sandboxPostureActive
)

type sandboxPosture struct {
	state       sandboxPostureState
	preset      string
	denyPaths   int
	sandboxed   []string
	unsandboxed []string
}

func currentSandboxPosture(config *Config, state *conversationState) sandboxPosture {
	cfg := config
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.NoSandbox {
		return sandboxPosture{state: sandboxPostureDisabled}
	}
	var reg *tools.ToolRegistry
	if state != nil {
		reg = state.toolRegistry
	}
	if reg == nil || !reg.HasSandbox() {
		return sandboxPosture{state: sandboxPostureUnavailable}
	}
	sandboxed, unsandboxed := sandboxToolSplit(reg)
	preset := cfg.SandboxPreset
	if preset == "" {
		preset = "base"
	}
	return sandboxPosture{
		state:       sandboxPostureActive,
		preset:      preset,
		denyPaths:   len(cfg.DenyPaths),
		sandboxed:   sandboxed,
		unsandboxed: unsandboxed,
	}
}

func sandboxPostureForContext(ctx *replCommandContext) sandboxPosture {
	if ctx == nil {
		return currentSandboxPosture(nil, nil)
	}
	return currentSandboxPosture(ctx.configOrDefault(), ctx.state)
}

func (p sandboxPosture) settingString() string {
	switch p.state {
	case sandboxPostureDisabled:
		return "disabled (--nosandbox)"
	case sandboxPostureUnavailable:
		return "unavailable (no backend)"
	default:
		line := fmt.Sprintf("active (preset: %s; denypaths: %d; tools: %d sandboxed, %d not", p.preset, p.denyPaths, len(p.sandboxed), len(p.unsandboxed))
		if len(p.unsandboxed) > 0 {
			line += ": " + strings.Join(p.unsandboxed, ", ")
		}
		return line + ")"
	}
}

func (p sandboxPosture) noticeString() string {
	switch p.state {
	case sandboxPostureDisabled:
		return "sandbox: disabled (--nosandbox)"
	case sandboxPostureUnavailable:
		return "sandbox: unavailable"
	default:
		line := fmt.Sprintf("sandbox: active (%s; %d tools sandboxed", p.preset, len(p.sandboxed))
		if len(p.unsandboxed) > 0 {
			line += "; not sandboxed: " + strings.Join(p.unsandboxed, ", ")
		}
		return line + ")"
	}
}

func replSettingValue(ctx *replCommandContext, key string) (string, bool) {
	config := ctx.configOrDefault()
	switch key {
	case "model":
		return config.Model, true
	case "temp":
		return fmt.Sprintf("%.2f", config.Temperature), true
	case "maxtokens":
		return fmt.Sprintf("%d", config.MaxTokens), true
	case "maxcontext":
		return fmt.Sprintf("%d", config.MaxHistoryTokens), true
	case "thinking":
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
	case "sandbox":
		return sandboxPostureForContext(ctx).settingString(), true
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

func loadedToolNames(ctx *replCommandContext) []string {
	if ctx == nil || ctx.state == nil || ctx.state.toolRegistry == nil {
		return nil
	}
	var names []string
	for _, t := range ctx.state.toolRegistry.All() {
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

func completeHelpCommand(ctx *replCommandContext, fields []string, prefix string) []string {
	if completionArgPos(fields, prefix) != 1 || ctx == nil || ctx.registry == nil {
		return nil
	}
	return matchingWords(ctx.registry.commandNames(), prefix)
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

func sandboxListBadge(info tools.SandboxInfo) string {
	if !info.Capable {
		return ""
	}
	if !info.Active {
		if info.OptedOut {
			return "[not sandboxed: opted out]"
		}
		return "[not sandboxed]"
	}
	if info.Config == nil {
		return "[sandboxed]"
	}
	return "[sandboxed: " + sandboxCompactSummary(info) + "]"
}

func sandboxCompactSummary(info tools.SandboxInfo) string {
	cfg := info.Config
	if cfg == nil {
		return ""
	}
	parts := []string{"net off"}
	if cfg.AllowNetwork {
		parts[0] = "net on"
		if cfg.DenyDNS {
			parts = append(parts, "dns off")
		}
	}
	switch {
	case cfg.DenyWrite:
		parts = append(parts, "read-only")
	case hasCustomWritablePaths(cfg.WritablePaths):
		parts = append(parts, "temp+custom writes")
	default:
		parts = append(parts, "temp writes")
	}
	if len(cfg.AllowEnv) > 0 {
		parts = append(parts, "env allowlist")
	} else {
		parts = append(parts, "env filtered")
	}
	return strings.Join(parts, ", ")
}

func sandboxShowDetail(info tools.SandboxInfo) string {
	if !info.Capable {
		return ""
	}
	if !info.Active {
		if info.OptedOut {
			return "not sandboxed (opted out)"
		}
		return "not sandboxed"
	}
	cfg := info.Config
	if cfg == nil {
		return "active (details unavailable)"
	}
	parts := []string{"network off"}
	if cfg.AllowNetwork {
		parts[0] = "network on"
		if cfg.DenyDNS {
			parts = append(parts, "DNS off")
		}
	}
	switch {
	case cfg.DenyWrite:
		parts = append(parts, "writes denied")
	case hasCustomWritablePaths(cfg.WritablePaths):
		parts = append(parts, "writes limited to temp and custom paths")
	default:
		parts = append(parts, "writes limited to temp")
	}
	if len(cfg.AllowEnv) > 0 {
		parts = append(parts, "env allowlist active")
	} else {
		parts = append(parts, "env filters credential-like variables")
	}
	return strings.Join(parts, "; ")
}

func hasCustomWritablePaths(paths []string) bool {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if !isTempWritablePath(p) {
			return true
		}
	}
	return false
}

func isTempWritablePath(path string) bool {
	clean := filepath.Clean(path)
	for _, candidate := range []string{os.TempDir(), "/tmp", "/private/tmp"} {
		if candidate == "" {
			continue
		}
		candidate = filepath.Clean(candidate)
		if clean == candidate {
			return true
		}
		// Effective sandbox configs are prepared before tools are loaded, which
		// canonicalizes writable grants. Match the canonical form of known temp
		// roots too (notably /var/... -> /private/var/... on macOS) without
		// resolving arbitrary writable paths during display.
		if real, err := filepath.EvalSymlinks(candidate); err == nil && clean == filepath.Clean(real) {
			return true
		}
	}
	return false
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
