package main

import (
	"strings"
	"time"
	"unicode"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	rw "github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"
)

// Chunk headers: one row per fold, never a strip of phrases. The triangle is
// the only fold mark in a transcript, so it always means the same thing: a
// run that a click or Ctrl-O opens. Thoughts and calls are rows of evidence
// with their own glyphs, and open their detail on a click.

// headerText makes a label safe for one row: no image markers, no bidi
// controls, one space between words.
func headerText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Bidi_Control, r) {
			return -1
		}
		return r
	}, style.SanitizeImageText(s))
	return strings.Join(strings.Fields(s), " ")
}

// The transcript wraps parsed cells (one rune apiece), not grapheme widths.
// Use its cell metric even for a multi-rune emoji; clip only between clusters.
func headerCols(s string) int {
	cols := 0
	for _, r := range s {
		cols += max(0, rw.RuneWidth(r))
	}
	return cols
}

// fitLabel clips text to room columns, ending a clipped label in an
// ellipsis.
func fitLabel(text string, room int) string {
	if headerCols(text) <= room {
		return text
	}
	if room < 1 {
		return ""
	}
	mark := "…"
	if headerCols(mark) > room { // one column left in a wide-punctuation locale
		mark = "."
	}
	g := uniseg.NewGraphemes(text)
	cols, end := headerCols(mark), 0
	for g.Next() {
		w := headerCols(g.Str())
		if cols+w > room {
			break
		}
		cols += w
		_, end = g.Positions()
	}
	return strings.TrimRight(text[:end], " ") + mark
}

// sentenceCase capitalizes a label's lead word, never its file or command.
func sentenceCase(s string) string {
	if s != "" && s[0] >= 'a' && s[0] <= 'z' {
		return strings.ToUpper(s[:1]) + s[1:]
	}
	return s
}

const collapsedActivityMinimum = 500 * time.Millisecond

// collapsedActivityHold remembers the last in-flight label shown on the newest
// folded run. It holds only the wording, not the tool state or the evidence.
// There is no queue: when the hold ends, the row catches up to the latest work.
type collapsedActivityHold struct {
	turn                    uint64
	key, batch, label, verb string
	command                 *bashSummary
	mark                    string
	failed, pending         bool
	until                   time.Time
}

func (m *replModel) holdCollapsedActivity(c *activityChunk, now time.Time) {
	h := &m.activityHold
	if c.kind != activityTools || m.chunkOpen(c.key) || m.canceling {
		*h = collapsedActivityHold{}
		return
	}
	sameRun := h.key == c.key && h.turn == m.activityTurn
	if sameRun && now.Before(h.until) && c.mark == h.mark && c.failed == h.failed {
		h.pending = c.latest != h.batch || c.label != h.label || c.verb != h.verb || c.command != h.command
		if h.pending {
			// The handoff follows the batch actually named on screen, not the
			// one waiting to replace it. Outcome styling still follows reality.
			c.latest, c.label, c.verb, c.command = h.batch, h.label, h.verb, h.command
		}
		return
	}
	if !c.live {
		*h = collapsedActivityHold{}
		return
	}
	until := h.until
	if !sameRun || h.batch != c.latest {
		until = now.Add(collapsedActivityMinimum)
	}
	*h = collapsedActivityHold{
		turn: m.activityTurn, key: c.key, batch: c.latest,
		label: c.label, verb: c.verb, command: c.command,
		mark: c.mark, failed: c.failed, until: until,
	}
}

const chunkMarkSeparator = " · "

// chunkLabelLead is the columns a header's triangle and the space after it
// take before the label.
const chunkLabelLead = 2

// chunkLeadLabelMinimum is the columns a header's label keeps beside a lead;
// a row with less drops the lead.
const chunkLeadLabelMinimum = 24

// thoughtGlyph marks a thought row, in the column where a call row has its
// outcome.
const thoughtGlyph = "∴"

// renderChunkHeader is one chunk's row: the fold triangle, what the run did,
// and beside it the cue for calls that did not succeed. A summary of several
// runs says how many before what they did. A row too narrow for all of it
// drops that count, then shortens the label, and keeps the cue. cols is how
// much of the row the header paints, all of it one hit target; label is
// where the words sit, for the sweep that marks a call in flight.
func renderChunkHeader(c *activityChunk, width int, open bool) (text string, cols int, label turnDockPlacement) {
	if width < 1 {
		return "", 0, turnDockPlacement{}
	}
	glyph := "▸"
	if open {
		glyph = "▾"
	}
	text = style.Styled(glyph, "accent", "bold")
	const indent = chunkLabelLead
	if width <= indent {
		return text, 1, turnDockPlacement{}
	}
	room := width - indent
	mark := headerText(c.mark)
	markCols := 0
	if mark != "" {
		markCols = headerCols(chunkMarkSeparator) + headerCols(mark)
	}
	// The lead is said before the label and gives way first: a row with no
	// room for both keeps what the runs did.
	lead := sentenceCase(headerText(c.lead))
	if lead != "" {
		lead += chunkMarkSeparator
		if room-markCols-headerCols(lead) < chunkLeadLabelMinimum {
			lead = ""
		}
	}
	leadCols := headerCols(lead)
	room -= leadCols
	words := c.label
	if c.command != nil {
		words = c.verb + c.command.fit(max(1, room-markCols-headerCols(c.verb)))
	}
	words = headerText(words)
	if lead == "" {
		words = sentenceCase(words)
	}
	fg, mod := "muted", ""
	switch {
	case c.live:
		fg, mod = "active", "bold"
	case c.failed && mark == "":
		fg, mod = "err", "bold"
	}
	if markCols > 0 && room-markCols < 2 {
		// No room for a word beside the cue: the cue is what stays.
		mark = fitLabel(mark, room)
		return text + " " + style.Styled(mark, "err", "bold"), indent + headerCols(mark), turnDockPlacement{}
	}
	shown := fitLabel(words, room-markCols)
	text += " "
	if lead != "" {
		text += style.Styled(lead, "muted", "")
	}
	text += style.Styled(shown, fg, mod)
	// Sweep ink, not the clipping mark or the settled outcomes after the
	// agents still at work: all of it remains part of the hit target.
	ink := shown
	if shown != words {
		ink = strings.TrimSuffix(strings.TrimSuffix(shown, "…"), ".")
	}
	if c.kind == activityAgents {
		ink, _, _ = strings.Cut(ink, ",")
	}
	label = turnDockPlacement{kind: c.kind, X: indent + leadCols, Cols: headerCols(ink)}
	if markCols > 0 {
		text += style.Styled(chunkMarkSeparator, "muted", "") + style.Styled(mark, "err", "bold")
	}
	return text, indent + leadCols + headerCols(shown) + markCols, label
}

// renderThoughtRow is a thought as a row of evidence: its glyph where a call
// has its outcome, then the label, bright only while the thought streams.
func renderThoughtRow(label string, live bool) string {
	fg, mod := "muted", ""
	if live {
		fg, mod = "active", "bold"
	}
	return style.Styled(thoughtGlyph, "muted", "") + " " + style.Styled(label, fg, mod)
}
