package swarm

// Model inspection summarizes the existing presentation; Go and UI consumers
// retain AgentPresentation and its full execution and task provenance.
type agentStateSummary struct {
	Lifecycle  Lifecycle  `json:"lifecycle"`
	TaskStatus string     `json:"taskStatus,omitempty"`
	Attention  bool       `json:"attention"`
	Deferred   bool       `json:"deferred"`
	Detail     string     `json:"detail,omitempty"`
	RetryPause RetryPause `json:"retryPause,omitzero"`
}

func summarizeAgentState(p AgentPresentation) agentStateSummary {
	detail := p.Detail
	if detail == p.TaskStatus {
		detail = ""
	}
	pause := p.RetryPause
	pause.Reason = clipInspection(pause.Reason, 512)
	return agentStateSummary{Lifecycle: p.Lifecycle, TaskStatus: p.TaskStatus,
		Attention: p.Attention, Deferred: p.Deferred, Detail: clipInspection(detail, 512), RetryPause: pause}
}
