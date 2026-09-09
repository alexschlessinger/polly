package main

import ui "github.com/metaspartan/gotui/v5"

// transcriptRowsText flattens rendered rows to plain strings for assertions.
func transcriptRowsText(rows [][]ui.Cell) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = ui.CellsToString(row)
	}
	return out
}
