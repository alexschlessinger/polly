package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/workflow"
)

const (
	deliveryInlineBytes = inspectionBytes
	admissionBytes      = 64 * 1024
	admissionMessages   = 16
)

func (r *Runtime) completionNotice(s *State, m *Member, e *Execution, t *Task) *Mail {
	mail := &Mail{ID: ids.New(), From: m.ID, To: r.ID, Kind: "info", Posted: time.Now().UTC()}
	if t == nil {
		mail.Text = "Agent " + clipInspection(m.Label, 512) + " " + e.Status
		return mail
	}
	mail.Task, mail.Revision, mail.Execution = t.ID, t.Revision, e.ID
	if e.Status != "completed" {
		mail.Text = fmt.Sprintf("Agent %s failed task %s. Reason: %s. The task is blocked: update it or cancel it.", clipInspection(m.Label, 512), t.ID, clipInspection(e.Error, 1024))
		return mail
	}
	mail.Text = fmt.Sprintf("Agent %s completed task %s revision %d.", clipInspection(m.Label, 512), t.ID, t.Revision)
	switch t.Requirement {
	case RequirementReviewed:
		mail.Text += fmt.Sprintf(" Review it: swarm_review({task: %q, revision: %d, accept: true}) or request changes.", t.ID, t.Revision)
	case RequirementApplied:
		result := "Editing result"
		if commit := snapshotCommit(s, t.Snapshot); commit != "" {
			result = "Submitted commit " + commit
		}
		mail.Text += fmt.Sprintf(" %s awaits integration: %s or request changes with swarm_review.", result, integrateTasksAction([]TaskReference{{Task: t.ID, Revision: t.Revision}}))
	}
	return mail
}

func deliveryBody(text, reference string) string {
	if len(text) <= deliveryInlineBytes {
		return "Result:\n" + text
	}
	return fmt.Sprintf("Result (%d bytes; full text: %s):\n%s", len(text), reference, clipInspection(text, 2048))
}

func admittedMailText(s *State, m *Mail) string {
	text := m.Text
	if f := s.Followups[m.ID]; f != nil && f.Phase == "launched" {
		v := followupView(s, m)
		provenance := fmt.Sprintf("Follow-up %s: %s, task %s, execution %s; baseline origin: %s.", m.ID, v.Operation, v.Task, v.Execution, v.BaseOrigin)
		if v.BaseCommit != "" {
			provenance += " Baseline commit: " + v.BaseCommit + "."
		}
		if v.Source != "" {
			provenance += " Live source: " + v.Source + "."
		}
		text = provenance + " " + v.Note + "\n\n" + text
	}
	if m.Task != "" {
		if t := s.Tasks[m.Task]; t != nil {
			if t.Revision == m.Revision && t.Execution == m.Execution {
				text += "\n" + deliveryBody(agentResultText(t.Result), fmt.Sprintf("swarm_read({view: \"tasks\", id: %q, section: \"result\"})", t.ID))
			} else {
				text += fmt.Sprintf("\n(superseded: task %s is now revision %d)", t.ID, t.Revision)
			}
		}
	}
	if w := s.Workflows[m.Workflow]; w != nil {
		text += "\n" + deliveryBody(agentResultText(w.Output), fmt.Sprintf("swarm_read({view: \"workflows\", id: %q, section: \"output\"})", w.ID))
	}
	// Ordinary peer mail may itself be large. Preserve its durable original and
	// admit a bounded preview so an oversized oldest message cannot block the queue.
	if len(text) > admissionBytes/2 {
		text = clipInspection(text, admissionBytes/2-1024) + fmt.Sprintf("\nFull message: swarm_read({view: \"messages\", id: %q}).", m.ID)
	}
	return text
}

func recordDelivery(s *State, m *Mail) {
	if t := s.Tasks[m.Task]; t != nil && deliveringTask(s, t) && t.Revision == m.Revision && t.Execution == m.Execution {
		t.Delivery = &TaskDelivery{Via: "mail", Ref: m.ID, Revision: t.Revision, Execution: t.Execution, Inline: len(agentResultText(t.Result)) <= deliveryInlineBytes, At: time.Now().UTC()}
		t.Status = "done"
	}
	if w := s.Workflows[m.Workflow]; w != nil && w.Status == "completed" {
		w.Acknowledged = true
	}
}

