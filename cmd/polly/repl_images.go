package main

import (
	"fmt"
	"image"
	"os/exec"
	"runtime"
	"slices"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	"github.com/alexschlessinger/pollytool/images"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

const maxLocalImageBytes = images.MaxSourceBytes

// transcriptDisplayBlock is one renderable unit of the transcript. An
// activity block carries the reasoning and tool disclosure records it shows;
// adjacent activity entries merge into one block, so both lists may hold
// several IDs.
type transcriptDisplayBlock struct {
	key                     string
	text                    string
	cells                   []ui.Cell // optional formatted streaming prefix
	images                  []style.Image
	reasoningIDs            []int64
	toolDisclosureIDs       []int64
	turnTrailerID           int64
	activityFields          []turnDockPlacement
	activityLabels          []turnDockPlacement // label paint bounds, independent of hitboxes
	activityReasoningDetail string
	activityToolDetail      string
	activityImageDetail     string
	thoughtSpan             [2]int // byte range of the open thought section in text
	agentLinks              []agentLink
}

// isActivity reports whether the block projects reasoning or tool records.
func (b transcriptDisplayBlock) isActivity() bool {
	return len(b.reasoningIDs) > 0 || len(b.toolDisclosureIDs) > 0
}

type transcriptImageSpan struct {
	imageIndex int
	row        int
	x          int
	cols       int
	rows       int
	fitByRows  bool
}

// refreshTranscriptImageSources updates dimensions when a referenced file is
// regenerated in place. This lets the transcript reflow its reserved slot
// before the terminal protocol sees the new aspect ratio.
func (m *replModel) refreshTranscriptImageSources(width int) bool {
	changed := false
	refresh := func(list []style.Image) ([]style.Image, bool) {
		updated := list
		copied := false
		for imageIndex, img := range list {
			version := images.FileVersion(img.Path)
			if version == img.Version {
				continue
			}
			if !copied {
				updated = append([]style.Image(nil), list...)
				copied = true
			}
			updated[imageIndex].Version = version
			if width, height, ok := markdown.LocalImageDimensions(img.Path); ok {
				updated[imageIndex].Width = width
				updated[imageIndex].Height = height
			}
		}
		return updated, copied
	}
	// This runs inside transcriptRows, so it cannot measure display blocks
	// (that would re-enter the layout); it re-anchors in per-entry space,
	// which is exact only while no earlier activity entries have merged.
	for transcriptIndex := range m.transcript {
		updated, copied := refresh(m.transcript[transcriptIndex].images)
		if copied {
			oldCount, start := 0, 0
			if !m.followBottom {
				oldCount = m.entryVisualLineCount(transcriptIndex, width)
				start = m.entryVisualStart(transcriptIndex, width)
			}
			// The one raw lane write: this runs inside transcriptRows, which
			// invalidates on the returned flag, so the owner's invalidation
			// would be redundant here.
			m.transcript[transcriptIndex].images = updated
			if !m.followBottom {
				m.anchorForResizedEntry(start, oldCount, m.entryVisualLineCount(transcriptIndex, width))
			}
			changed = true
		}
	}
	// Tool and Images disclosures project canonical sidecars into transient
	// activity blocks. Keep those copies fresh so reopening either disclosure
	// cannot resurrect stale dimensions or file versions.
	for _, record := range m.toolDisclosures.all() {
		for rowIndex := range record.rows {
			updated, copied := refresh(record.rows[rowIndex].inspectionImages)
			if copied {
				record.rows[rowIndex].inspectionImages = updated
				changed = true
			}
		}
	}
	return changed
}

// locateTranscriptImages removes private markers from terminal cells. A row
// may hold several markers side by side, as an image strip does; it collapses
// once every thumbnail on it has used up its fitted rows. With no native
// backend the marker rows collapse completely, leaving the caption/path as a
// compact fallback.
func locateTranscriptImages(rows [][]ui.Cell, images []style.Image, native bool, width, cellWidth, cellHeight int) ([][]ui.Cell, []transcriptImageSpan) {
	if len(images) == 0 {
		return rows, nil
	}
	type point struct {
		row int
		x   int
	}
	type marker struct {
		index int
		cell  int
		x     int
	}
	type slotGeometry struct {
		cols      int
		rows      int
		fitByRows bool
	}
	points := make(map[int][]point)
	geometries := make(map[int]slotGeometry)
	markerRows := make(map[int]int)
	out := make([][]ui.Cell, 0, len(rows))
	var markers []marker
	for _, row := range rows {
		markers = markers[:0]
		x := 0
		for i, cell := range row {
			if index, ok := style.ImageMarkerIndex(cell.Rune); ok && index < len(images) {
				markers = append(markers, marker{index: index, cell: i, x: x})
			}
			if width := rw.RuneWidth(cell.Rune); width > 0 {
				x += width
			}
		}
		if len(markers) == 0 {
			out = append(out, row)
			continue
		}
		if !native {
			continue
		}
		row = append([]ui.Cell(nil), row...)
		live := false
		for _, mark := range markers {
			geometry, ok := geometries[mark.index]
			if !ok {
				imageMaxCols, imageMaxRows := style.ImageBounds(images[mark.index])
				maxCols := min(imageMaxCols, width-mark.x)
				cols, slotRows, fitByRows := termimg.CellGeometry(images[mark.index], maxCols, imageMaxRows, cellWidth, cellHeight)
				geometry = slotGeometry{cols: cols, rows: slotRows, fitByRows: fitByRows}
				geometries[mark.index] = geometry
			}
			seenRows := markerRows[mark.index]
			markerRows[mark.index] = seenRows + 1
			row[mark.cell].Rune = ' '
			if geometry.cols <= 0 || geometry.rows <= 0 || seenRows >= geometry.rows {
				continue
			}
			live = true
			points[mark.index] = append(points[mark.index], point{row: len(out), x: mark.x})
		}
		if live {
			out = append(out, row)
		}
	}

	var spans []transcriptImageSpan
	for imageIndex := range images {
		geometry := geometries[imageIndex]
		imagePoints := points[imageIndex]
		for start := 0; start < len(imagePoints); {
			end := start + 1
			for end < len(imagePoints) && imagePoints[end].row == imagePoints[end-1].row+1 && imagePoints[end].x == imagePoints[start].x {
				end++
			}
			spans = append(spans, transcriptImageSpan{
				imageIndex: imageIndex,
				row:        imagePoints[start].row,
				x:          imagePoints[start].x,
				cols:       geometry.cols,
				rows:       end - start,
				fitByRows:  geometry.fitByRows,
			})
			start = end
		}
	}
	return out, spans
}

// visibleImagePlacements projects transcript-relative slots into screen cells.
// A slot that is scrolled only partly into the pane is clipped to the rows and
// columns that are on screen rather than dropped, so thumbnails stay visible
// while the buffer scrolls; the manager crops the pixels to match.
func (m *replModel) visibleImagePlacements(v transcriptViewport) []termimg.Placement {
	if !m.nativeImages || v.width < style.MinimumThumbnailCols {
		return nil
	}
	var placements []termimg.Placement
	rowOffset := 0
	for _, block := range m.visual.blocks {
		for _, span := range block.imageSpans {
			if span.imageIndex < 0 || span.imageIndex >= len(block.images) || span.rows <= 0 {
				continue
			}
			row := rowOffset + span.row
			top := max(row, v.start)
			bottom := min(row+span.rows, v.end)
			if bottom <= top {
				continue
			}
			left := max(span.x, 0)
			right := min(span.x+span.cols, v.width)
			if right <= left {
				continue
			}
			img := block.images[span.imageIndex]
			placement := termimg.Placement{
				Key:       fmt.Sprintf("%s:image:%d", block.key, span.imageIndex),
				X:         span.x,
				Y:         v.screenY(row),
				Cols:      span.cols,
				Rows:      span.rows,
				FitByRows: span.fitByRows,
			}
			if img.Embedded != "" {
				placement.Embedded = img.Embedded
			} else {
				placement.Path = img.Path
			}
			if top != row || bottom != row+span.rows || left != span.x || right != span.x+span.cols {
				placement.Clip = termimg.Clip{
					X:    left - span.x,
					Y:    top - row,
					Cols: right - left,
					Rows: bottom - top,
				}
			}
			placements = append(placements, placement)
		}
		rowOffset += len(block.rows)
	}
	return placements
}

// occludePlacements lets text-layer cells drawn at covers sit above native
// images, which otherwise paint over everything: each placement a cover
// touches is split into clipped pieces that tile exactly its uncovered cells,
// row runs with the same uncovered spans merging into one piece. It returns a
// new slice; the caller's placements stay intact for hit-testing.
func occludePlacements(placements []termimg.Placement, covers []image.Rectangle) []termimg.Placement {
	if len(covers) == 0 {
		return placements
	}
	out := make([]termimg.Placement, 0, len(placements))
	for _, p := range placements {
		visible := p.Bounds()
		if !slices.ContainsFunc(covers, visible.Overlaps) {
			out = append(out, p)
			continue
		}
		var pieces, open []image.Rectangle
		for y := visible.Min.Y; y < visible.Max.Y; y++ {
			spans := uncoveredSpans(visible.Min.X, visible.Max.X, y, covers)
			var next []image.Rectangle
			for _, span := range spans {
				if i := slices.IndexFunc(open, func(r image.Rectangle) bool {
					return r.Min.X == span[0] && r.Max.X == span[1]
				}); i >= 0 {
					open[i].Max.Y = y + 1
					next = append(next, open[i])
					open = slices.Delete(open, i, i+1)
					continue
				}
				next = append(next, image.Rect(span[0], y, span[1], y+1))
			}
			pieces = append(pieces, open...)
			open = next
		}
		pieces = append(pieces, open...)
		for i, piece := range pieces {
			clipped := p
			if i > 0 {
				clipped.Key = fmt.Sprintf("%s:piece:%d", p.Key, i)
			}
			clipped.Clip = termimg.Clip{
				X:    piece.Min.X - p.X,
				Y:    piece.Min.Y - p.Y,
				Cols: piece.Dx(),
				Rows: piece.Dy(),
			}
			out = append(out, clipped)
		}
	}
	return out
}

// uncoveredSpans returns the [left, right) runs of row y within [left, right)
// that no cover reaches, in order.
func uncoveredSpans(left, right, y int, covers []image.Rectangle) [][2]int {
	spans := [][2]int{{left, right}}
	for _, cover := range covers {
		if y < cover.Min.Y || y >= cover.Max.Y {
			continue
		}
		var cut [][2]int
		for _, span := range spans {
			if cover.Max.X <= span[0] || cover.Min.X >= span[1] {
				cut = append(cut, span)
				continue
			}
			if cover.Min.X > span[0] {
				cut = append(cut, [2]int{span[0], cover.Min.X})
			}
			if cover.Max.X < span[1] {
				cut = append(cut, [2]int{cover.Max.X, span[1]})
			}
		}
		spans = cut
	}
	return spans
}

// openImageInViewer hands a local image to the OS default viewer, detached
// from the TUI. The reap goroutine keeps the exited launcher from lingering
// as a zombie.
func openImageInViewer(path string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	cmd := exec.Command(opener, path)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
