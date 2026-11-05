package main

import (
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
)

// contextTokenTotal is a sum of recorded request counts, not context occupancy.
// Missing reports leave a lower bound rather than turning unknown usage into zero.
type contextTokenTotal struct {
	tokens  int
	missing bool
}

func (t *contextTokenTotal) add(tokens int, reported bool) {
	if reported {
		t.tokens += tokens
	} else {
		t.missing = true
	}
}

func (t contextTokenTotal) plus(other contextTokenTotal) contextTokenTotal {
	return contextTokenTotal{tokens: t.tokens + other.tokens, missing: t.missing || other.missing}
}

func (t contextTokenTotal) text() string {
	if t.missing {
		if t.tokens == 0 {
			return "—"
		}
		return "≥" + humanizeTokens(t.tokens)
	}
	return humanizeTokens(t.tokens)
}

type contextTokenUsage struct {
	input, output contextTokenTotal
}

func (u *contextTokenUsage) addMessage(msg messages.ChatMessage) {
	if !msg.ReportsUsage() {
		return
	}
	_, inputReported := msg.Metadata[messages.MetadataKeyInputTokens]
	_, outputReported := msg.Metadata[messages.MetadataKeyOutputTokens]
	u.input.add(msg.GetInputTokens(), inputReported)
	u.output.add(msg.GetOutputTokens(), outputReported)
}

func (u contextTokenUsage) plus(other contextTokenUsage) contextTokenUsage {
	return contextTokenUsage{input: u.input.plus(other.input), output: u.output.plus(other.output)}
}

type contextSessionUsage struct {
	parent, subagents contextTokenUsage
	agents            []contextAgentUsage
	err               error
}

// These display projections own their slices and retain only primitive values,
// never member, execution, or usage pointers from the coordination snapshot.
type contextAgentUsage struct {
	id, label string
	usage     contextTokenUsage
}

func contextUsageFromExecution(usage swarm.Usage) contextTokenUsage {
	var total contextTokenUsage
	if usage.Samples == 0 && usage.InputTokens == nil && usage.OutputTokens == nil {
		return total // No recorded responses yet, not a missing provider report.
	}
	if usage.InputTokens != nil {
		total.input.add(*usage.InputTokens, true)
	} else {
		total.input.missing = true
	}
	if usage.OutputTokens != nil {
		total.output.add(*usage.OutputTokens, true)
	} else {
		total.output.missing = true
	}
	return total
}

// Every execution belongs to this root, including workflow agents. Sum all of
// them, not each member's latest execution or the visible roster: follow-ups get
// new executions, resumes accumulate on the existing one, and workspace release
// preserves the records. Result receipts and child transcripts are not added again.
func (u *contextSessionUsage) addExecutions(state *swarm.State) {
	totals := make(map[string]contextTokenUsage)
	for _, execution := range state.Executions {
		totals[execution.Member] = totals[execution.Member].plus(contextUsageFromExecution(execution.Usage))
	}
	// Only the final member order matters; execution sums are order-independent.
	for _, id := range swarmRecordIDs(totals) {
		label := ""
		if member := state.Members[id]; member != nil {
			label = strings.Join(strings.Fields(style.SanitizeImageText(swarmMemberLabel(state, member))), " ")
		}
		if label == "" {
			label = strings.Join(strings.Fields(style.SanitizeImageText(id)), " ")
		}
		usage := totals[id]
		u.agents = append(u.agents, contextAgentUsage{id: id, label: label, usage: usage})
		u.subagents = u.subagents.plus(usage)
	}
}

func contextUsageRow(label, input, output string) string {
	return fmt.Sprintf("%-13s %14s %14s", label, input, output)
}

func (u contextSessionUsage) details() []string {
	total := u.parent.plus(u.subagents)
	rows := []string{contextUsageRow("Session usage", "↑ input", "↓ output")}
	for _, entry := range []struct {
		label string
		usage contextTokenUsage
	}{{"Parent", u.parent}, {"Subagents", u.subagents}, {"Total", total}} {
		rows = append(rows, contextUsageRow(entry.label, entry.usage.input.text(), entry.usage.output.text()))
	}
	if total.input.missing || total.output.missing {
		rows = append(rows, "≥ partial · — not reported")
	}
	if u.err != nil {
		rows = append(rows, "Subagent usage unavailable: "+u.err.Error())
	}
	return rows
}
