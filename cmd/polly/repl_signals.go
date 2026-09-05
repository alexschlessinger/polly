package main

// Signals from hidden tabs. A tab off screen still settles turns and asks
// for tool approvals; the visible transcript gets one notice line per
// event, named after the tab, and /tab lists what each hidden tab is up
// to. Signals are keyed on the tab's screen model, so a tab that is not a
// session of its own (a subagent's, say) signals the same way.

type tabSignalKind int

const (
	signalTurnDone tabSignalKind = iota
	signalTurnFailed
	signalApprovalNeeded
)

// tabSignal is one event on a hidden tab, awaiting relay into the visible
// transcript. detail is the elapsed time, the error, or the tool label.
type tabSignal struct {
	kind   tabSignalKind
	detail string
}

// signalHiddenLocked records an event for relay when the tab is hidden; a
// visible tab shows the event itself. Caller must hold m.mu.
func (m *replModel) signalHiddenLocked(kind tabSignalKind, detail string) {
	if !m.hidden {
		return
	}
	m.notificationMu.Lock()
	defer m.notificationMu.Unlock()
	m.signals = append(m.signals, tabSignal{kind: kind, detail: detail})
}

// formatTabSignal is the notice line for a signal from tab.
func formatTabSignal(tab *replTab, s tabSignal) string {
	name := tab.signalName()
	switch s.kind {
	case signalTurnDone:
		return name + " done · " + s.detail
	case signalTurnFailed:
		return name + " failed · " + s.detail
	case signalApprovalNeeded:
		return name + " needs approval: " + s.detail + " · /tab " + tab.name
	}
	return name + ": " + s.detail
}

// relayTabSignals moves hidden tabs' signals into the visible transcript as
// notice lines. News of a settled turn waits while the visible tab's own
// turn runs, so it never lands inside streaming output; a request for
// approval cannot wait, since the turn behind it is stalled until someone
// answers. Runs on the event loop with no model lock held.
func (r *managedREPL) relayTabSignals() {
	r.model.mu.Lock()
	busy := r.model.busy
	r.model.mu.Unlock()
	var lines []string
	var failures []bool
	for _, tab := range r.tabs {
		m := tab.model
		if m == r.model {
			continue
		}
		m.notificationMu.Lock()
		var kept []tabSignal
		for _, s := range m.signals {
			if busy && s.kind != signalApprovalNeeded {
				kept = append(kept, s)
				continue
			}
			lines = append(lines, formatTabSignal(tab, s))
			failures = append(failures, s.kind == signalTurnFailed)
		}
		m.signals = kept
		m.notificationMu.Unlock()
	}
	if len(lines) == 0 {
		return
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	for i, line := range lines {
		if failures[i] {
			r.model.appendLine(styled(line, "err", ""))
		} else {
			r.model.appendNoticeLine(line)
		}
	}
}
