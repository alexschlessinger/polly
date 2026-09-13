package swarm

import (
	"encoding/json"
	"sync"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// workflowDeliveries stages only results actually produced by the tool runner.
// It belongs to one parent turn, never to the runtime or persisted workflow.
// Tool callbacks may run concurrently; admission/checkpoint follow the batch.
type workflowDeliveries struct {
	mu      sync.Mutex
	pending map[workflowDelivery]workflowResultProof
}

type workflowResultProof struct {
	text         string
	textArtifact string
	parts        []string
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
	proof := workflowResultProof{text: result.Content, textArtifact: artifacts.RefForBlob(artifacts.Blob{Kind: artifacts.KindText, Data: []byte(result.Content)}).ID}
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
	// Context projection can replace inline text with an owned text artifact.
	// Metadata alone or a preview stripped of its full attachment is not delivery.
	return m.Content == proof.text || parts[proof.textArtifact]
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

func (p *workflowDeliveries) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.pending)
}
