package main

import (
	"strings"
	"time"
	"unicode/utf8"

	ui "github.com/metaspartan/gotui/v5"
	"github.com/rivo/uniseg"
)

const (
	typewriterRate  = 90 // graphemes per second; larger bursts catch up faster
	typewriterDelay = 120 * time.Millisecond
)

type typewriterArrival struct {
	end int
	at  time.Time
}

// streamTypewriter paces only the visible prefix. Arrivals retain a deadline
// so a fast provider cannot build up an animation backlog. The raw response,
// tool execution, and turn completion never wait for this presentation state.
type streamTypewriter struct {
	shown    int
	arrivals []typewriterArrival
	frameAt  time.Time
	credit   float64
}

func (s *streamTypewriter) snap(end int) {
	*s = streamTypewriter{shown: end}
}

func (s *streamTypewriter) append(end int, now time.Time) {
	if len(s.arrivals) == 0 {
		// Idle time must not turn into a bank of characters to reveal at once.
		s.frameAt, s.credit = now, 0
	}
	s.arrivals = append(s.arrivals, typewriterArrival{end: end, at: now})
}

func (s *streamTypewriter) visibleLen(raw string, now time.Time) int {
	if s.shown >= len(raw) || len(s.arrivals) == 0 {
		s.snap(len(raw))
		return s.shown
	}
	// Work on the small pending tail, never rescan the already revealed text.
	// Grapheme boundaries keep accented letters and multi-codepoint emoji
	// together instead of briefly painting half a character.
	tail := raw[s.shown:]
	count := uniseg.GraphemeClusterCount(tail)
	rate := max(float64(typewriterRate), float64(count)/typewriterDelay.Seconds())
	s.credit += max(0, now.Sub(s.frameAt).Seconds()) * rate
	s.frameAt = now
	steps := int(s.credit)
	s.credit -= float64(steps)
	if s.shown == 0 {
		steps = max(1, steps)
	}
	due := s.shown
	for _, arrival := range s.arrivals {
		if now.Sub(arrival.at) < typewriterDelay {
			break
		}
		due = arrival.end
	}
	base := s.shown
	g := uniseg.NewGraphemes(tail)
	for (steps > 0 || s.shown < due) && g.Next() {
		_, end := g.Positions()
		s.shown = base + end
		steps--
	}
	for len(s.arrivals) > 0 && s.arrivals[0].end <= s.shown {
		s.arrivals = s.arrivals[1:]
	}
	if len(s.arrivals) == 0 {
		s.snap(s.shown)
	}
	return s.shown
}

func (m *replModel) typewriterVisible() bool {
	return m.affordancesVisible() && m.busy && !m.canceling && m.followBottom
}

// assistantTypewriter reveals formatted cells, not raw Markdown delimiters.
// Its optional cell prefix is a transcript projection; entry.text continues
// to hold the complete formatted output and images keep their real markers.
type assistantTypewriter struct {
	streamTypewriter
	receivedAt time.Time
	instant    bool
	source     string
	text       string
	cells      []ui.Cell
	count      int
}

func (s *assistantTypewriter) received(now time.Time, animate bool) {
	if s.receivedAt.IsZero() {
		s.receivedAt = now
	}
	if !animate {
		s.instant = true
	}
}

func (s *assistantTypewriter) update(source string, now time.Time, animate bool) bool {
	if !animate {
		changed := s.prefix() != nil
		*s = assistantTypewriter{instant: true}
		return changed
	}
	previous := s.count
	if source != s.source {
		s.cells = parseStyledCells(strings.TrimRight(source, "\r\n"), ui.StyleClear)
		text := ui.CellsToString(s.cells)
		// Late link definitions or completed tables can rewrite earlier text.
		// Show those corrections immediately instead of replaying the block.
		if !strings.HasPrefix(text, s.text) {
			s.instant = true
		}
		if len(text) != len(s.text) {
			at := s.receivedAt
			if at.IsZero() {
				at = now
			}
			s.append(len(text), at)
		}
		s.source, s.text = source, text
	}
	s.receivedAt = time.Time{}
	if s.instant {
		s.snap(len(s.text))
		s.instant = false
	}
	n := s.visibleLen(s.text, now)
	s.count = len(s.cells)
	if n < len(s.text) {
		s.count = utf8.RuneCountInString(s.text[:n])
	}
	return previous != s.count
}

func (s *assistantTypewriter) prefix() []ui.Cell {
	if s.count >= len(s.cells) {
		return nil
	}
	return s.cells[:s.count]
}
