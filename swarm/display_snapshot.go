package swarm

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

// DisplaySnapshot is a compact, immutable-by-convention display observation,
// deliberately NOT State. Missing source records confer no authority. It contains
// only current member descriptors, current task/execution previews, workflow
// launch identities, and the newest non-info mail involving each member.
// Launches preserves every historical call-ID/member edge, including ambiguous
// bindings; it does not retain historical execution intents or result payloads.
type DisplaySnapshot struct {
	Members                            map[string]*Member
	Executions                         map[string]*Execution
	Tasks                              map[string]*Task
	Workflows                          map[string]*workflow.Report
	Messages                           map[string]*Mail
	Status                             DisplayStatus
	Presentations                      map[string]AgentPresentation
	MemberDecisions, WorkflowDecisions map[string]int
	FirstMemberDecision                map[string]*DecisionItem
	Launches                           map[string][]string
	BusyMembers                        int
}

type DisplayView interface {
	DisplaySnapshot(parent string) *DisplaySnapshot
}

func (s *DisplaySnapshot) DisplaySnapshot(string) *DisplaySnapshot { return s }
func (s *State) DisplaySnapshot(parent string) *DisplaySnapshot {
	return ProjectDisplayState(s, parent)
}
func (s *DisplaySnapshot) MemberState(id string) AgentPresentation {
	if s == nil {
		return AgentPresentation{}
	}
	return s.Presentations[id]
}
func (s *DisplaySnapshot) ExecutionWorkflow(e *Execution) string {
	if s == nil || e == nil {
		return ""
	}
	if e.Workflow != "" {
		return e.Workflow
	}
	if m := s.Members[e.Member]; m != nil && m.Execution == e.ID {
		if p, ok := s.Presentations[m.ID]; ok {
			return p.Workflow
		}
		if m.Controller != "" {
			return m.Controller
		}
	}
	for id, w := range s.Workflows {
		for _, step := range w.Steps {
			if step.Kind == "agent" && step.ID == e.Request.CallID && e.Request.CallID != "" {
				return id
			}
		}
	}
	return ""
}
func emptyDisplaySnapshot() *DisplaySnapshot {
	return &DisplaySnapshot{Members: map[string]*Member{}, Executions: map[string]*Execution{}, Tasks: map[string]*Task{}, Workflows: map[string]*workflow.Report{}, Messages: map[string]*Mail{}, Presentations: map[string]AgentPresentation{}, MemberDecisions: map[string]int{}, WorkflowDecisions: map[string]int{}, FirstMemberDecision: map[string]*DecisionItem{}, Launches: map[string][]string{}}
}
func compactDisplay[T any](kind string, source *T) *T {
	if source == nil {
		return nil
	}
	raw, err := json.Marshal(source)
	if err != nil {
		return nil
	}
	raw, err = sessions.CompactDisplayRecord(kind, raw)
	if err != nil {
		return nil
	}
	var out T
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return &out
}

// ProjectDisplayState is the explicit compatibility adapter for old hosts and
// tests. All semantic decisions are computed from the COMPLETE source State,
// before compacting it. Never call State helpers on a DisplaySnapshot's records.
func ProjectDisplayState(s *State, parent string) *DisplaySnapshot {
	if s == nil {
		return nil
	}
	out := emptyDisplaySnapshot()
	p := Present(s, parent, parent)
	out.Status = DisplayStatus{Counts: p.Counts, HasRuns: len(s.Runs) > 0, ParentDisposition: parentDisposition(s)}
	if len(p.Decisions) > 0 {
		d := p.Decisions[0]
		out.Status.First = &d
	}
	if turn := s.ParentTurns[parent]; turn != nil && turn.RetryPause != nil {
		v := *turn.RetryPause
		out.Status.RetryPause = &v
	}
	for _, d := range p.Decisions {
		switch d.Kind {
		case KindWorkflow:
			out.WorkflowDecisions[d.ID]++
		case KindTask, KindMail:
			if d.Member != "" {
				out.MemberDecisions[d.Member]++
				if out.FirstMemberDecision[d.Member] == nil && !(d.Kind == KindMail && strings.HasPrefix(d.Label, "reply ")) {
					v := d
					out.FirstMemberDecision[d.Member] = &v
				}
			}
		}
	}
	for id, m := range s.Members {
		if m == nil {
			continue
		}
		out.Members[id] = compactDisplay("member", m)
		out.Presentations[id] = MemberState(s, m)
		if out.Presentations[id].Busy {
			out.BusyMembers++
		}
		if e := s.Executions[m.Execution]; e != nil {
			out.Executions[m.Execution] = compactDisplay("execution", e)
		}
		if t := s.Tasks[m.Task]; t != nil {
			out.Tasks[m.Task] = compactDisplay("task", t)
		}
	}
	for id, w := range s.Workflows {
		if w == nil {
			continue
		}
		v := compactDisplay("workflow", w)
		for _, step := range w.Steps {
			if step.Kind == "agent" {
				v.Steps = append(v.Steps, *compactDisplay("workflow_step", &step))
			}
		}
		out.Workflows[id] = v
	}
	latest := map[string]*Mail{}
	for _, m := range s.Messages {
		if m == nil || m.Kind == "info" {
			continue
		}
		for _, id := range []string{m.From, m.To} {
			if s.Members[id] == nil {
				continue
			}
			old := latest[id]
			if old == nil || m.Posted.After(old.Posted) || m.Posted.Equal(old.Posted) && m.ID > old.ID {
				latest[id] = m
			}
		}
	}
	for _, m := range latest {
		out.Messages[m.ID] = compactDisplay("mail", m)
	}
	for _, e := range s.Executions {
		if e != nil && e.Member != "" && e.Request.CallID != "" {
			out.Launches[e.Request.CallID] = append(out.Launches[e.Request.CallID], e.Member)
		}
	}
	for id, edges := range out.Launches {
		slices.Sort(edges)
		out.Launches[id] = edges
	}
	return out
}

