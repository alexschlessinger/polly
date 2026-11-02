package main

import (
	"slices"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
)

// agentOverviewCache belongs to one inspector display. It retains only bounded
// excerpts, not a second full transcript or tool-output cache.
// Dropping the display drops this cache; it does not modify Watch's m.visual.
type agentOverviewCache struct {
	assignment, update agentOverviewExcerptCache
	thinking           agentOverviewThinkingCache
}

// Reasoning already has a bounded semantic tail. Cache its newest physical
// rows independently from Watch's preview width and disclosure state.
type agentOverviewThinkingCache struct {
	record  *reasoningRecord
	version uint64
	width   int
	epoch   uint64
	rows    [][]ui.Cell
}

func (c *agentOverviewThinkingCache) render(record *reasoningRecord, width int) [][]ui.Cell {
	if record == nil {
		if c != nil {
			*c = agentOverviewThinkingCache{}
		}
		return nil
	}
	if c != nil && c.record == record && c.version == record.tailVersion && c.width == width && c.epoch == style.Epoch() {
		return c.rows
	}
	lines := reasoningTailLines(string(record.tail), width, agentOverviewThinkingRows)
	var rows [][]ui.Cell
	for _, line := range lines {
		rows = append(rows, style.VisualRows(style.Styled(line, "muted", "italic"), ui.StyleClear, width)...)
	}
	if c != nil {
		*c = agentOverviewThinkingCache{record: record, version: record.tailVersion, width: width, epoch: style.Epoch(), rows: rows}
	}
	return rows
}

type agentOverviewExcerptCache struct {
	text         string
	images       []style.Image
	width, limit int
	epoch        uint64
	rows         [][]ui.Cell
	truncated    bool
	valid        bool
}

func (c *agentOverviewExcerptCache) render(text string, images []style.Image, width, limit int) ([][]ui.Cell, bool) {
	if c != nil && c.valid && c.text == text && c.width == width && c.limit == limit && c.epoch == style.Epoch() && style.ImagesEqual(c.images, images) {
		return c.rows, c.truncated
	}
	source := text
	if len(images) == 0 {
		text = style.StripImageMarkers(text)
	}
	rows, _ := transcriptBlockRowsWithImages(text, false, width, images, false, 0, 0)
	truncated := limit > 0 && len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	if c != nil {
		*c = agentOverviewExcerptCache{}
		if limit > 0 {
			// Wrapped rows may share the full parsed body's backing array.
			// Keep only the visible cells, even for a huge hidden update.
			visible := make([][]ui.Cell, len(rows))
			for i, row := range rows {
				visible[i] = slices.Clone(row)
			}
			*c = agentOverviewExcerptCache{
				text: source, images: slices.Clone(images), width: width, limit: limit,
				epoch: style.Epoch(), rows: visible, truncated: truncated, valid: true,
			}
			rows = visible
		}
	}
	return rows, truncated
}
