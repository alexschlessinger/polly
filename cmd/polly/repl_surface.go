package main

import (
	"image"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

// viewSurface is where the inspected view is shown: the area its content
// occupies on a screen, and what the last paint seated there. The view's
// header, rows, buttons and scrollbar address the surface and nothing around
// it, so whatever provides the area decides only where it is. Today that is
// the frame polly paints beside the conversation (see repl_chrome.go).
type viewSurface struct {
	// inner holds the header rows and the body beneath them. track is the
	// column beside the body that carries the scrollbar thumb, empty when the
	// surface has no room for one. Both are absolute on the surface's screen,
	// as of the last paint.
	inner, track image.Rectangle

	header *style.LiteralParagraph
	// headerRows is how many of inner's rows the header took.
	headerRows int
	body       *transcriptParagraph
	// buttons are the last paint's click targets, absolute like inner.
	buttons   []inspectorButton
	scrollbar scrollbar
}

func newViewSurface() viewSurface {
	s := viewSurface{body: newTranscriptParagraph(), header: style.NewLiteralParagraph()}
	noBorder(&s.body.Block)
	s.body.UseRows = true
	noBorder(&s.header.Block)
	s.header.WrapText = false
	return s
}

// place seats the surface for a frame.
func (s *viewSurface) place(inner, track image.Rectangle) {
	s.inner, s.track = inner, track
}

// split divides the surface into the header, the body beneath it, and the
// scrollbar track beside the body.
func (s *viewSurface) split() (header, body, track image.Rectangle) {
	if s.inner.Empty() {
		return
	}
	rows := max(0, min(s.headerRows, s.inner.Dy()))
	header = image.Rect(s.inner.Min.X, s.inner.Min.Y, s.inner.Max.X, s.inner.Min.Y+rows)
	body = image.Rect(s.inner.Min.X, header.Max.Y, s.inner.Max.X, s.inner.Max.Y)
	if !s.track.Empty() {
		track = image.Rect(s.track.Min.X, body.Min.Y, s.track.Max.X, body.Max.Y)
	}
	return header, body, track
}

// bodyHeight is the rows left for the view under its header; it can be
// negative before the first paint.
func (s *viewSurface) bodyHeight() int {
	return s.inner.Dy() - s.headerRows
}
