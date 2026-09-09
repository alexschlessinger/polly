package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/google/jsonschema-go/jsonschema"
)

const completionToolName = "swarm_complete"
const resultCorrectionKey = "swarm_result_correction"
const maxResultCorrections = 2

type completionCallKey struct{}

// Tool callbacks finish before the loop's continuation/checkpoint callbacks.
// Completion is exclusive, so no two tool goroutines modify this state.
type structuredResultState struct {
	validator       *jsonschema.Resolved
	toolValueSchema map[string]any
	toolEnabled     bool
	task            string
	corrections     int
	pendingInput    string
	correctionText  string
	lastError       string
	candidate       *StructuredCompletion
	accepted        *StructuredCompletion
	acceptedText    string
}

func newStructuredResult(e *Execution, task string, toolEnabled bool) (*structuredResultState, error) {
	data, err := json.Marshal(e.Request.Schema)
	if err != nil {
		return nil, err
	}
	var raw jsonschema.Schema
	if err = json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("result schema: %w", err)
	}
	compiled, err := raw.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("result schema: %w", err)
	}
	var toolValueSchema map[string]any
	if err = json.Unmarshal(data, &toolValueSchema); err != nil {
		return nil, err
	}
	rebaseCompletionRefs(toolValueSchema)
	return &structuredResultState{validator: compiled, toolValueSchema: toolValueSchema, task: task, toolEnabled: toolEnabled, corrections: e.ResultCorrections, pendingInput: e.PendingResultCorrection}, nil
}

// Local pointers must still address the original result schema after nesting
// it in tool parameters. Walk schema keywords only, never literal JSON values
// in const/default/examples. A nested $id establishes its own reference base.
func rebaseCompletionRefs(node map[string]any) {
	if id, _ := node["$id"].(string); id != "" {
		return
	}
	for _, key := range []string{"$ref", "$dynamicRef", "$recursiveRef"} {
		if ref, _ := node[key].(string); ref == "#" || strings.HasPrefix(ref, "#/") {
			node[key] = "#/properties/value" + strings.TrimPrefix(ref, "#")
		}
	}
	visit := func(value any) {
		if child, ok := value.(map[string]any); ok {
			rebaseCompletionRefs(child)
		}
	}
	for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies"} {
		if children, ok := node[key].(map[string]any); ok {
			for _, child := range children {
				visit(child)
			}
		}
	}
	for _, key := range []string{"items", "prefixItems", "allOf", "anyOf", "oneOf", "additionalItems", "additionalProperties", "unevaluatedItems", "unevaluatedProperties", "contains", "propertyNames", "not", "if", "then", "else", "contentSchema"} {
		if children, ok := node[key].([]any); ok {
			for _, child := range children {
				visit(child)
			}
		} else {
			visit(node[key])
		}
	}
}

func (s *structuredResultState) decode(text string, wrapped bool) (any, error) {
	v, err := schema.DecodeJSON(text)
	if err != nil {
		return nil, err
	}
	if wrapped {
		object, ok := v.(map[string]any)
		if !ok || len(object) != 1 {
			return nil, errors.New("expected exactly one argument: value")
		}
		v, ok = object["value"]
		if !ok {
			return nil, errors.New("missing value argument")
		}
	}
	if err = s.validator.Validate(v); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *structuredResultState) register(registry *tools.ToolRegistry) {
	registry.Register(&tools.Func{
		Name: completionToolName, Exclusive: true, Strict: true,
		Desc:   "Finish this execution with the requested typed value and submit it for parent review. Complete the investigation first. This must be the only call in its batch; publications are progress, not completion.",
		Params: schema.Params{"value": s.toolValueSchema}, Required: []string{"value"},
		Run: func(ctx context.Context, _ tools.Args) (string, error) {
			call, ok := ctx.Value(completionCallKey{}).(messages.ChatMessageToolCall)
			if !ok {
				return "", errors.New("completion requires its active execution")
			}
			if _, err := s.decode(call.Arguments, true); err != nil {
				return "", fmt.Errorf("invalid completion value: %w", err)
			}
			return "Validated completion received; the runtime will submit the result for parent review.", nil
		},
	})
	registry.MarkAlwaysAllowed(completionToolName)
}

func (s *structuredResultState) guidance(resultSchema map[string]any) string {
	if s.toolEnabled {
		return "Complete the assigned work using tools as needed. Finish this execution by calling swarm_complete with the requested value, alone in its batch. The runtime submits that value for parent review. swarm_publish records progress, not the final return value. Do not claim another task or finish with prose instead of swarm_complete."
	}
	encoded, _ := json.Marshal(resultSchema)
	return "Tools are disabled. Return only a JSON value matching this result schema, without Markdown fences or surrounding prose: " + string(encoded)
}

func addResultGuidance(req *llm.CompletionRequest, guidance string) {
	req.Messages = append([]messages.ChatMessage(nil), req.Messages...)
	if len(req.Messages) > 0 && req.Messages[0].Role == messages.MessageRoleSystem {
		req.Messages[0].Content += "\n\n" + guidance
	} else {
		req.Messages = append([]messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: guidance}}, req.Messages...)
	}
}