func agentStepReceipt(step workflow.Step) (AgentResult, bool) {
	if step.Status != "completed" || step.Kind != "agent" && step.Kind != "followup" {
		return AgentResult{}, false
	}
	var result AgentResult
	data, err := json.Marshal(step.Value)
	if err != nil || json.Unmarshal(data, &result) != nil {
		return result, false
	}
	return result, result.Task != "" && result.Session != "" && result.Execution != "" && result.Revision > 0
}

func recordWorkflowDeliveries(s *State, w *workflow.Report) {
	for _, step := range w.Steps {
		result, ok := agentStepReceipt(step)
		if !ok {
			continue
		}
		e, t := s.Executions[result.Execution], s.Tasks[result.Task]
		if e == nil || e.Status != "completed" || e.Workflow != w.ID || e.Member != result.Session || e.Request.CallID != step.ID || e.Result == nil {
			continue
		}
		saved := e.Result
		if saved.Task != result.Task || saved.Session != result.Session || saved.Execution != result.Execution || saved.Revision != result.Revision {
			continue
		}
		if !deliveringTask(s, t) || t.Owner != result.Session || t.Execution != result.Execution || t.Revision != result.Revision {
			continue
		}
		t.Delivery = &TaskDelivery{Via: "workflow_step", Ref: step.ID, Execution: result.Execution, Revision: result.Revision, Inline: true, At: time.Now().UTC()}
		t.Status = "done"
	}
}

func needsDeliveryNotice(s *State, t *Task) bool {
	return deliveringTask(s, t) && !TaskDeferred(s, t) && resultNotice(s, t) == nil &&
		s.Members[t.Owner] != nil && !workflowControlled(s, s.Executions[t.Execution])
}

// Waiting must not write or broadcast when there is nothing to repair: Settle
// would wake itself on every iteration. Recheck eligibility in the transaction
// so a concurrent delivery or workflow change cannot post a stale notice.
func (r *Runtime) repairDeliveryNotices(ctx context.Context, s *State) (*State, error) {
	for _, t := range s.Tasks {
		if needsDeliveryNotice(s, t) {
			err := r.update(ctx, func(current *State) error {
				r.ensureDeliveryNotices(current)
				s = current
				return nil
			})
			return s, err
		}
	}
	return s, nil
}

func (r *Runtime) ensureDeliveryNotices(s *State) {
	for _, id := range sortedInspectionIDs(s.Tasks) {
		t := s.Tasks[id]
		if !needsDeliveryNotice(s, t) {
			continue
		}
		notice := r.completionNotice(s, s.Members[t.Owner], s.Executions[t.Execution], t)
		s.Messages[notice.ID] = notice
	}
}

func deliveryCount(f *coordinationFacts) int {
	n := len(f.outputs)
	for _, tf := range f.tasks {
		if tf.delivering {
			n++
		}
	}
	return n
}

func deliveryNext(n int) string {
	if n == 1 {
		return "1 result awaits delivery; its completion notice accompanies this prompt: read it, then answer"
	}
	text := countNoun(n, "result") + " await delivery; their completion notices accompany this prompt: read them, then answer"
	if n > admissionMessages {
		text += "; more follow: park with wait_agent until none remain"
	}
	return text
}

func workflowDeliveredTasks(s *State, w *workflow.Report) string {
	var tasks []string
	for _, id := range sortedInspectionIDs(s.Tasks) {
		t := s.Tasks[id]
		if t.Delivery != nil && t.Delivery.Via == "workflow_step" && strings.HasPrefix(t.Delivery.Ref, w.ID+"/") {
			tasks = append(tasks, t.ID)
		}
	}
	if len(tasks) == 0 {
		return ""
	}
	return ". Delivered to the script only: tasks " + strings.Join(tasks, ", ")
}
