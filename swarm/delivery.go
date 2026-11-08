package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/internal/coordsem"
	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/workflow"
)

const (
	deliveryInlineBytes = inspectionBytes
	admissionBytes      = 64 * 1024
	admissionMessages   = 16
)

func deliveryPendingError() error {
	return fail("delivery_pending", "result awaits delivery; read its completion notice at the next input boundary, then issue followup_task again as a new call. This call did not start the worker; do not integrate or acknowledge ordinary research to unblock it")
}

func (r *Runtime) completionNotice(s *State, m *Member, e *Execution, t *Task) *Mail {
	commit := ""
	if t != nil {
		commit = snapshotCommit(s, t.Snapshot)
	}
	return r.completionNoticeValues(m, e, t, commit)
}

func (r *Runtime) completionNoticeValues(m *Member, e *Execution, t *Task, commit string) *Mail {
	mail := &Mail{ID: ids.New(), From: m.ID, To: r.ID, Kind: "info", Posted: time.Now().UTC()}
	label := clipInspection(m.Label, 512)
	if t == nil {
		mail.Text = "Agent " + label + " " + e.Status
		return mail
	}
	mail.Task, mail.Revision, mail.Execution = t.ID, t.Revision, e.ID
	if e.Status == "paused" {
		if e.RetryPause != nil {
			mail.Text = fmt.Sprintf("Agent %s paused task %s: %s. Its assignment and workspace are retained. Continue explicitly with followup_task using target %q once eligible.", label, t.ID, e.RetryPause.description(), agentName(m))
		} else if e.StopReason == messages.StopReasonMaxIterations {
			mail.Text = fmt.Sprintf("Agent %s paused task %s at its iteration limit. Saved results remain available. Ask the user for an additional iteration grant through the client before continuing.", label, t.ID)
		} else {
			mail.Text = fmt.Sprintf("Agent %s was interrupted during task %s. Its assignment is retained. Continue with followup_task using target %q and the remaining instructions.", label, t.ID, agentName(m))
		}
		return mail
	}
	if e.Status != "completed" {
		mail.Text = fmt.Sprintf("Agent %s failed task %s. Reason: %s. Resolve the blocker and continue with followup_task using target %q, or cancel it with swarm_control.", label, t.ID, clipInspection(e.Error, 1024), agentName(m)) + outputsText(t.Outputs)
		return mail
	}
	mail.Text = fmt.Sprintf("Agent %s completed task %s revision %d.", label, t.ID, t.Revision) + outputsText(t.Outputs)
	switch t.Requirement {
	case RequirementReviewed:
		mail.Text += fmt.Sprintf(" Review it: swarm_review({task: %q, revision: %d, accept: true}) or request changes.", t.ID, t.Revision)
	case RequirementApplied:
		result := "Editing result"
		if commit != "" {
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

// followupProvenance describes a launched follow-up to the member it
// addresses: what it started, from which baseline, and what that means.
func followupProvenance(s *State, m *Mail) string {
	f := s.Followups[m.ID]
	if f == nil || f.Phase != "launched" {
		return ""
	}
	v := followupView(s, m)
	provenance := fmt.Sprintf("Follow-up %s: %s, task %s, execution %s; baseline origin: %s.", m.ID, v.Operation, v.Task, v.Execution, v.BaseOrigin)
	if v.BaseCommit != "" {
		provenance += " Baseline commit: " + v.BaseCommit + "."
	}
	if v.Source != "" {
		provenance += " Live source: " + v.Source + "."
	}
	return provenance + " " + v.Note
}

func admittedMailText(s *State, m *Mail) string {
	text := m.Text
	if provenance := followupProvenance(s, m); provenance != "" {
		text = provenance + "\n\n" + text
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

func (r *recordReader) admittedMail(mail *Mail) (string, *workflow.ReportHeader, bool, error) {
	f, err := r.Followup(mail.ID)
	if err != nil {
		return "", nil, false, err
	}
	if f != nil && f.Refresh && f.Phase != "launched" {
		return "", nil, true, nil
	}
	text := mail.Text
	if f != nil && f.Phase == "launched" {
		commit := ""
		if f.Base != "" {
			snapshot, err := r.Snapshot(f.Base)
			if err != nil {
				return "", nil, false, err
			}
			if validTaskSnapshot(snapshot, f.Base) {
				commit = snapshot.Commit
			}
		}
		provenance := fmt.Sprintf("Follow-up %s: %s, task %s, execution %s; baseline origin: %s.", mail.ID, f.Operation, f.Task, f.Execution, f.BaseOrigin)
		if commit != "" {
			provenance += " Baseline commit: " + commit + "."
		}
		if f.Source != "" {
			provenance += " Live source: " + f.Source + "."
		}
		note := "Continues the workspace, including worker edits beyond its baseline. Parent changes are not copied."
		switch f.BaseOrigin {
		case "parent":
			note = "Uses this call's parent capture; later parent edits are excluded."
		case "previous_result":
			note = "Uses the worker's previous result. refresh:true selects current parent files after work settles."
		case "original_baseline":
			note = "Uses the original baseline. refresh:true selects current parent files after work settles."
		case "live_source":
			note = "Observes the saved live source; no immutable baseline."
		}
		text = provenance + " " + note + "\n\n" + text
	}
	if mail.Task != "" {
		t, err := r.Task(mail.Task)
		if err != nil {
			return "", nil, false, err
		}
		if t != nil {
			if t.Revision == mail.Revision && t.Execution == mail.Execution {
				text += "\n" + deliveryBody(agentResultText(t.Result), fmt.Sprintf("swarm_read({view: \"tasks\", id: %q, section: \"result\"})", t.ID))
			} else {
				text += fmt.Sprintf("\n(superseded: task %s is now revision %d)", t.ID, t.Revision)
			}
		}
	}
	var w *workflow.ReportHeader
	if mail.Workflow != "" {
		w, err = r.WorkflowHeader(mail.Workflow)
		if err != nil {
			return "", nil, false, err
		}
		if w != nil {
			text += "\n" + deliveryBody(agentResultText(w.Output), fmt.Sprintf("swarm_read({view: \"workflows\", id: %q, section: \"output\"})", w.ID))
		}
	}
	if len(text) > admissionBytes/2 {
		text = clipInspection(text, admissionBytes/2-1024) + fmt.Sprintf("\nFull message: swarm_read({view: \"messages\", id: %q}).", mail.ID)
	}
	return text, w, false, nil
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

func (tx *recordTx) recordDelivery(d *checkpointDependencies, mail *Mail) error {
	if task := d.Tasks[mail.Task]; task.delivering() && task.Task.Revision == mail.Revision && task.Task.Execution == mail.Execution {
		t := task.Task
		t.Delivery = &TaskDelivery{Via: "mail", Ref: mail.ID, Revision: t.Revision, Execution: t.Execution, Inline: len(agentResultText(t.Result)) <= deliveryInlineBytes, At: time.Now().UTC()}
		t.Status = "done"
		if err := tx.PutTask(t); err != nil {
			return err
		}
	}
	if w := d.Workflows[mail.Workflow]; w != nil && w.Status == "completed" {
		w.Acknowledged = true
		if err := tx.PutWorkflowHeader(w); err != nil {
			return err
		}
	}
	return nil
}

func checkpointReceipts(appended []messages.ChatMessage) admissionReceipts {
	out := admissionReceipts{}
	seen := map[string]bool{}
	for _, m := range appended {
		if ids, ok := m.Metadata[messages.MetadataKeySwarmMessages].([]string); ok {
			for _, id := range ids {
				if !seen[id] {
					out.MailIDs = append(out.MailIDs, id)
					seen[id] = true
				}
			}
		}
		if ids, ok := m.Metadata[messages.MetadataKeySwarmPublications].([]string); ok {
			out.PublicationIDs = append(out.PublicationIDs, ids...)
		}
	}
	return out
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
	return coordsem.DeliveryNext(n)
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
