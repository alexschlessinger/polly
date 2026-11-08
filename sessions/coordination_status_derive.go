package sessions

import (
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/coordsem"
)

func (s statusStore) classify(f statusTaskBase, run, apply string, running bool) (coordsem.Bucket, string, int, error) {
	t := f.task
	in := coordsem.Classification{Status: t.Status, Unchanged: coordsem.AcceptedUnchanged(&t.Task, f.deferred, f.unchanged), Running: running, FirstApply: apply}
	controlled, err := s.source().controlled(f.execution)
	if err != nil {
		return 0, "", 0, err
	}
	in.Controlled = controlled
	if f.delivering && !f.deferred {
		// Existence and pending delivery are intentionally different inputs.
		err = (derivedEngine{store: s.sqliteDerivedStore}).each(coordindex.OverviewDependency, coordindex.Key("task", t.ID), "mail", func(item derivedItem) error {
			m, err := semanticGet[struct {
				Task, Execution string
				Revision        int
			}](s.source(), "mail", item.key.ID)
			if err != nil {
				return err
			}
			in.Delivering = in.Delivering || m != nil && m.Task == t.ID && m.Execution == t.Execution && m.Revision == t.Revision
			return nil
		})
		if err != nil {
			return 0, "", 0, err
		}
	}
	if o, e := f.owner, f.execution; o != nil && e != nil && t.Execution != "" && o.Task == t.ID && o.Execution == t.Execution {
		in.Advancing = t.Status == "running" && coordsem.ExecutionRunning(e.Status)
		in.Parked = e.Status == "waiting"
	}
	if in.Parked {
		n, err := s.total("asking", coordindex.Key(s.source().parent, t.Owner))
		if err != nil {
			return 0, "", 0, err
		}
		in.Asking = n > 0
	}
	for _, id := range t.Dependencies {
		dep, err := s.source().taskBase(id)
		if err != nil {
			return 0, "", 0, err
		}
		if dep.task != nil && dep.task.Status == "done" {
			continue
		}
		d := coordsem.Dependency{ID: id, Canceled: dep.task == nil || dep.task.Status == "canceled"}
		if dep.task != nil {
			d.Open = coordsem.Unsettled(&dep.task.Task, run, dep.deferred)
		}
		in.Dependencies = append(in.Dependencies, d)
	}
	b, reason := coordsem.Classify(in)
	return b, reason, len(in.Dependencies), nil
}
func (s statusStore) derive(key RecordKey) (out statusFact, err error) {
	source := s.source()
	d := derivedEngine{store: s.sqliteDerivedStore, parent: source.parent}
	switch key.Kind {
	case "run":
		return s.budgetFact(key.ID)
	case "execution":
		e, err := source.execution(key.ID)
		if err != nil || e == nil {
			return out, err
		}
		if e.Status != "queued" && e.Status != "running" && e.Status != "waiting" {
			return out, nil
		}
		out.add("inspector-run-active", e.Run)
		workflow, cold, err := source.statusExecutionWorkflow(key.ID)
		if err != nil {
			return out, err
		}
		if cold {
			out.add("cold", "")
		}
		if workflow != "" {
			out.add("agents", workflow)
			if e.Status == "waiting" {
				out.add("waiting-agents", workflow)
			}
		}
	case "mail":
		m, err := semanticGet[struct {
			ID, From, To, Kind, ReplyID, Workflow, Text string
			Delivered                                   bool
		}](source, "mail", key.ID)
		if err != nil || m == nil {
			return out, err
		}
		b, err := source.store.get(key)
		if err != nil {
			return out, err
		}
		_, order, err := coordindex.Extract("mail", key.ID, b)
		if err != nil {
			return out, err
		}
		if !m.Delivered && m.Workflow != "" {
			out.add("workflow-notice", coordindex.Key(m.Workflow, m.To))
		}
		if m.Kind == "request" && m.ReplyID == "" || m.Kind == "reply" && !m.Delivered {
			_, state, err := source.memberFacts(m.From)
			if err != nil {
				return out, err
			}
			item := coordsem.DecisionItem{Kind: "mail", ID: m.ID, Member: m.From, State: state.Display}
			group := byte(2)
			if m.Kind == "request" {
				group = 1
				out.add("asking", coordindex.Key(m.To, m.From))
				item.Label, item.Why, item.Action = "request "+m.ID+" from "+m.From, coordsem.Clip(m.Text, 160), fmt.Sprintf("send_message {target: %q, message: <reply>}", m.From)
				out.decision("requests", m.To, group, order, item)
			} else {
				item.Label, item.Why, item.Action = "reply "+m.ID+" from "+m.From, "unread answer to your request", fmt.Sprintf("swarm_read({view: %q, id: %q})", "messages", m.ID)
			}
			if m.To == source.parent {
				out.decision("decisions", "", group, order, item)
			}
		}
	case "workflow_step":
		b, err := source.store.get(key)
		if err != nil || b == nil {
			return out, err
		}
		workflow, _, _ := strings.Cut(key.ID, "/")
		w, err := semanticGet[coordsem.Workflow](source, "workflow", workflow)
		if err != nil {
			return out, err
		}
		if w == nil {
			out.add("invalid-workflow", "")
		}
	case "workflow":
		w, err := semanticGet[struct {
			coordsem.Workflow
			Name string
		}](source, "workflow", key.ID)
		if err != nil || w == nil {
			return out, err
		}
		run, err := s.run()
		if err != nil {
			return out, err
		}
		stepCount, invalid := int64(0), false
		if err := d.each(coordindex.WorkflowSteps, key.ID, "workflow_step", func(item derivedItem) error {
			invalid = invalid || item.order.Step != stepCount
			stepCount++
			return nil
		}); err != nil {
			return out, err
		}
		if invalid {
			out.add("invalid-workflow", "")
		}
		if w.Status == "running" {
			steps, host, display := 0, false, false
			err = d.each(coordindex.WorkflowSteps, key.ID, "workflow_step", func(item derivedItem) error {
				step, err := semanticGet[struct{ Kind, Status string }](source, "workflow_step", item.key.ID)
				if err != nil {
					return err
				}
				if step != nil {
					steps++
					host = host || coordsem.WorkStepRunning(step.Kind, step.Status)
					display = display || coordsem.DisplayStepRunning(step.Kind, step.Status)
				}
				return nil
			})
			if err != nil {
				return out, err
			}
			if steps == 0 || host {
				out.add("host-running", "")
			}
			agents, err := s.total("agents", w.ID)
			if err != nil {
				return out, err
			}
			waiting, err := s.total("waiting-agents", w.ID)
			if err != nil {
				return out, err
			}
			state := "running"
			if agents > 0 && waiting == agents && !display {
				state = "parked"
			}
			label := coordsem.Clip(w.Name, 160)
			if label == "" {
				label = w.ID
			}
			out.working("working", 0, key.ID, coordsem.WorkingItem{Kind: "workflow", ID: w.ID, Label: label, State: state, Agents: agents})
		} else if !w.Acknowledged {
			if w.Status == "completed" {
				out.add("disposition-delivering", "")
				n, err := s.total("workflow-notice", coordindex.Key(w.ID, source.parent))
				if err != nil {
					return out, err
				}
				if n > 0 {
					out.working("working", 1, key.ID, coordsem.WorkingItem{Kind: "workflow", ID: w.ID, Label: "workflow " + w.ID, State: "delivering"})
					out.add("deliveries", "")
				}
			} else if w.Run == run {
				out.decision("decisions", "", 5, coordindex.Order{Kind: "status", ID: key.ID}, coordsem.DecisionItem{Kind: "workflow", ID: w.ID, Label: "workflow " + w.ID, Why: w.Status, Action: coordsem.WorkflowNext(w.Workflow)})
			}
		}
	case "apply":
		a, err := semanticGet[struct{ ID, Status string }](source, "apply", key.ID)
		if err != nil || a == nil {
			return out, err
		}
		if coordsem.UncertainApply(a.Status) {
			out.decision("decisions", "", 0, coordindex.Order{Kind: "status", ID: key.ID}, coordsem.DecisionItem{Kind: "integration", ID: a.ID, Label: "integration " + a.ID, Why: "outcome unconfirmed", Action: fmt.Sprintf("use workflow_run with polly.integration.reconcile(%q) to observe its outcome", a.ID)})
		}
	case "task":
		f, err := source.taskBase(key.ID)
		if err != nil || f.task == nil {
			return out, err
		}
		t := f.task
		done, deferred := coordsem.TaskCounts(t.Task, t.Run, f.deferred)
		if done != 0 {
			out.add("done", t.Run)
		}
		if deferred != 0 {
			out.add("deferred", "")
			if t.Deferral != nil {
				out.add("inspector-workflow-deferred", t.Deferral.Workflow)
			}
		}
		if t.Status != "done" && t.Status != "canceled" {
			out.add("run-open", t.Run)
			if !f.deferred {
				out.add("run-open-not-deferred", t.Run)
			}
		}
		c, err := source.candidate(t)
		if err != nil {
			return out, err
		}
		display := coordsem.TaskDisplay(&t.Task, f.delivering, f.editing, c != nil && c.Status == "conflicted")
		disp := coordsem.TaskDisposition(t.Task, display, f.deferred)
		for name, on := range map[string]bool{"review": disp.Review, "integration": disp.Integration, "halted": disp.Halted, "open": disp.Open, "delivering": disp.Delivering} {
			if on {
				out.add("disposition-"+name, "")
			}
		}
		run, err := s.run()
		if err != nil {
			return out, err
		}
		if !coordsem.Unsettled(&t.Task, run, f.deferred) {
			return out, nil
		}
		apply, err := s.first(statusUncertain, "")
		if err != nil {
			return out, err
		}
		running, err := s.running()
		if err != nil {
			return out, err
		}
		bucket, reason, deps, err := s.classify(f, run, apply, running)
		if err != nil {
			return out, err
		}
		// Delivery is an obligation even if a workflow folds this task's row.
		if f.delivering && !f.deferred {
			notice := false
			err = d.each(coordindex.OverviewDependency, coordindex.Key("task", t.ID), "mail", func(item derivedItem) error {
				m, err := semanticGet[struct {
					Task, Execution string
					Revision        int
				}](source, "mail", item.key.ID)
				if err == nil && m != nil && m.Task == t.ID && m.Execution == t.Execution && m.Revision == t.Revision {
					notice = true
				}
				return err
			})
			if err != nil {
				return out, err
			}
			if notice {
				out.add("deliveries", "")
			}
		}
		switch bucket {
		case coordsem.Done:
			out.add("repaired", "")
		case coordsem.Working:
			out.working("working", 3, key.ID, coordsem.WorkingItem{Kind: "task", ID: t.ID, Label: "task " + t.ID, State: reason, Task: t.ID})
		case coordsem.Decision:
			var item coordsem.DecisionItem
			if c != nil {
				for _, ref := range append(c.Inputs, c.Repairs...) {
					if ref.Task >= t.ID {
						continue
					}
					other, err := source.taskBase(ref.Task)
					if err != nil {
						return out, err
					}
					if other.task == nil || !coordsem.Unsettled(&other.task.Task, run, other.deferred) {
						continue
					}
					b, _, _, err := s.classify(other, run, apply, running)
					if err != nil {
						return out, err
					}
					if b != coordsem.Decision {
						continue
					}
					selected, err := source.candidate(other.task)
					if err != nil {
						return out, err
					}
					if selected != nil && selected.ID == c.ID {
						return out, nil
					}
				}
				why := "ready"
				if c.Status == "conflicted" {
					why = "halted · conflicted"
				}
				item = coordsem.DecisionItem{Kind: "integration", ID: c.ID, Label: "integration " + c.ID, Why: why, Action: statusCandidateAction(c)}
			} else {
				item, err = source.taskDecision(f, nil, reason, deps)
				if err != nil {
					return out, err
				}
				if item.Why == "candidate ready" {
					out.decision("ready", "", 0, coordindex.Order{Kind: "status", ID: key.ID}, item)
				}
			}
			out.decision("decisions", "", 4, coordindex.Order{Kind: "status", ID: key.ID}, item)
		}
	case "member":
		m, p, err := source.memberFacts(key.ID)
		if err != nil || m == nil {
			return out, err
		}
		delivery, dormant := coordsem.MemberCounts(m.Control, p.Delivering, p.Busy, p.Attention)
		if delivery != 0 {
			out.add("delivering", "")
		}
		if dormant != 0 {
			out.add("dormant", "")
		}
		if !p.Busy {
			return out, nil
		}
		label := m.Name
		if m.Label != "" {
			label = coordsem.Clip(m.Label, 160)
		}
		item := coordsem.WorkingItem{Kind: "member", ID: m.ID, Label: label, State: p.Display, Task: m.Task}
		out.working("teammates", 0, key.ID, item)
		e, err := source.execution(m.Execution)
		if err != nil {
			return out, err
		}
		controlled, err := source.controlled(e)
		if err != nil {
			return out, err
		}
		if !controlled && m.Controller != "" {
			w, err := semanticGet[coordsem.Workflow](source, "workflow", m.Controller)
			if err != nil {
				return out, err
			}
			controlled = w != nil && w.Status == "running"
		}
		if controlled {
			return out, nil
		}
		if p.Lifecycle == "waiting" {
			asking, err := s.total("asking", coordindex.Key(source.parent, m.ID))
			if err != nil {
				return out, err
			}
			running, err := s.running()
			if err != nil {
				return out, err
			}
			if asking > 0 || !running {
				return out, nil
			}
		}
		out.working("working", 2, key.ID, item)
	}
	return out, nil
}
