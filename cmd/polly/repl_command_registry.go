package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
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
	// busySafeWhen, when set, decides per invocation: /set shows settings
	// mid-turn but queues a change behind it.
	busySafeWhen func(args []string) bool
	run          replCommandFunc
	complete     replCommandCompleteFunc
}

type replCommandRegistry struct {
	commands []replCommand
	byName   map[string]int
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
	if !ok {
		return false
	}
	if cmd.busySafeWhen != nil {
		return cmd.busySafeWhen(fields)
	}
	return cmd.busySafe
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

// helpLines is the plain /help text: the commands, then the keys by task.
func (r *replCommandRegistry) helpLines() []string {
	return r.helpLinesStyled(false)
}

// helpLinesStyled renders /help; with markup, names and keys keep the text
// color while their descriptions are muted, so the eye can scan one column.
func (r *replCommandRegistry) helpLinesStyled(markup bool) []string {
	row := func(name, desc string, width int) string {
		name = fmt.Sprintf("%-*s", width, name)
		if markup {
			return "  " + style.Escape(name) + "  " + style.Styled(desc, "muted", "")
		}
		return "  " + name + "  " + desc
	}
	// Help is a reference list, so commands sort by name rather than by
	// registration order.
	commands := append([]replCommand(nil), r.commands...)
	sort.Slice(commands, func(i, j int) bool { return commands[i].name < commands[j].name })
	width := 0
	names := make([]string, 0, len(commands))
	for _, cmd := range commands {
		name := strings.Join(append([]string{cmd.name}, cmd.aliases...), ", ")
		width = max(width, len(name))
		names = append(names, name)
	}
	var lines []string
	for n, cmd := range commands {
		lines = append(lines, row(names[n], cmd.summary, width))
	}
	width = 0
	for _, g := range keyTable {
		for _, k := range g.helpRows() {
			width = max(width, len(k.key))
		}
	}
	for _, g := range keyTable {
		title := g.title
		if markup {
			title = style.Styled(title, "", "bold")
		}
		lines = append(lines, "", title)
		for _, k := range g.helpRows() {
			lines = append(lines, row(k.key, k.desc, width))
		}
	}
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
