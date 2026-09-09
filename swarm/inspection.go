package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

const inspectionBytes = 16 << 10

// The rich wrapper keeps full content in a text attachment. The agent stores
// and authorizes its receipt in the caller's own conversation, just like other
// tool artifacts. Direct Go callers retain the complete, readable text.
type inspectionTool struct {
	*tools.Func
}

func (t *inspectionTool) ExecuteOutput(ctx context.Context, args map[string]any) (tools.ToolOutput, error) {
	text, err := t.Execute(ctx, args)
	if len(text) <= inspectionBytes {
		return tools.ToolOutput{Text: text}, err
	}
	return tools.ToolOutput{
		Text:  clipInspection(text, inspectionBytes/2) + "\nFull selected content is attached. Use read_artifact with its receipt ID: offset/limit for lines, query for literal search, or byte_offset for exact byte paging.",
		Media: []tools.ToolMedia{{Data: []byte(text), MIMEType: "text/plain", Name: t.Name + ".json"}},
	}, err
}

func clipInspection(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "…"
}

func inspectionParams(extra schema.Params) schema.Params {
	if extra == nil {
		extra = schema.Params{}
	}
	extra["offset"] = schema.Int("1-based listing position (default 1)")
	extra["limit"] = schema.Int("Maximum listing entries (default 50, maximum 100); also bounded to 16 KiB")
	return extra
}

type inspectionPage struct {
	Items []any `json:"items"`
	Total int   `json:"total"`
	Next  int   `json:"next,omitempty"`
}

func pageInspection(items []any, a tools.Args) (any, error) {
	offset, limit := a.Int("offset", 1), a.Int("limit", 50)
	if offset < 1 || limit < 1 || limit > 100 {
		return nil, errors.New("offset must be positive and limit between 1 and 100")
	}
	page := inspectionPage{Items: []any{}, Total: len(items)}
	bytes := 128 // envelope and continuation reserve
	for i := min(offset-1, len(items)); i < len(items); i++ {
		data, err := json.MarshalIndent(items[i], "", "  ")
		if err != nil {
			return nil, err
		}
		if len(page.Items) >= limit || bytes+len(data)+256 > inspectionBytes-2048 {
			page.Next = i + 1
			break
		}
		page.Items = append(page.Items, items[i])
		bytes += len(data) + 256
	}
	return page, nil
}

