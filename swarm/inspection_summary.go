package swarm

// Model inspection summarizes the existing presentation; Go and UI consumers
// retain AgentPresentation and its full execution and task provenance.
type agentStateSummary struct {
	Lifecycle  Lifecycle `json:"lifecycle"`
	TaskStatus string    `json:"taskStatus,omitempty"`
	Attention  bool      `json:"attention"`
	Deferred   bool      `json:"deferred"`
	Detail     string    `json:"detail,omitempty"`
}

func summarizeAgentState(p AgentPresentation) agentStateSummary {
	detail := p.Detail
	if detail == p.TaskStatus {
		detail = ""
	}
	return agentStateSummary{Lifecycle: p.Lifecycle, TaskStatus: p.TaskStatus,
		Attention: p.Attention, Deferred: p.Deferred, Detail: clipInspection(detail, 512)}
}
