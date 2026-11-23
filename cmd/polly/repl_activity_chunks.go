package main

import (
	"cmp"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

// Chunks: a line's activity as the runs a reader folds.
//
// A step, one model response, says when something happened. A chunk says what
// folds together: adjacent calls until the kind of work changes. Three kinds
// of work end a run: an edit, a test and an agent launch. Reads, commands,
// viewed images and every other tool share one, because a turn that looks
// around alternates between them and a row per call would fold nothing.
// Calls requested together stay together: a batch joins the run of its
// weightiest call, and the run's header counts what came with it. A thought
// goes with the calls its response made, at the start of their chunk; one
// that led no call stands alone as a row of its own. Chunks are built from
// records alone, before any width or fold is known, so a live and a replayed
// transcript fold the same way.

func callIdentity(row toolDisclosureRow) string {
	if row.displayID != "" {
		return row.displayID
	}
	// Older providers reused IDs across batches, even within one turn. The
	// display catalog gives those rows an ordinal as well as their raw ID.
	if row.sectionKey != "" {
		return row.sectionKey
	}
	if row.callID != "" {
		return row.callID
	}
	if row.agent != nil && row.agent.viewID != "" {
		return "agent:" + row.agent.viewID
	}
	return ""
}

// Recognize literal test invocations, not an occurrence of a test command in
// a pipeline, script, argument or quoted string. In particular -c, -list,
// --help and --version do not run tests. Everything else remains a command.
var directTestCommand = regexp.MustCompile(`^(go test( (-v|-race|-count=1|[a-zA-Z0-9_./][a-zA-Z0-9_./-]*))*|cargo test|npm test|pytest( [a-zA-Z0-9_./][a-zA-Z0-9_./:-]*)*|python3? -m pytest)$`)

func isTestCommand(command string) bool {
	return len(command) <= 4096 && directTestCommand.MatchString(strings.Trim(command, " \t"))
}

type callAction string

const (
	actionRead  callAction = "read"
	actionEdit  callAction = "edit"
	actionView  callAction = "image view"
	actionTest  callAction = "test run"
	actionShell callAction = "command"
	actionTool  callAction = "tool"
	actionAgent callAction = "agent"
)

func callActionOf(r *toolDisclosureRow) callAction {
	switch {
	case r.isAgent():
		return actionAgent
	case len(r.inspectionImages) > 0 || r.toolName == "view_image":
		return actionView
	case r.testCommand:
		return actionTest
	}
	switch compactToolName(r.toolName) {
	case "read":
		return actionRead
	case "edit", "write":
		return actionEdit
	case "bash":
		return actionShell
	}
	return actionTool
}

// chunkGroup is which calls may share a run.
type chunkGroup uint8

const (
	chunkShared  chunkGroup = iota // reads, commands, viewed images, every other tool
	chunkEdit                      // edits and writes, and what was asked with them
	chunkTest                      // test runs asked on their own
	chunkAgent                     // agent launches, and what was asked with them
	chunkThought                   // a thought with no run to join
)

func chunkGroupOf(a callAction) chunkGroup {
	switch a {
	case actionEdit:
		return chunkEdit
	case actionTest:
		return chunkTest
	case actionAgent:
		return chunkAgent
	}
	return chunkShared
}

// batchGroup is the run one request batch belongs to: rows of record, one
// model response. Calls requested together ran together, so a batch is never
// split between runs and never reads as a sequence: it joins the run of its
// weightiest call. An agent launch outweighs an edit, and an edit everything
// else. A test is a run of its own only when nothing but tests was asked with
// it; beside other calls it is one more command. A call with members listed
// under it launched them, so it is an agent launch whatever its tool is
// called.
func batchGroup(record *toolDisclosureRecord, rows []int, launched map[int][]int) chunkGroup {
	group, tests := chunkShared, 0
	for _, i := range rows {
		if len(launched[i]) > 0 {
			return chunkAgent
		}
		switch chunkGroupOf(callActionOf(&record.rows[i])) {
		case chunkAgent:
			return chunkAgent
		case chunkEdit:
			group = chunkEdit
		case chunkTest:
			tests++
		}
	}
	if group == chunkShared && tests == len(rows) {
		return chunkTest
	}
	return group
}

// launchedMembers maps a call's row to the rows of the workflow members that
// call launched. The swarm reports a member whenever it starts, which can be
// several batches after its launch; it is listed with the launch all the
// same, so a workflow reads as one run under one heading.
func launchedMembers(record *toolDisclosureRecord) map[int][]int {
	var calls map[string]int
	var launched map[int][]int
	for i := range record.rows {
		row := &record.rows[i]
		if !row.isProjectedAgent() || row.agent.launch == "" {
			continue
		}
		if calls == nil {
			calls = map[string]int{}
			for j := range record.rows {
				call := &record.rows[j]
				if _, seen := calls[call.callID]; call.callID != "" && !call.isProjectedAgent() && !seen {
					calls[call.callID] = j
				}
			}
			launched = map[int][]int{}
		}
		if at, ok := calls[row.agent.launch]; ok {
			launched[at] = append(launched[at], i)
		}
	}
	return launched
}

// chunkPart is one row of an open chunk, in the order the turn made it: a
// step's thought, then the calls of its batch. A thought no call followed is
// the last row of the run before it.
type chunkPart struct {
	thought *reasoningRecord // nil for a call
	segment int
	tools   *toolDisclosureRecord
	row     int // index in tools.rows
	ordinal int // index among the record's ordinary rows; -1 for an agent
	step    int
	// launch marks the call that launched the workflow members listed after
	// it: the run's agents speak for it, so it is not counted beside them.
	launch bool
}

// activityChunk is one fold: a header that says what a run did, and the
// thoughts and calls behind it.
type activityChunk struct {
	// key names the chunk by its first call, or by its thought when it has
	// none, so a fold chosen by hand follows the evidence rather than a
	// label, a count or a screen row.
	key   string
	group chunkGroup
	kind  activityKind
	parts []chunkPart
	calls int
	// latest is the first call of the newest batch: the identity a live
	// label hands off when the next call takes its place.
	latest string
	// label is what the header says, before it is fit to a row. command,
	// when set, is the one call the label names after verb; the painter
	// fits it to the row so a long command keeps the file it acts on.
	label   string
	verb    string
	command *bashSummary
	// lead is said before the label and is never lit with it: how many
	// earlier runs a summary stands for.
	lead string
	// summary marks the header that stands for several runs folded together:
	// it counts what changed first and opens onto their headers, not onto
	// calls.
	summary bool
	// mark is the cue for calls that did not succeed. A header too narrow
	// for its label shortens the label and keeps the mark. cue is that cue as
	// counted, kept for a label that takes the header over when the settled
	// label had said it all and needed no mark.
	mark, cue    string
	live, failed bool
}

// chunkMark is what a painted header needs to know of its chunk once the
// line is laid out: which fold it is, and how its label is lit. It points at
// no record, so a cached layout keeps none alive and measures as what it
// is: a chunk's parts lead back into the records a view already owns.
type chunkMark struct {
	key, latest  string
	live, failed bool
}

func chunkMarks(chunks []activityChunk) []chunkMark {
	if len(chunks) == 0 {
		return nil
	}
	marks := make([]chunkMark, len(chunks))
	for i, c := range chunks {
		marks[i] = chunkMark{key: c.key, latest: c.latest, live: c.live, failed: c.failed}
	}
	return marks
}

// chunkLabelCols is the width a named command is fit to in label, wide
// enough that only the painter ever shortens it.
const chunkLabelCols = 512

// details visits only the rows this chunk owns, including members grouped
// under their launch. The image budget is shared by all chunks on the line.
func (c *activityChunk) details(imageLimit int) (agents []agentRowRef, images []style.Image) {
	for _, p := range c.parts {
		if p.tools == nil {
			continue
		}
		row := &p.tools.rows[p.row]
		if row.agent != nil {
			agents = append(agents, agentRowRef{p.tools, p.row})
		}
		if remaining := imageLimit - len(images); remaining > 0 {
			images = append(images, row.inspectionImages[:min(remaining, len(row.inspectionImages))]...)
		}
	}
	return agents, images
}

// hasOpenThought reports whether any thought in the chunk shows its text.
func (c *activityChunk) hasOpenThought() bool {
	for _, p := range c.parts {
		if p.thought != nil && p.thought.partOpen(p.segment) {
			return true
		}
	}
	return false
}

// activityChunks folds a line's records into chunks: the thoughts and calls
// in step order, a step's thought before its calls, never in the order
// results arrived. A transcript anchor is not in itself an event: both
// reasoning and tool records can span successive responses.
func (m *replModel) activityChunks(reasoningIDs, toolIDs []int64) []activityChunk {
	type event struct {
		step, index int
		thought     *reasoningRecord
		segment     int
		tools       *toolDisclosureRecord
		rows        []int // a batch: the calls of one model response, in tools.rows
	}
	var events []event
	// members are the workflow members each record lists with a launch call
	// rather than as calls of their own.
	members := map[*toolDisclosureRecord]map[int][]int{}
	for _, id := range reasoningIDs {
		r := m.reasoningRecords.get(id)
		if r == nil {
			continue
		}
		for i := range max(1, len(r.segments)) {
			step := 0
			if i < len(r.segments) {
				step = r.segments[i].step
			}
			events = append(events, event{step: step, index: r.transcriptIndex, thought: r, segment: i})
		}
	}
	for _, id := range toolIDs {
		r := m.toolDisclosures.get(id)
		if r == nil {
			continue
		}
		launched := launchedMembers(r)
		var listed map[int]bool
		if len(launched) > 0 {
			members[r], listed = launched, map[int]bool{}
			for _, rows := range launched {
				for _, i := range rows {
					listed[i] = true
				}
			}
		}
		var batch []int
		for i := range r.rows {
			if listed[i] {
				continue
			}
			if len(batch) > 0 && (r.rows[i].batchStart || r.rows[i].step != r.rows[batch[0]].step) {
				events = append(events, event{step: r.rows[batch[0]].step, index: r.transcriptIndex, tools: r, rows: batch})
				batch = nil
			}
			batch = append(batch, i)
		}
		if len(batch) > 0 {
			events = append(events, event{step: r.rows[batch[0]].step, index: r.transcriptIndex, tools: r, rows: batch})
		}
	}
	slices.SortStableFunc(events, func(a, b event) int {
		if a.step != b.step {
			return cmp.Compare(a.step, b.step)
		}
		// In a response, the reasoning precedes the execution of its calls.
		if a.thought != nil && b.thought == nil {
			return -1
		}
		if b.thought != nil && a.thought == nil {
			return 1
		}
		return cmp.Compare(a.index, b.index)
	})
	// ordinals maps a record's rows to their place among its ordinary rows,
	// which is how the record's display snapshot lists them.
	ordinals := map[*toolDisclosureRecord][]int{}
	ordinalOf := func(r *toolDisclosureRecord, index int) int {
		o, ok := ordinals[r]
		if !ok {
			o = make([]int, len(r.rows))
			n := 0
			for i := range r.rows {
				if r.rows[i].isAgent() {
					o[i] = -1
					continue
				}
				o[i] = n
				n++
			}
			ordinals[r] = o
		}
		return o[index]
	}
	var out []activityChunk
	// held are thoughts no chunk has taken yet: the next batch's chunk takes
	// them, whether they led it or belong to a step that made no call.
	var held []chunkPart
	run := -1
	for _, e := range events {
		if e.thought != nil {
			held = append(held, chunkPart{thought: e.thought, segment: e.segment, step: e.step})
			continue
		}
		group := batchGroup(e.tools, e.rows, members[e.tools])
		if run < 0 || out[run].group != group {
			out = append(out, activityChunk{group: group})
			run = len(out) - 1
		}
		c := &out[run]
		c.parts = append(c.parts, held...)
		held = nil
		for _, i := range e.rows {
			c.parts = append(c.parts, chunkPart{tools: e.tools, row: i, ordinal: ordinalOf(e.tools, i), step: e.step, launch: len(members[e.tools][i]) > 0})
			c.calls++
			for _, member := range members[e.tools][i] {
				c.parts = append(c.parts, chunkPart{tools: e.tools, row: member, ordinal: -1, step: e.step})
			}
		}
		c.latest = callIdentity(e.tools.rows[e.rows[0]])
	}
	for i := range out {
		m.describeChunk(&out[i])
	}
	// A thought no call has followed belongs with the run before it: it is
	// that run's last row, and while it streams it is what the run's header
	// says, as a call in flight would be. A row of its own would come and go
	// with every response that thinks before it calls. The header waits until
	// the thought has outlasted the time a label is held, so one too short to
	// read never shows. A run still at work with its agents keeps its header;
	// a thought streaming after it has the row below, which the next call's
	// header takes.
	if run >= 0 && len(held) > 0 {
		last, newest := &out[run], held[len(held)-1]
		age, streaming := m.thoughtTime(newest)
		if !streaming || !last.live {
			last.parts = append(last.parts, held...)
			held = nil
			if streaming && age >= thinkingLabelDelay {
				last.label, _ = m.thoughtLabel(newest, true)
				last.verb, last.command = "", nil
				last.kind, last.live = activityTools, true
				last.latest = m.thoughtPartKey(newest.thought, newest.segment)
				// A label such as "tests failed" was its own cue; this one is not.
				last.mark = last.cue
			}
		}
	}
	// A thought with no run to join stands alone, one row each.
	for _, t := range held {
		c := activityChunk{group: chunkThought, parts: []chunkPart{t}}
		m.describeChunk(&c)
		out = append(out, c)
	}
	return out
}

// thoughtSection names a reasoning record for keys that outlive a projection.
func (m *replModel) thoughtSection(r *reasoningRecord) string {
	if r.sectionKey != "" {
		return r.sectionKey
	}
	return fmt.Sprintf("reasoning:%d", r.id)
}

func (m *replModel) chunkKey(c *activityChunk) string {
	for _, p := range c.parts {
		if p.thought != nil {
			continue
		}
		if id := callIdentity(p.tools.rows[p.row]); id != "" {
			return "call:" + id
		}
		// No host identity at all: the record and row are all there is.
		return fmt.Sprintf("row:%d/%d", p.tools.id, p.row)
	}
	p := c.parts[0]
	return fmt.Sprintf("thought:%s/%d", m.thoughtSection(p.thought), p.segment)
}

// thoughtLabel names one step's thought: as a row among its chunk's calls,
// timed like them, or standing alone, where it reads as a sentence. A thought
// still streaming says so; the clock beside it is what moves.
func (m *replModel) thoughtLabel(p chunkPart, alone bool) (label string, live bool) {
	d, live := m.thoughtTime(p)
	switch {
	case live:
		label = "thinking…"
		if d >= time.Second {
			label += " " + activityElapsed(d)
		}
	case alone:
		label = "thought"
		if d >= time.Second {
			label += " for " + activityElapsed(d)
		}
	default:
		label = "thought"
		if d >= time.Second {
			label += " · " + activityElapsed(d)
		}
	}
	if p.thought.unsaved {
		label += " · not saved"
	}
	return label, live
}

// thoughtTime is how long one step's thought has taken, and whether it is
// still streaming.
func (m *replModel) thoughtTime(p chunkPart) (d time.Duration, live bool) {
	r := p.thought
	live = r.active && p.segment == max(0, len(r.segments)-1)
	d = r.elapsed
	if p.segment < len(r.segments) && (len(r.segments) > 1 || r.tailCut) {
		d = r.segments[p.segment].elapsed
	}
	if live && r.id == m.turnReasoningID && !m.thinkingSegmentStart.IsZero() {
		d += time.Since(m.thinkingSegmentStart)
	}
	return d, live
}

// thinkingLabelDelay is how long a thought streams before the newest run's
// header says so: the time a label is held, so a thought that would be gone
// before its label could be read never takes the header.
const thinkingLabelDelay = collapsedActivityMinimum

// describeChunk works out what a chunk's header says. While a call runs the
// header is that call. Settled, it is what the run did: a run of one call
// names the call, a longer one counts by kind. A call that failed, was denied
// or was canceled marks its run and never moves to another: membership is
// decided by what was requested, so a late result cannot re-split a chunk.
func (m *replModel) describeChunk(c *activityChunk) {
	c.key = m.chunkKey(c)
	if c.group == chunkThought {
		c.kind = activityThought
		c.label, c.live = m.thoughtLabel(c.parts[0], true)
		return
	}
	c.kind = activityTools
	byAction := map[callAction][]*toolDisclosureRow{}
	var running []*toolDisclosureRow
	// all counts every call that did not succeed; asked leaves out the agent
	// launches, whose summary counts its own.
	var all, asked outcomeCounts
	unknown, launching := 0, false
	for _, p := range c.parts {
		if p.thought != nil {
			continue
		}
		r := &p.tools.rows[p.row]
		a := c.actionOf(r)
		launch := a == actionAgent || p.launch
		if !p.launch {
			byAction[a] = append(byAction[a], r)
		}
		switch {
		case all.add(r.pres.outcome):
			if !launch {
				asked.add(r.pres.outcome)
			}
		case a == actionAgent: // the agent's ongoing state is independent of its launch
		case !r.settled && !p.tools.complete:
			if p.launch {
				launching = true
			} else {
				running = append(running, r)
			}
		case r.pres.outcome != toolOutcomeOK && len(r.inspectionImages) == 0:
			unknown++
		}
	}
	c.mark = all.mark(c.calls == 1)
	c.cue, c.failed = c.mark, c.mark != ""

	// While a call asked with a launch runs, the header is that call, as in
	// any run; the agents have the row once it has settled.
	if c.group == chunkAgent && len(running) == 0 {
		var counts activityAgentCounts
		for _, r := range byAction[actionAgent] {
			switch {
			case r.agent == nil:
				counts.addOutcome("unknown", false)
			case r.agent.local != "":
				counts.addOutcome(r.agent.local, r.agent.busy())
			default:
				counts.add(r.agent.state)
			}
		}
		// The summary counts its own failures and cancellations; the cue is
		// for the calls asked with the launch, which are counted after it.
		c.kind, c.mark = activityAgents, asked.mark(false)
		labels := []string{turnAgentSummaryLabel(counts.Total, counts.Running, counts.Failed, counts.Canceled, counts.Paused, counts.Deferred)}
		for _, a := range c.actionOrder() {
			if rows := byAction[a]; len(rows) > 0 {
				labels = append(labels, actionLabel(a, rows, tenseDone))
			}
		}
		c.label = joinLabels(labels)
		// A launch call still running holds the header live with its agents.
		c.live = counts.Running > 0 || launching
		c.failed = c.failed || counts.Failed+counts.Canceled > 0
		return
	}
	if len(running) > 0 {
		c.live = true
		action, single := c.actionOf(running[0]), true
		for _, r := range running[1:] {
			single = single && c.actionOf(r) == action
		}
		if single {
			c.setActionLabel(action, running, tenseLive, len(running) == 1)
		} else {
			c.label = "running " + countNoun(len(running), "tool") + "…"
		}
		return
	}
	switch {
	case c.group == chunkTest && !c.failed && unknown == 0:
		c.label = "tests passed"
	case c.group == chunkTest && c.calls == 1 && c.failed:
		// The whole label is the failure; it needs no second cue.
		c.label, c.mark = "tests "+c.mark, ""
	case c.group == chunkTest:
		c.label = countNoun(c.calls, string(actionTest))
	case unknown > 0 && !c.failed:
		// A stored legacy result can lack an outcome, or the call itself can
		// lack a result. Neither proves success or continued execution.
		c.label = countNoun(c.calls, "call")
		if len(byAction) == 1 {
			for a := range byAction {
				c.label = countNoun(c.calls, string(a)+" call")
			}
		}
	default:
		var parts []string
		for _, a := range c.actionOrder() {
			rows := byAction[a]
			if len(rows) == 0 {
				continue
			}
			if c.calls == 1 {
				// A lone call that did not succeed is named as it was
				// asked: it edited nothing and viewed nothing. A command or
				// a tool call that failed did run.
				tense := tenseDone
				if c.failed && !(all.failed == 1 && (a == actionShell || a == actionTool)) {
					tense = tenseAsked
				}
				c.setActionLabel(a, rows, tense, true)
				return
			}
			parts = append(parts, actionLabel(a, rows, tenseDone))
		}
		c.label = joinLabels(parts)
	}
}

// sharedActionOrder is the order a header counts its kinds in; editActionOrder
// is the same with the edits first.
var (
	sharedActionOrder = []callAction{actionRead, actionView, actionEdit, actionShell, actionTool}
	editActionOrder   = []callAction{actionEdit, actionRead, actionView, actionShell, actionTool}
)

// actionOrder is the order this chunk's header counts its kinds in. It is
// fixed, so the eye learns where each count sits: what was read and viewed,
// what was changed, then what was run. An edit run leads with its edits, so
// it still reads as one when a search or a command was asked beside them, and
// a summary of several runs leads with them because what changed is what a
// reader looks for first.
func (c *activityChunk) actionOrder() []callAction {
	if c.group == chunkEdit || c.summary {
		return editActionOrder
	}
	return sharedActionOrder
}

// actionOf is what a call counts as in this chunk. A test asked beside other
// calls is one of the run's commands; only a run of nothing but tests reports
// on them as tests.
func (c *activityChunk) actionOf(r *toolDisclosureRow) callAction {
	a := callActionOf(r)
	if a == actionTest && c.group != chunkTest {
		return actionShell
	}
	return a
}

// outcomeCounts tallies the calls of a run that did not succeed, by how.
type outcomeCounts struct{ denied, failed, canceled, paused int }

// add counts outcome when it is one of those, and reports whether it was.
func (o *outcomeCounts) add(outcome toolOutcome) bool {
	switch outcome {
	case toolOutcomeDenied:
		o.denied++
	case toolOutcomeFailed:
		o.failed++
	case toolOutcomeCanceled:
		o.canceled++
	case toolOutcomePaused:
		o.paused++
	default:
		return false
	}
	return true
}

// mark is the header's cue for those calls. alone says the run is one call,
// which takes the word without a count.
func (o outcomeCounts) mark(alone bool) string {
	var marks []string
	for _, s := range []struct {
		n    int
		word string
	}{{o.denied, "denied"}, {o.failed, "failed"}, {o.canceled, "canceled"}, {o.paused, "paused"}} {
		switch {
		case s.n == 0:
		case alone:
			marks = append(marks, s.word)
		default:
			marks = append(marks, fmt.Sprintf("%d %s", s.n, s.word))
		}
	}
	return strings.Join(marks, ", ")
}

// joinLabels reads a run's counts as a list: "a", "a and b", "a, b and c".
func joinLabels(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// labelTense is how a label speaks of its calls: as done, as in flight, or,
// for a call that did not happen, as what was asked.
type labelTense uint8

const (
	tenseDone labelTense = iota
	tenseLive
	tenseAsked
)

// pick is the word for the tense.
func (t labelTense) pick(done, live, asked string) string {
	switch t {
	case tenseLive:
		return live
	case tenseAsked:
		return asked
	}
	return done
}

// setActionLabel labels rows of one action. alone says they are the whole of
// what the label covers, so a single command or tool call is named rather
// than counted: "ran a command" tells a reader nothing the fold does not.
func (c *activityChunk) setActionLabel(a callAction, rows []*toolDisclosureRow, tense labelTense, alone bool) {
	if alone && len(rows) == 1 {
		r := rows[0]
		switch a {
		case actionShell:
			if r.bash != nil {
				c.verb, c.command = tense.pick("ran ", "running ", "run "), r.bash
				c.label = c.verb + c.command.fit(chunkLabelCols)
				return
			}
		case actionTool:
			if name := toolRowName(r); name != "" {
				c.label = tense.pick("used ", "using ", "use ") + name
				return
			}
		}
	}
	c.label = actionLabel(a, rows, tense)
}

// toolRowName is a tool call as its row names it: the short tool name, then
// what it was called with.
func toolRowName(r *toolDisclosureRow) string {
	if r.toolName == "" {
		return bashSummaryLine(r.label)
	}
	subject := strings.TrimSpace(strings.TrimPrefix(r.label, r.toolName))
	return bashSummaryLine(strings.TrimSpace(compactToolName(r.toolName) + " " + subject))
}

func actionLabel(a callAction, rows []*toolDisclosureRow, tense labelTense) string {
	n := len(rows)
	switch a {
	case actionTest:
		return tense.pick("tests passed", "running tests…", "tests")
	case actionShell, actionTool:
		noun := string(a)
		object := "a " + noun
		if n > 1 {
			object = countNoun(n, noun)
		}
		if a == actionTool {
			return tense.pick("used ", "using ", "use ") + object
		}
		return tense.pick("ran ", "running ", "run ") + object
	}
	// Count distinct known paths, not calls or basenames. A pathless call
	// isn't evidence of a new file; don't guess the distinct-file count.
	paths := map[string]bool{}
	missing, adds, dels := 0, 0, 0
	add := func(s string) {
		if s == "" {
			missing++
		} else if strings.Contains(s, "://") {
			paths[s] = true // preserve URL syntax until basename extraction
		} else {
			paths[path.Clean(s)] = true
		}
	}
	for _, r := range rows {
		if a == actionView {
			switch {
			case len(r.inspectionImages) > 0:
				for _, img := range r.inspectionImages {
					add(cmp.Or(img.DisplayPath, img.Path, r.imageSource))
				}
			default:
				add(r.imageSource)
			}
		} else if r.file != nil {
			add(r.file.path)
		} else {
			add("")
		}
		if a == actionEdit {
			x, y := r.pres.changes.totals()
			adds += x
			dels += y
		}
	}
	word, noun := tense.pick("read", "reading", "read"), "file"
	if a == actionEdit {
		word = tense.pick("edited", "editing", "edit")
	}
	if a == actionView {
		word, noun = tense.pick("viewed", "viewing", "view"), "image"
	}
	object := "a " + noun
	if noun == "image" {
		object = "an image"
	}
	switch {
	case missing == 0 && len(paths) == 1:
		for s := range paths {
			if name := labelBasename(s); name != "" {
				object = name
			}
		}
	case missing == 0:
		object = countNoun(len(paths), noun)
	case n > 1:
		return countNoun(n, string(a)+" call")
	}
	text := word + " " + object
	if adds > 0 {
		text += fmt.Sprintf(" +%d", adds)
	}
	if dels > 0 {
		text += fmt.Sprintf(" −%d", dels)
	}
	return text
}

func labelBasename(s string) string {
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		s = u.Path
	}
	s = path.Base(strings.ReplaceAll(s, `\`, "/"))
	if s == "." || s == "/" || s == ".." {
		return ""
	}
	return strings.TrimSpace(style.SanitizeImageText(s))
}