type displayMemberBundle struct {
	member       *Member
	execution    *Execution
	task         *Task
	mail         *Mail
	presentation AgentPresentation
	decisions    int
	first        *DecisionItem
}
type displayWorkflowBundle struct {
	report    *workflow.Report
	decisions int
}

// DisplayCache rechecks a durable mark in constant work. A changed native read
// decodes only changed compact facts; rebuilding output maps is proportional to
// the actual roster/workflows/launch edges, never unrelated retained history.
// Old hosts use the explicit full-state compatibility adapter.
type DisplayCache struct {
	mu        sync.Mutex
	mark      string
	snapshot  *DisplaySnapshot
	members   map[string]displayMemberBundle
	workflows map[string]displayWorkflowBundle
	launches  map[string]sessions.DisplayLaunch
	status    DisplayStatus
	cold      bool
}

func (c *DisplayCache) Read(ctx context.Context, r *Runtime) (*DisplaySnapshot, error) {
	access, err := sessions.OpenCoordinationAccess(r.parent)
	if err != nil {
		return nil, err
	}
	return c.read(ctx, r.ID, func(fn func(sessions.CoordinationReader) error) error { return access.ReadRecords(ctx, fn) })
}
func (c *DisplayCache) ReadView(ctx context.Context, store any, root string) (*DisplaySnapshot, error) {
	access, err := sessions.OpenCoordinationViewAccess(store)
	if err != nil {
		return nil, err
	}
	return c.read(ctx, root, func(fn func(sessions.CoordinationReader) error) error { return access.ReadRecordsView(ctx, root, fn) })
}
func (c *DisplayCache) read(ctx context.Context, parent string, read func(func(sessions.CoordinationReader) error) error) (*DisplaySnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Work on a candidate cache so a callback/backend error never publishes staged
	// or rolled-back values, nor advances the reusable mark.
	next := DisplayCache{mark: c.mark, snapshot: c.snapshot, members: c.members, workflows: c.workflows, launches: c.launches, status: c.status, cold: c.cold}
	err := read(func(raw sessions.CoordinationReader) error {
		if reader, ok := raw.(sessions.CoordinationDisplayReader); ok {
			delta, err := reader.DisplayChanges(next.mark)
			if err != nil {
				return err
			}
			if delta.Mark == next.mark && next.snapshot != nil && !next.cold {
				return nil
			}
			previous := next.snapshot
			if err = next.apply(delta); err != nil {
				return err
			}
			if !next.cold {
				return nil
			}
			// Compare compatibility output with the last handed-out snapshot,
			// not the intermediate native projection of ambiguous provenance.
			next.snapshot = previous
		}
		rd := &recordReader{raw: raw, ctx: ctx, active: true}
		defer func() { rd.active = false }()
		state, err := rd.SlowSnapshot()
		if err != nil {
			return err
		}
		snapshot := ProjectDisplayState(state, parent)
		if !reflect.DeepEqual(snapshot, next.snapshot) {
			next.snapshot = snapshot
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.mark, c.snapshot, c.members, c.workflows, c.launches, c.status, c.cold = next.mark, next.snapshot, next.members, next.workflows, next.launches, next.status, next.cold
	return c.snapshot, nil
}
func decodeDisplay[T any](raw json.RawMessage) (*T, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var out T
	err := json.Unmarshal(raw, &out)
	return &out, err
}
func displayDecision(d *sessions.SemanticDecision) *DecisionItem {
	if d == nil {
		return nil
	}
	return &DecisionItem{Kind: d.Kind, ID: d.ID, Label: d.Label, Why: d.Why, Action: d.Action, Member: d.Member, State: d.State}
}
func (c *DisplayCache) apply(delta sessions.DisplayChanges) error {
	if delta.Reset {
		c.members = map[string]displayMemberBundle{}
		c.workflows = map[string]displayWorkflowBundle{}
		c.launches = map[string]sessions.DisplayLaunch{}
		c.status = DisplayStatus{}
		c.cold = false
	} else {
		c.members = maps.Clone(c.members)
		c.workflows = maps.Clone(c.workflows)
		c.launches = maps.Clone(c.launches)
	}
	for _, r := range delta.Records {
		switch r.Kind {
		case "member":
			if r.Value == nil {
				delete(c.members, r.ID)
				continue
			}
			var wire sessions.DisplayMember
			if err := json.Unmarshal(r.Value, &wire); err != nil {
				return err
			}
			var b displayMemberBundle
			var err error
			if b.member, err = decodeDisplay[Member](wire.Member); err != nil {
				return err
			}
			if b.execution, err = decodeDisplay[Execution](wire.Execution); err != nil {
				return err
			}
			if b.task, err = decodeDisplay[Task](wire.Task); err != nil {
				return err
			}
			if b.mail, err = decodeDisplay[Mail](wire.Message); err != nil {
				return err
			}
			if err = json.Unmarshal(wire.Presentation, &b.presentation); err != nil {
				return err
			}
			b.decisions, b.first = wire.Decisions, displayDecision(wire.First)
			c.members[r.ID] = b
		case "workflow":
			if r.Value == nil {
				delete(c.workflows, r.ID)
				continue
			}
			var wire sessions.DisplayWorkflow
			if err := json.Unmarshal(r.Value, &wire); err != nil {
				return err
			}
			report, err := decodeDisplay[workflow.Report](wire.Workflow)
			if err != nil {
				return err
			}
			c.workflows[r.ID] = displayWorkflowBundle{report, wire.Decisions}
		case "launch":
			if r.Value == nil {
				delete(c.launches, r.ID)
				continue
			}
			var edge sessions.DisplayLaunch
			if err := json.Unmarshal(r.Value, &edge); err != nil {
				return err
			}
			c.launches[r.ID] = edge
		case "status":
			var wire sessions.DisplayHeader
			if err := json.Unmarshal(r.Value, &wire); err != nil {
				return err
			}
			c.status = DisplayStatus{Counts: Counts(wire.Counts), HasRuns: wire.HasRuns, ParentDisposition: wire.ParentDisposition, First: displayDecision(wire.First)}
			pause, err := decodeDisplay[RetryPause](wire.RetryPause)
			if err != nil {
				return err
			}
			c.status.RetryPause = pause
			c.cold = wire.ColdCompatibility
		}
	}
	out := emptyDisplaySnapshot()
	out.Status = c.status
	for id, b := range c.members {
		if b.decisions > 0 {
			out.MemberDecisions[id] = b.decisions
		}
		if b.first != nil {
			out.FirstMemberDecision[id] = b.first
		}
		if b.member == nil {
			continue
		}
		out.Members[id] = b.member
		out.Presentations[id] = b.presentation
		if b.presentation.Busy {
			out.BusyMembers++
		}
		if b.execution != nil {
			out.Executions[b.member.Execution] = b.execution
		}
		if b.task != nil {
			out.Tasks[b.member.Task] = b.task
		}
		if b.mail != nil {
			out.Messages[b.mail.ID] = b.mail
		}
	}
	for id, b := range c.workflows {
		if b.report != nil {
			out.Workflows[id] = b.report
		}
		if b.decisions > 0 {
			out.WorkflowDecisions[id] = b.decisions
		}
	}
	for _, edge := range c.launches {
		out.Launches[edge.CallID] = append(out.Launches[edge.CallID], edge.Member)
	}
	for id, edges := range out.Launches {
		slices.Sort(edges)
		out.Launches[id] = edges
	}
	c.snapshot, c.mark = out, delta.Mark
	return nil
}