func (s *structuredResultState) bind(cb *llm.AgentCallbacks) {
	admit := cb.AdmitInput
	cb.AdmitInput = func(ctx context.Context) ([]messages.ChatMessage, error) {
		var input []messages.ChatMessage
		if admit != nil {
			var err error
			input, err = admit(ctx)
			if err != nil {
				return nil, err
			}
		}
		if s.pendingInput != "" {
			input = append(input, s.correctionMessage(s.pendingInput))
			// Failed admission stops the slice; durable pending input survives
			// until the checkpoint actually includes this message.
			s.pendingInput = ""
		}
		return input, nil
	}
	before := cb.BeforeToolExecute
	cb.BeforeToolExecute = func(ctx context.Context, call messages.ChatMessageToolCall, args map[string]any) context.Context {
		if before != nil {
			ctx = before(ctx, call, args)
		}
		if call.Name == completionToolName {
			ctx = context.WithValue(ctx, completionCallKey{}, call)
		}
		return ctx
	}
	usage := cb.OnIterationUsage
	cb.OnIterationUsage = func(iteration, input, output int) {
		s.candidate, s.accepted = nil, nil
		s.lastError = "missing typed completion; finish with " + completionToolName
		if usage != nil {
			usage(iteration, input, output)
		}
	}
	result := cb.OnToolResult
	cb.OnToolResult = func(call messages.ChatMessageToolCall, receipt messages.ChatMessage) {
		if call.Name == completionToolName {
			if success, known := receipt.ToolSucceeded(); known && success {
				value, err := s.decode(call.Arguments, true)
				if err == nil {
					s.candidate = &StructuredCompletion{Task: s.task, CallID: call.ID, Value: value}
				} else {
					s.lastError = err.Error()
				}
			} else {
				s.lastError = receipt.Content
			}
		}
		if result != nil {
			result(call, receipt)
		}
	}
	prior := cb.ContinueAfterFinal
	cb.ContinueAfterFinal = func(ctx context.Context, response *messages.ChatMessage) ([]messages.ChatMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if prior != nil {
			input, err := prior(ctx, response)
			if err != nil || len(input) != 0 {
				return input, err
			}
		}
		if response != nil && response.StopReason == messages.StopReasonMaxTokens {
			return nil, errors.New("structured completion was truncated")
		}
		if !s.toolEnabled && response != nil {
			if len(response.ToolCalls) != 0 {
				return nil, errors.New("tool execution is disabled")
			}
			value, err := s.decode(response.Content, false)
			if err == nil {
				s.candidate = &StructuredCompletion{Task: s.task, Value: value}
				s.acceptedText = response.Content
			} else {
				s.lastError = err.Error()
			}
		}
		if s.candidate != nil {
			s.accepted = s.candidate
			return nil, nil
		}
		if s.corrections >= maxResultCorrections {
			return nil, fmt.Errorf("typed result invalid after two corrections: %s", s.lastError)
		}
		// Reserve the correction with the failed final's checkpoint. If the
		// call budget is exhausted, explicit recovery admits the saved prompt
		// before another model call instead of granting another repair.
		s.corrections++
		instruction := "Correct the result by calling swarm_complete with a value matching its schema. Reuse completed investigation; do not repeat successful tool work."
		if !s.toolEnabled {
			instruction = "Correct the result to JSON matching the supplied schema, without prose or Markdown."
		}
		s.correctionText = "Invalid final result: " + s.lastError + "\n" + instruction
		return []messages.ChatMessage{s.correctionMessage(s.correctionText)}, nil
	}
}

func (s *structuredResultState) correctionMessage(text string) messages.ChatMessage {
	return messages.ChatMessage{Role: messages.MessageRoleUser, Content: text,
		Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true, resultCorrectionKey: s.corrections}}
}

// applyCheckpoint runs inside the same transaction as receipts and usage.
func (s *structuredResultState) applyCheckpoint(state *State, e *Execution, appended []messages.ChatMessage) error {
	if e.Request.Schema == nil {
		return errors.New("structured execution lost its schema")
	}
	if s.corrections > e.ResultCorrections {
		e.ResultCorrections = s.corrections
		e.PendingResultCorrection = s.correctionText
	}
	for _, m := range appended {
		if m.Role == messages.MessageRoleUser && m.Metadata[messages.MetadataKeyAgentSynthetic] == true && m.Content == e.PendingResultCorrection {
			e.PendingResultCorrection = ""
		}
	}
	if s.accepted == nil {
		return nil
	}
	task := state.Tasks[s.task]
	member := state.Members[e.Member]
	if task == nil || member == nil || member.Task != s.task || task.Execution != e.ID || task.Owner != e.Member || task.Status != "running" {
		return errors.New("typed completion task ownership changed")
	}
	for _, m := range appended {
		if s.toolEnabled {
			if success, known := m.ToolSucceeded(); m.Role == messages.MessageRoleTool && m.ToolName == completionToolName && m.ToolCallID == s.accepted.CallID && known && success {
				e.Completion = s.accepted
				return nil
			}
		} else if m.Role == messages.MessageRoleAssistant && len(m.ToolCalls) == 0 && m.Content == s.acceptedText {
			e.Completion = s.accepted
			return nil
		}
	}
	return errors.New("typed completion is missing its successful receipt")
}
