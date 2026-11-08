package swarm

import (
	"encoding/json"
	"sync"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

// workflowDeliveries stages only results actually produced by the tool runner.
// It belongs to one parent turn, never to the runtime or persisted workflow.
// Tool callbacks may run concurrently; admission/checkpoint follow the batch.
type workflowDeliveries struct {
	mu      sync.Mutex
	pending map[workflowDelivery]workflowResultProof
}

type workflowResultProof struct {
	text  string
	parts []string
}

func workflowPartKey(part messages.ContentPart) string {
	if part.Artifact != nil {
		return part.Artifact.ID
	}
	data, _ := json.Marshal(part)
	return string(data)
}

func workflowDeliveryOf(m messages.ChatMessage) (workflowDelivery, bool) {
	if m.Role != messages.MessageRoleTool || m.ToolName != "workflow_run" || m.ToolCallID == "" {
		return workflowDelivery{}, false
	}
	data, ok := m.Metadata["tool_data"].(map[string]any)
	if !ok {
		return workflowDelivery{}, false
	}
	d := workflowDelivery{}
	d.Kind, _ = data["kind"].(string)
	d.Workflow, _ = data["workflow"].(string)
	d.CallID, _ = data["callID"].(string)
	d.Status, _ = data["status"].(string)
	return d, d.Kind == workflowDeliveryKind && d.Workflow != "" && d.CallID == m.ToolCallID && terminalWorkflow(d.Status)
}

func (d workflowDelivery) matches(s *State) bool {
	w := s.Workflows[d.Workflow]
	return w != nil && w.ID == d.Workflow && w.CallID == d.CallID && w.Status == d.Status && terminalWorkflow(w.Status)
}

func (p *workflowDeliveries) stage(call messages.ChatMessageToolCall, result messages.ChatMessage) {
	d, ok := workflowDeliveryOf(result)
	if !ok || call.Name != "workflow_run" || call.ID != d.CallID {
		return
	}
	proof := workflowResultProof{text: result.Content}
	for _, part := range result.Parts {
		proof.parts = append(proof.parts, workflowPartKey(part))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = map[workflowDelivery]workflowResultProof{}
	}
	p.pending[d] = proof
}

func (p *workflowDeliveries) suppress(s *State, mail *Mail) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for d := range p.pending {
		if mail.Workflow == d.Workflow && mail.From == d.Workflow && d.matches(s) {
			return true
		}
	}
	return false
}

func (proof workflowResultProof) retained(m messages.ChatMessage) bool {
	parts := map[string]bool{}
	for _, part := range m.Parts {
		parts[workflowPartKey(part)] = true
	}
	for _, required := range proof.parts {
		if !parts[required] {
			return false
		}
	}
	// Metadata alone, or the text without its attachments, is not delivery.
	return m.Content == proof.text
}

func (p *workflowDeliveries) record(s *State, actor string, appended []messages.ChatMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	calls := map[string]bool{}
	for _, m := range appended {
		if m.Role == messages.MessageRoleAssistant {
			for _, call := range m.ToolCalls {
				if call.Name == "workflow_run" {
					calls[call.ID] = true
				}
			}
		}
	}
	for _, m := range appended {
		d, ok := workflowDeliveryOf(m)
		proof, staged := p.pending[d]
		if !ok || !staged || !calls[d.CallID] || !d.matches(s) || !proof.retained(m) {
			continue
		}
		for _, mail := range s.Messages {
			if mail.To == actor && mail.From == d.Workflow && mail.Workflow == d.Workflow && !mail.Delivered {
				mail.Delivered = true
				recordDelivery(s, mail)
			}
		}
	}
}

func (d workflowDelivery) matchesHeader(w *workflow.ReportHeader) bool {
	return w != nil && w.ID == d.Workflow && w.CallID == d.CallID && w.Status == d.Status && terminalWorkflow(w.Status)
}

func (p *workflowDeliveries) suppressHeader(mail *Mail, w *workflow.ReportHeader) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for d := range p.pending {
		if mail.Workflow == d.Workflow && mail.From == d.Workflow && d.matchesHeader(w) {
			return true
		}
	}
	return false
}

func (p *workflowDeliveries) recordRecords(tx *recordTx, actor string, appended []messages.ChatMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	calls := map[string]bool{}
	for _, m := range appended {
		if m.Role == messages.MessageRoleAssistant {
			for _, call := range m.ToolCalls {
				if call.Name == "workflow_run" {
					calls[call.ID] = true
				}
			}
		}
	}
	for _, m := range appended {
		d, ok := workflowDeliveryOf(m)
		proof, staged := p.pending[d]
		if !ok || !staged || !calls[d.CallID] || !proof.retained(m) {
			continue
		}
		w, err := tx.WorkflowHeader(d.Workflow)
		if err != nil {
			return err
		}
		if !d.matchesHeader(w) {
			continue
		}
		q := sessions.RecordQuery{Index: sessions.CoordinationIndexOverviewDependency, Key: sessions.CoordinationIndexKey("workflow", d.Workflow), Limit: admissionMessages}
		for {
			page, err := tx.Mails(q)
			if err != nil {
				return err
			}
			for _, entry := range page.Records {
				mail := entry.Value
				if mail.To != actor || mail.From != d.Workflow || mail.Workflow != d.Workflow || mail.Delivered {
					continue
				}
				deps, err := tx.Checkpoint("", admissionReceipts{MailIDs: []string{mail.ID}})
				if err != nil {
					return err
				}
				mail.Delivered = true
				if err := tx.recordDelivery(deps, mail); err != nil {
					return err
				}
				if err := tx.PutMail(mail); err != nil {
					return err
				}
			}
			if page.Next == "" {
				break
			}
			q.After = page.Next
		}
	}
	return nil
}

func (p *workflowDeliveries) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.pending)
}
