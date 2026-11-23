package main

// The row budget: how much of a turn's activity a collapsed view keeps on
// screen.
//
// A run is a header row, and a long turn makes many: an edit ends a run, so
// work that looks, edits and looks again adds two rows each time round. A
// collapsed view therefore gives one activity line a fixed number of rows.
// While the turn is still adding to the line, its newest runs keep their
// headers and the ones before them fold under one summary header. Once the
// line is over, because the answer or a notice follows it or its turn has
// ended, all of its runs fold under that header. A line within the budget is
// left as it is.
//
// The summary is a fold like any other: a click opens it onto the headers it
// stands for, which read as they would without it, and Ctrl-O closes it with
// every other fold made by hand. An expanded view has no budget: it shows
// every call.

// activityRowBudget is how many rows a collapsed activity line may take
// before its runs fold under a summary.
const activityRowBudget = 5

// foldedRuns is how many of the line's leading chunks fold under a summary
// header, zero for a line within its budget or a view that is expanded.
func (m *replModel) foldedRuns(block *transcriptDisplayBlock) int {
	if m.depth != depthCollapsed {
		return 0
	}
	// The budget counts runs. A thought with a row of its own comes after
	// them and goes again, and must not make a summary come and go with it.
	runs := runCount(block.chunks)
	if runs <= activityRowBudget {
		return 0
	}
	if block.currentActivity && !m.activityFollowed(block) {
		// The summary is one of the budget's rows.
		return runs - (activityRowBudget - 1)
	}
	return len(block.chunks)
}

// runCount is how many of chunks are runs of calls.
func runCount(chunks []activityChunk) int {
	n := 0
	for i := range chunks {
		if chunks[i].group != chunkThought {
			n++
		}
	}
	return n
}

// activityFollowed reports whether prose, a notice or a prompt comes after
// the line's records: activity after that opens a line of its own, so this
// one is over even while its turn runs on.
func (m *replModel) activityFollowed(block *transcriptDisplayBlock) bool {
	last := -1
	for _, id := range block.reasoningIDs {
		if r := m.reasoningRecords.get(id); r != nil {
			last = max(last, r.transcriptIndex)
		}
	}
	for _, id := range block.toolDisclosureIDs {
		if r := m.toolDisclosures.get(id); r != nil {
			last = max(last, r.transcriptIndex)
		}
	}
	return last >= 0 && m.separatedAfter(last)
}

// separatedAfter reports whether a transcript entry that is neither activity
// nor empty comes after the entry at index.
func (m *replModel) separatedAfter(index int) bool {
	for i := len(m.transcript) - 1; i > index; i-- {
		if m.reasoningRecords.idAt(i) == 0 && m.toolDisclosures.idAt(i) == 0 && !transparentLineEntry(m, i) {
			return true
		}
	}
	return false
}

// runsSummaryKey names the fold over a line's runs by the first of them, so
// it stays the same fold as runs join it and when the session is reloaded.
func runsSummaryKey(first string) string { return "runs:" + first }

// runsSummary is the header that stands for runs folded together: everything
// they did, counted as one run would count it, with what changed first.
// earlier says later runs of the line still have headers of their own, and
// open that the summary's runs are listed after it. Either way the summary
// says how many runs it stands for, so it reads as their heading and not as
// one more of them; closed over a whole line it is the line, and needs no
// count.
func (m *replModel) runsSummary(runs []activityChunk, earlier, open bool) activityChunk {
	s := activityChunk{summary: true}
	for i := range runs {
		c := &runs[i]
		s.parts = append(s.parts, c.parts...)
		s.calls += c.calls
		switch {
		case c.group == chunkAgent:
			s.group = chunkAgent
		case c.group == chunkEdit && s.group != chunkAgent:
			s.group = chunkEdit
		}
	}
	m.describeChunk(&s)
	// The fold is its own, not the first run's, and no call hands a label on
	// to it: the newest run does that under its own header.
	s.key, s.latest = runsSummaryKey(runs[0].key), ""
	switch {
	case earlier:
		s.lead = countNoun(runCount(runs), "earlier run")
	case open:
		s.lead = countNoun(runCount(runs), "run")
	}
	return s
}