func sortedInspectionIDs[T any](items map[string]T) []string {
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func taskSummary(s *State, t *Task) any {
	return map[string]any{"id": t.ID, "owner": t.Owner, "run": t.Run, "execution": t.Execution,
		"status": t.Status, "displayStatus": TaskStatus(t), "revision": t.Revision,
		"acceptedRevision": t.AcceptedRevision, "snapshot": t.Snapshot, "deferred": TaskDeferred(s, t),
		"description": clipInspection(t.Description, 512), "read": map[string]any{"task": t.ID, "section": "result"}}
}

func (r *Runtime) inspectTasks(ctx context.Context, a tools.Args) (any, error) {
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	if id := a.String("task"); id != "" {
		t := s.Tasks[id]
		if t == nil {
			return nil, errors.New("unknown task")
		}
		var value any
		switch a.String("section") {
		case "", "summary":
			value = taskSummary(s, t)
		case "details":
			copy := *t
			copy.Result = nil
			value = copy
		case "result":
			value = t.Result
			if e := s.Executions[t.Execution]; value == nil && e != nil && e.Result != nil {
				value = e.Result.Value
			}
		default:
			return nil, errors.New("task section must be summary, details or result")
		}
		return selectInspection(value, a.String("pointer"))
	}
	if a.String("section") != "" && a.String("section") != "summary" || a.String("pointer") != "" {
		return nil, errors.New("task selection is required for details, result or pointer")
	}
	items := []any{}
	for _, id := range sortedInspectionIDs(s.Tasks) {
		items = append(items, taskSummary(s, s.Tasks[id]))
	}
	return pageInspection(items, a)
}

func (r *Runtime) inspectAgents(ctx context.Context, actor string, a tools.Args) (any, error) {
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	items := []any{}
	for _, id := range sortedInspectionIDs(s.Members) {
		m := s.Members[id]
		items = append(items, map[string]any{"id": m.ID, "name": m.Name, "label": clipInspection(m.Label, 512), "context": m.Context, "task": m.Task, "execution": m.Execution, "state": MemberState(s, m), "readOnly": m.ReadOnly})
	}
	page, err := pageInspection(items, a)
	if err != nil {
		return nil, err
	}
	return struct {
		inspectionPage
		Self        string            `json:"self"`
		Parent      string            `json:"parent"`
		ParentState AgentPresentation `json:"parentState"`
	}{page.(inspectionPage), actor, r.ID, r.ParentState(s)}, nil
}

func stepSummary(step workflow.Step) any {
	var failure any
	if step.Error != nil {
		failure = map[string]any{"code": step.Error.Code, "message": clipInspection(step.Error.Message, 512)}
	}
	return map[string]any{"id": step.ID, "kind": step.Kind, "status": step.Status, "started": step.Started, "finished": step.Finished, "error": failure}
}

func (r *Runtime) inspectWorkflow(ctx context.Context, a tools.Args) (any, error) {
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	w := s.Workflows[a.String("id")]
	if w == nil {
		return nil, errors.New("unknown workflow")
	}
	var value any
	switch a.String("section") {
	case "", "summary":
		counts := map[string]int{}
		for _, step := range w.Steps {
			counts[step.Status]++
		}
		agents := []any{}
		for _, id := range sortedInspectionIDs(s.Executions) {
			e := s.Executions[id]
			if ExecutionWorkflow(s, e) == w.ID {
				task := ""
				if e.Result != nil {
					task = e.Result.Task
				}
				if task == "" {
					for _, t := range s.Tasks {
						if t.Execution == e.ID {
							task = t.ID
							break
						}
					}
				}
				agents = append(agents, map[string]any{"session": e.Member, "execution": e.ID, "task": task, "status": e.Status, "error": clipInspection(e.Error, 512)})
			}
		}
		page, pageErr := pageInspection(agents, a)
		if pageErr != nil {
			return nil, pageErr
		}
		var failure any
		if w.Error != nil {
			failure = map[string]any{"code": w.Error.Code, "message": clipInspection(w.Error.Message, 512)}
		}
		value = map[string]any{"id": w.ID, "name": clipInspection(w.Name, 512), "run": w.Run, "status": w.Status, "acknowledged": w.Acknowledged, "steps": len(w.Steps), "stepCounts": counts, "agents": page, "error": failure, "deferred": DeferredCount(s, w.ID), "started": w.Started, "finished": w.Finished, "read": "Use section=steps to list step IDs; section=step with step=<id> to inspect one. Sections source, input and output are also available; pointer selects a JSON Pointer within the selected value."}
	case "steps":
		items := []any{}
		for _, step := range w.Steps {
			items = append(items, stepSummary(step))
		}
		value, err = pageInspection(items, a)
		if err != nil {
			return nil, err
		}
	case "step":
		found := false
		for _, step := range w.Steps {
			if step.ID == a.String("step") {
				value, found = step, true
				break
			}
		}
		if !found {
			return nil, errors.New("unknown step in this workflow")
		}
	case "source":
		value = w.Source
	case "input":
		value = w.Input
	case "output":
		value = w.Output
	default:
		return nil, errors.New("workflow section must be summary, steps, step, source, input or output")
	}
	return selectInspection(value, a.String("pointer"))
}

// JSON Pointer is a selector, not an expression language. Round-trip with
// UseNumber so large integer results survive selection without float rounding.
func selectInspection(value any, pointer string) (any, error) {
	if pointer == "" {
		return value, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, errors.New("pointer must be empty or start with /")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err = decoder.Decode(&value); err != nil {
		return nil, err
	}
	for _, token := range strings.Split(pointer[1:], "/") {
		for i := 0; i < len(token); i++ {
			if token[i] == '~' {
				if i+1 >= len(token) || token[i+1] != '0' && token[i+1] != '1' {
					return nil, errors.New("invalid JSON Pointer escape")
				}
				i++
			}
		}
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch v := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = v[token]
			if !ok {
				return nil, fmt.Errorf("pointer key %q is unavailable", token)
			}
		case []any:
			i, e := strconv.Atoi(token)
			if e != nil || i < 0 || i >= len(v) || strconv.Itoa(i) != token {
				return nil, errors.New("pointer array index is unavailable")
			}
			value = v[i]
		default:
			return nil, errors.New("pointer cannot descend into a scalar")
		}
	}
	return value, nil
}
