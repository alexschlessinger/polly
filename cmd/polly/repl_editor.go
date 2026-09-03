package main

import (
	"unicode"
)

// lineEditor: the composer text buffer and its cursor motions.

// lineEditor is a single-line rune buffer with a cursor and readline-style
// editing operations. It owns no terminal state and performs no rendering —
// the REPL feeds it discrete key events and reads back text/cursor for display.
type lineEditor struct {
	buf    []rune
	cursor int
	// goalCol is the rune column that vertical movement (up/down) tries to
	// keep as the cursor crosses lines of differing length; -1 means "unset —
	// recompute from the cursor on the next vertical move". Every horizontal
	// move or edit resets it, so each fresh up/down run starts from the column
	// the cursor is on, while a run of consecutive up/downs holds its column.
	goalCol int
}

func (e *lineEditor) text() string { return string(e.buf) }
func (e *lineEditor) empty() bool  { return len(e.buf) == 0 }

func (e *lineEditor) setText(s string) {
	e.buf = []rune(s)
	e.cursor = len(e.buf)
	e.goalCol = -1
}

func (e *lineEditor) clear() {
	e.buf = nil
	e.cursor = 0
	e.goalCol = -1
}

func (e *lineEditor) insert(r rune) {
	e.buf = append(e.buf[:e.cursor], append([]rune{r}, e.buf[e.cursor:]...)...)
	e.cursor++
	e.goalCol = -1
}

func (e *lineEditor) backspace() {
	if e.cursor > 0 {
		e.buf = append(e.buf[:e.cursor-1], e.buf[e.cursor:]...)
		e.cursor--
	}
	e.goalCol = -1
}

func (e *lineEditor) deleteForward() {
	if e.cursor < len(e.buf) {
		e.buf = append(e.buf[:e.cursor], e.buf[e.cursor+1:]...)
	}
	e.goalCol = -1
}

func (e *lineEditor) left() {
	if e.cursor > 0 {
		e.cursor--
	}
	e.goalCol = -1
}

func (e *lineEditor) right() {
	if e.cursor < len(e.buf) {
		e.cursor++
	}
	e.goalCol = -1
}

func (e *lineEditor) home() { e.cursor = e.lineStartAt(e.cursor); e.goalCol = -1 }
func (e *lineEditor) end()  { e.cursor = e.lineEndAt(e.cursor); e.goalCol = -1 }

func (e *lineEditor) killToStart() {
	e.buf = append([]rune(nil), e.buf[e.cursor:]...)
	e.cursor = 0
	e.goalCol = -1
}

func (e *lineEditor) killToEnd() {
	e.buf = append([]rune(nil), e.buf[:e.cursor]...)
	e.goalCol = -1
}

// prevWordStart is the index where the word before the cursor begins: skip any
// whitespace to the left, then skip the run of non-whitespace.
func (e *lineEditor) prevWordStart() int {
	i := e.cursor
	for i > 0 && unicode.IsSpace(e.buf[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(e.buf[i-1]) {
		i--
	}
	return i
}

// nextWordEnd is the index just past the word after the cursor: skip any
// whitespace to the right, then skip the run of non-whitespace.
func (e *lineEditor) nextWordEnd() int {
	i, n := e.cursor, len(e.buf)
	for i < n && unicode.IsSpace(e.buf[i]) {
		i++
	}
	for i < n && !unicode.IsSpace(e.buf[i]) {
		i++
	}
	return i
}

func (e *lineEditor) wordLeft()  { e.cursor = e.prevWordStart(); e.goalCol = -1 }
func (e *lineEditor) wordRight() { e.cursor = e.nextWordEnd(); e.goalCol = -1 }

func (e *lineEditor) deleteWordBackward() {
	e.goalCol = -1
	start := e.prevWordStart()
	if start == e.cursor {
		return
	}
	e.buf = append(e.buf[:start], e.buf[e.cursor:]...)
	e.cursor = start
}

func (e *lineEditor) deleteWordForward() {
	e.goalCol = -1
	end := e.nextWordEnd()
	if end == e.cursor {
		return
	}
	e.buf = append(e.buf[:e.cursor], e.buf[end:]...)
}

// lineStartAt returns the index of the first rune on the logical line that
// contains position pos (0, or one past the preceding '\n').
func (e *lineEditor) lineStartAt(pos int) int {
	i := pos
	for i > 0 && e.buf[i-1] != '\n' {
		i--
	}
	return i
}

// lineEndAt returns the index just past the last rune on the logical line that
// contains pos (the index of the next '\n', or len(buf)).
func (e *lineEditor) lineEndAt(pos int) int {
	i := pos
	for i < len(e.buf) && e.buf[i] != '\n' {
		i++
	}
	return i
}

// up moves the cursor to the same column on the previous logical line, holding
// the goal column across shorter lines. It returns false when the cursor is
// already on the first line, so the caller can fall through to history recall.
func (e *lineEditor) up() bool {
	start := e.lineStartAt(e.cursor)
	if start == 0 {
		return false // already on the first line
	}
	if e.goalCol < 0 {
		e.goalCol = e.cursor - start
	}
	prevStart := e.lineStartAt(start - 1)
	prevLen := (start - 1) - prevStart // runes before the '\n' that ends it
	col := e.goalCol
	if col > prevLen {
		col = prevLen
	}
	e.cursor = prevStart + col
	return true
}

// down mirrors up onto the next logical line. It returns false when the cursor
// is already on the last line.
func (e *lineEditor) down() bool {
	end := e.lineEndAt(e.cursor)
	if end >= len(e.buf) {
		return false // already on the last line
	}
	if e.goalCol < 0 {
		e.goalCol = e.cursor - e.lineStartAt(e.cursor)
	}
	nextStart := end + 1
	nextLen := e.lineEndAt(nextStart) - nextStart
	col := e.goalCol
	if col > nextLen {
		col = nextLen
	}
	e.cursor = nextStart + col
	return true
}
