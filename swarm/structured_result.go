package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/google/jsonschema-go/jsonschema"
)

const completionToolName = "swarm_complete"
const resultCorrectionKey = "swarm_result_correction"
const maxResultCorrections = 2

// resultErrorBytes bounds a validation error quoted back to the model. The
// schema library embeds the offending value in several of its messages, and
// a large value would otherwise return to the model in the receipt and again
// in the correction, on top of the arguments it already sent.
const resultErrorBytes = 512

// resultIsFinal goes into every correction. A model unsure whether its value
// will parse this time may send a probe first ({"summary":"s"}); the probe
// validates and is delivered as the result.
const resultIsFinal = "The first value that validates is final, so never send a placeholder or a test value."

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
	if err = s.validator.Validate(v); err == nil {
		return v, nil
	}
	// A value sent as a JSON string is decoded once: the model wrapped the
	// value it meant to send. Coercion runs only after a failure, so a
	// string a schema accepts is never touched.
	if encoded, ok := v.(string); ok {
		if inner, decodeErr := schema.DecodeJSON(encoded); decodeErr == nil {
			innerErr := s.validator.Validate(inner)
			if innerErr == nil {
				return inner, nil
			}
			return nil, fmt.Errorf("value is a JSON string; decoding it gives a value that is also invalid: %w", s.resultError(inner, innerErr))
		}
	}
	return nil, s.resultError(v, err)
}

// resultError renders a validation failure without quoting the value: a root
// type mismatch is reported by type alone, anything else is the library's
// message with its middle elided. The tail survives because that is where
// the library puts its diagnosis.
func (s *structuredResultState) resultError(v any, err error) error {
	if msg := s.rootTypeMismatch(v); msg != "" {
		return errors.New(msg)
	}
	return errors.New(elideMiddle(err.Error(), resultErrorBytes))
}

// rootTypeMismatch names the root type a value fails, or "" when the root
// declares no type or accepts the value's type.
func (s *structuredResultState) rootTypeMismatch(v any) string {
	root := s.validator.Schema()
	if root == nil {
		return ""
	}
	want := root.Types
	if root.Type != "" {
		want = []string{root.Type}
	}
	if len(want) == 0 {
		return ""
	}
	got := jsonTypeName(v)
	for _, name := range want {
		if name == got || name == "number" && got == "integer" {
			return ""
		}
	}
	wanted := strings.Join(want, " or ")
	msg := "value has type " + got + ", want " + wanted
	if got == "string" {
		msg += " (send the " + wanted + " itself, not a JSON string)"
	}
	return msg
}

func jsonTypeName(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		if x == float64(int64(x)) {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// elideMiddle keeps the head and tail of text within limit bytes, cutting on
// rune boundaries, with a marker counting the omitted bytes.
func elideMiddle(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	head := limit / 2
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	tail := len(text) - (limit - limit/2)
	for tail < len(text) && !utf8.RuneStart(text[tail]) {
		tail++
	}
	return fmt.Sprintf("%s ... [%d bytes elided] ... %s", text[:head], tail-head, text[tail:])
}

func (s *structuredResultState) register(registry *tools.ToolRegistry) {
	registry.Register(&tools.Func{
		Name: completionToolName, Exclusive: true, Strict: true,
		Desc:   "Finish this execution with the requested typed value under the task's completion requirement. Complete the investigation first. The first value that validates is final and is what gets delivered: send the complete value, never a placeholder or a test value. Pass the value itself as the value argument, never a string containing its JSON. This must be the only call in its batch; publications are progress, not completion.",
		Params: schema.Params{"value": s.toolValueSchema}, Required: []string{"value"},
		Run: func(ctx context.Context, _ tools.Args) (string, error) {
			call, ok := ctx.Value(completionCallKey{}).(messages.ChatMessageToolCall)
			if !ok {
				return "", errors.New("completion requires its active execution")
			}
			if _, err := s.decode(call.Arguments, true); err != nil {
				return "", fmt.Errorf("invalid completion value: %w", err)
			}
			return "Validated completion received; the runtime will deliver it or submit it for review according to the task requirement.", nil
		},
	})
	registry.MarkAlwaysAllowed(completionToolName)
}

func (s *structuredResultState) guidance(resultSchema map[string]any) string {
	if s.toolEnabled {
		return "Complete the assigned work using tools as needed. Finish this execution by calling swarm_complete with the requested value, alone in its batch; pass the value itself as the value argument, never a string containing its JSON. The runtime delivers or submits that value according to the task's completion requirement. Use swarm_publish only for findings or artifacts another worker needs during ongoing work; final results need no separate publication. Do not claim another task or finish with prose instead of swarm_complete."
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
				s.lastError = elideMiddle(receipt.Content, resultErrorBytes)
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
		// The model learns how many attempts remain: the last correction
		// says so, so it does not spend it on a probe.
		countdown := fmt.Sprintf("Correction %d of %d; ", s.corrections, maxResultCorrections)
		switch remaining := maxResultCorrections - s.corrections; {
		case remaining == 1:
			countdown += "1 more correction remains after this one, then an invalid result fails the task."
		case remaining > 1:
			countdown += fmt.Sprintf("%d more corrections remain after this one, then an invalid result fails the task.", remaining)
		default:
			countdown += "the next invalid result fails the task."
		}
		instruction := "Correct the result by calling swarm_complete with the complete value matching its schema. " + resultIsFinal + " Reuse completed investigation; do not repeat successful tool work."
		if !s.toolEnabled {
			instruction = "Correct the result to JSON matching the supplied schema, without prose or Markdown. " + resultIsFinal
		}
		s.correctionText = "Invalid final result: " + s.lastError + "\n" + countdown + " " + instruction
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
