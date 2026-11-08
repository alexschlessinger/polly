package sessions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/coordsem"
)

type statusGates struct {
	run, apply string
	running    bool
}

func (s statusStore) gates() (g statusGates, err error) {
	g.run, err = s.run()
	if err != nil {
		return g, err
	}
	g.apply, err = s.first(statusUncertain, "")
	if err != nil {
		return g, err
	}
	g.running, err = s.running()
	return g, err
}
func statusKind(kind string) bool {
	switch kind {
	case "task", "member", "execution", "workflow", "workflow_step", "mail", "integration", "apply", "snapshot", "run":
		return true
	}
	return false
}

// statusInputsChanged excludes only member workspace attachment and saved sandbox
// floors. Neither feeds status rules, so release/restoration must not revisit all
// historical tasks owned by the member. Compare every other field conservatively
// (including unknown fields); additions, deletions and real status input changes
// still follow the complete reverse dependency closure. Compact display sources
// are refreshed separately even when status inputs are unchanged.
func statusInputsChanged(key RecordKey, before, after []byte) (bool, error) {
	if !statusKind(key.Kind) {
		return false, nil
	}
	if key.Kind != "member" || before == nil || after == nil {
		return true, nil
	}
	var old, fresh map[string]json.RawMessage
	if err := json.Unmarshal(before, &old); err != nil {
		return false, err
	}
	if err := json.Unmarshal(after, &fresh); err != nil {
		return false, err
	}
	for _, field := range []string{"context", "sandboxPresets"} {
		delete(old, field)
		delete(fresh, field)
	}
	return !maps.EqualFunc(old, fresh, func(a, b json.RawMessage) bool { return bytes.Equal(a, b) }), nil
}

func (s statusStore) affected(key RecordKey, payload []byte, affected map[RecordKey]bool) error {
	if !statusKind(key.Kind) {
		return nil
	}
	source := s.source()
	d := derivedEngine{store: s.sqliteDerivedStore, parent: source.parent}
	queue := []RecordKey{key}
	seen := map[RecordKey]bool{}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if seen[key] {
			continue
		}
		seen[key] = true
		affected[key] = true
		add := func(kind, id string, follow bool) {
			if id != "" {
				k := RecordKey{kind, id}
				affected[k] = true
				if follow {
					queue = append(queue, k)
				}
			}
		}
		var reverse []string
		switch key.Kind {
		case "task":
			reverse = []string{"task", "member", "integration"}
		case "member":
			reverse = []string{"task"}
		case "execution":
			reverse = []string{"task", "member"}
		case "workflow":
			reverse = []string{"execution", "member", "task", "workflow_step"}
		case "snapshot":
			reverse = []string{"task", "integration"}
		}
		for _, kind := range reverse {
			if err := d.each(coordindex.OverviewDependency, coordindex.Key(key.Kind, key.ID), kind, func(item derivedItem) error { queue = append(queue, item.key); return nil }); err != nil {
				return err
			}
		}
		if key.Kind == "member" {
			if err := d.each(coordindex.Index("semantic.member-mail"), key.ID, "mail", func(item derivedItem) error { affected[item.key] = true; return nil }); err != nil {
				return err
			}
		}
		b, err := source.store.get(key)
		if err != nil {
			return err
		}
		if b == nil {
			continue
		}
		var v struct {
			Owner, Member, Execution, Task, Workflow, From, Run string
			Inputs, Repairs, Pending                            []struct{ Task string }
			Request                                             struct{ CallID string }
			ID                                                  string
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		switch key.Kind {
		case "task":
			add("member", v.Owner, true)
			add("run", v.Run, false)
		case "member":
			add("execution", v.Execution, true)
		case "execution":
			add("member", v.Member, true)
			w, _, err := source.statusExecutionWorkflow(key.ID)
			if err != nil {
				return err
			}
			add("workflow", w, false)
		case "mail":
			add("member", v.From, true)
			add("task", v.Task, true)
			add("workflow", v.Workflow, false)
		case "integration":
			for _, refs := range [][]struct{ Task string }{v.Inputs, v.Repairs, v.Pending} {
				for _, ref := range refs {
					add("task", ref.Task, true)
				}
			}
		case "apply":
			add("integration", key.ID, true)
		case "workflow_step":
			w, _, _ := strings.Cut(key.ID, "/")
			add("workflow", w, false)
			if v.ID != "" {
				if err := d.each(coordindex.ExecutionCallID, v.ID, "execution", func(item derivedItem) error { queue = append(queue, item.key); return nil }); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (s statusStore) refresh(affected map[RecordKey]bool, before statusGates) error {
	d := derivedEngine{store: s.sqliteDerivedStore, parent: s.source().parent}
	// These primitives supply aggregate inputs consumed by workflow/task/member
	// rules. No source record is rewritten through a display projection.
	refresh := func(kinds ...string) error {
		for _, kind := range kinds {
			var ids []string
			for key := range affected {
				if key.Kind == kind {
					ids = append(ids, key.ID)
				}
			}
			sort.Strings(ids)
			for _, id := range ids {
				key := RecordKey{kind, id}
				f, err := s.derive(key)
				if err != nil {
					return err
				}
				if err = s.save(key, f); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := refresh("execution", "mail", "workflow_step", "workflow", "apply"); err != nil {
		return err
	}
	after, err := s.gates()
	if err != nil {
		return err
	}
	if before.run != after.run {
		// Run changes are an explicit proportional exception. Ordinary writes
		// never scan a kind. All retained tasks with invalidated deferrals remain
		// eligible regardless of their run.
		for _, kind := range []string{"task", "workflow"} {
			if err := d.each(coordindex.Kind, kind, kind, func(item derivedItem) error { affected[item.key] = true; return nil }); err != nil {
				return err
			}
		}
		if err := refresh("workflow"); err != nil {
			return err
		}
	}
	if before.running != after.running || before.apply != after.apply {
		if err := d.each(statusOpenTask, "", "task", func(item derivedItem) error { affected[item.key] = true; return nil }); err != nil {
			return err
		}
		if err := d.each(coordindex.MemberLifecycle, "waiting", "member", func(item derivedItem) error { affected[item.key] = true; return nil }); err != nil {
			return err
		}
	}
	if err := refresh("task", "member", "run"); err != nil {
		return err
	}
	return s.syncBudget()
}

func (s statusStore) budgetFact(id string) (out statusFact, err error) {
	r, err := semanticGet[struct {
		ID, Status    string
		Starts, Limit int
	}](s.source(), "run", id)
	if err != nil || r == nil || r.Status != "paused" {
		return out, err
	}
	open, err := s.total("run-open", id)
	if err != nil {
		return out, err
	}
	notDeferred, err := s.total("run-open-not-deferred", id)
	if err != nil {
		return out, err
	}
	if open > 0 && notDeferred == 0 {
		return out, nil
	}
	out.decision("budgets", "", 0, coordindex.Order{Kind: "status", ID: id}, coordsem.DecisionItem{Kind: "budget", ID: r.ID, Label: "run " + r.ID, Why: fmt.Sprintf("execution budget exhausted (%d/%d)", r.Starts, r.Limit), Action: "tell the user; only a user-directed /swarm grant N extends it"})
	return out, nil
}
