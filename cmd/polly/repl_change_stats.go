package main

func (m *replModel) changeStats() (additions, deletions, files int) {
	if m.workspaceChanges == nil {
		return sessionChangeStats(m.displayCatalog.tools)
	}
	if !m.workspaceChanges.tracked {
		return 0, 0, 0
	}
	additions, deletions = m.workspaceChanges.totals()
	return additions, deletions, len(m.workspaceChanges.changes)
}
