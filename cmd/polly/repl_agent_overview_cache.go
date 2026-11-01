package main

import (
	"slices"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
)

// agentOverviewCache belongs to one inspector display. It retains only the two
// collapsed prose excerpts, not a second full transcript or tool-output cache.
// Dropping the display drops this cache; it does not modify Watch's m.visual.
type agentOverviewCache struct {
	assignment, update agentOverviewExcerptCache
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
