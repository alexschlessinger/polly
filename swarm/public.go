package swarm

import (
	"context"
	"errors"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

// CommitView identifies captured code without exposing its internal record key.
// These views never change serialization of persisted or exported runtime types.
type CommitView struct {
	Commit string `json:"commit,omitempty"`
	Tree   string `json:"tree,omitempty"`
	Source string `json:"source,omitempty"`
}

func presentCommit(state *State, capture worktree.Snapshot) CommitView {
	if state == nil || !validTaskSnapshot(state.Snapshots[capture.ID], capture.ID) || !sameCapturedCommit(state.Snapshots[capture.ID], &capture) {
		return CommitView{}
	}
	return CommitView{Commit: capture.Commit, Tree: capture.Tree, Source: capture.Source}
}

type TaskView struct {
	FollowupCallID   string        `json:"followupCallID,omitempty"`
	Requirement      string        `json:"requirement,omitempty"`
	Delivery         *TaskDelivery `json:"delivery,omitempty"`
	Follows          string        `json:"follows,omitempty"`
	SourceRoot       string        `json:"sourceRoot,omitempty"`
	Deferral         *TaskDeferral `json:"deferral,omitempty"`
	BaseCommit       string        `json:"baseCommit,omitempty"`
	Execution        string        `json:"execution,omitempty"`
	ID               string        `json:"id"`
	Run              string        `json:"run"`
	Description      string        `json:"description"`
	Criteria         string        `json:"criteria"`
	Dependencies     []string      `json:"dependencies"`
	Owner            string        `json:"owner,omitempty"`
	Status           string        `json:"status"`
	Revision         int           `json:"revision"`
	AcceptedRevision int           `json:"acceptedRevision,omitempty"`
	Result           any           `json:"result,omitempty"`
	Feedback         string        `json:"feedback,omitempty"`
	ResultCommit     string        `json:"resultCommit,omitempty"`
}

// PresentTask resolves retained captures; user-authored Result remains opaque.
func PresentTask(s *State, t *Task) *TaskView {
	if t == nil {
		return nil
	}
	return &TaskView{
		FollowupCallID: t.FollowupCallID, Requirement: t.Requirement, Delivery: t.Delivery,
		Follows: t.Follows, SourceRoot: t.SourceRoot, Deferral: t.Deferral,
		BaseCommit: snapshotCommit(s, t.StartingSnapshot), Execution: t.Execution,
		ID: t.ID, Run: t.Run, Description: t.Description, Criteria: t.Criteria,
		Dependencies: t.Dependencies, Owner: t.Owner, Status: t.Status,
		Revision: t.Revision, AcceptedRevision: t.AcceptedRevision, Result: t.Result,
		Feedback: t.Feedback, ResultCommit: snapshotCommit(s, t.Snapshot),
	}
}

type PublicationView struct {
	ID         string          `json:"id"`
	Author     string          `json:"author"`
	Run        string          `json:"run"`
	Text       string          `json:"text"`
	Sources    []string        `json:"sources,omitempty"`
	Supersedes string          `json:"supersedes,omitempty"`
	Commit     string          `json:"commit,omitempty"`
	Artifacts  []artifacts.Ref `json:"artifacts,omitempty"`
	Posted     time.Time       `json:"posted"`
}

// PresentPublication keeps attribution while resolving its captured code.
func PresentPublication(s *State, p *Publication) *PublicationView {
	if p == nil {
		return nil
	}
	return &PublicationView{ID: p.ID, Author: p.Author, Run: p.Run, Text: p.Text,
		Sources: p.Sources, Supersedes: p.Supersedes, Commit: snapshotCommit(s, p.Snapshot),
		Artifacts: p.Artifacts, Posted: p.Posted}
}

type IntegrationInputView struct {
	TaskReference
	Base      CommitView `json:"base"`
	Submitted CommitView `json:"submitted"`
}

func presentInputs(s *State, inputs []IntegrationInput) []IntegrationInputView {
	if inputs == nil {
		return nil
	}
	result := make([]IntegrationInputView, len(inputs))
	for i, input := range inputs {
		result[i] = IntegrationInputView{TaskReference: input.TaskReference, Base: presentCommit(s, input.Base), Submitted: presentCommit(s, input.Submitted)}
	}
	return result
}

type ConflictView struct {
	Type    string                   `json:"type"`
	Message string                   `json:"message"`
	Paths   []string                 `json:"paths"`
	Entries []worktree.ConflictEntry `json:"entries,omitempty"`
	Base    CommitView               `json:"base"`
	Ours    CommitView               `json:"ours"`
	Theirs  CommitView               `json:"theirs"`
}

type ApplyPlanView struct {
	ID        string                `json:"id"`
	Parent    CommitView            `json:"parent"`
	Merged    CommitView            `json:"merged"`
	Drift     string                `json:"drift"`
	PatchHash string                `json:"patchHash"`
	Paths     []worktree.PathChange `json:"paths"`
}

func presentPlan(s *State, p worktree.ApplyPlan) ApplyPlanView {
	return ApplyPlanView{ID: p.ID, Parent: presentCommit(s, p.Parent), Merged: presentCommit(s, p.Merged), Drift: p.Drift, PatchHash: p.PatchHash, Paths: p.Paths}
}

type ApplyView struct {
	ID             string          `json:"id"`
	Tasks          []TaskReference `json:"tasks"`
	Plan           ApplyPlanView   `json:"plan"`
	ObservedParent CommitView      `json:"observedParent"`
	Status         string          `json:"status"`
	Error          string          `json:"error,omitempty"`
	Started        time.Time       `json:"started"`
	Finished       time.Time       `json:"finished,omitempty"`
}

// PresentApply preserves the recorded outcome without exposing snapshot keys.
func PresentApply(s *State, a *ApplyRecord) *ApplyView {
	if a == nil {
		return nil
	}
	return &ApplyView{ID: a.ID, Tasks: a.Tasks, Plan: presentPlan(s, a.Plan), ObservedParent: presentCommit(s, a.ObservedParent), Status: a.Status, Error: a.Error, Started: a.Started, Finished: a.Finished}
}

type IntegrationView struct {
	ID          string                 `json:"id"`
	Run         string                 `json:"run"`
	Status      string                 `json:"status"`
	Inputs      []IntegrationInputView `json:"inputs"`
	Repairs     []IntegrationInputView `json:"repairs"`
	Pending     []IntegrationInputView `json:"pending"`
	Parent      CommitView             `json:"parent"`
	Merged      CommitView             `json:"merged"`
	Conflicts   []ConflictView         `json:"conflicts"`
	Drift       string                 `json:"drift"`
	Plan        ApplyPlanView          `json:"plan"`
	Accepted    bool                   `json:"accepted"`
	Predecessor string                 `json:"predecessor,omitempty"`
	Successor   string                 `json:"successor,omitempty"`
	Created     time.Time              `json:"created"`
	Receipt     *ApplyView             `json:"receipt"`
}

// PresentIntegration distinguishes integration identity from captured commits.
func PresentIntegration(s *State, c *IntegrationCandidate) *IntegrationView {
	if c == nil {
		return nil
	}
	// ReadIntegration hydrates Receipt, but unchanged refreshes and structured
	// errors may carry a raw candidate. Resolve their authoritative attempt here
	// without modifying the exported candidate or its stored representation.
	receipt := c.Receipt
	if s != nil && s.Applies[c.ID] != nil {
		receipt = s.Applies[c.ID]
	}
	var conflicts []ConflictView
	if c.Conflicts != nil {
		conflicts = make([]ConflictView, len(c.Conflicts))
		for i, f := range c.Conflicts {
			conflicts[i] = ConflictView{Type: f.Type, Message: f.Message, Paths: f.Paths, Entries: f.Entries, Base: presentCommit(s, f.Base), Ours: presentCommit(s, f.Ours), Theirs: presentCommit(s, f.Theirs)}
		}
	}
	return &IntegrationView{ID: c.ID, Run: c.Run, Status: c.Status,
		Inputs: presentInputs(s, c.Inputs), Repairs: presentInputs(s, c.Repairs), Pending: presentInputs(s, c.Pending),
		Parent: presentCommit(s, c.Parent), Merged: presentCommit(s, c.Merged), Conflicts: conflicts,
		Drift: c.Drift, Plan: presentPlan(s, c.Plan), Accepted: c.Accepted,
		Predecessor: c.Predecessor, Successor: c.Successor, Created: c.Created, Receipt: PresentApply(s, receipt)}
}

type integrationRefreshView struct {
	*IntegrationView
	Changed bool `json:"changed"`
}

type integrationOutcomeView struct {
	Status    string          `json:"status"`
	Candidate string          `json:"candidate,omitempty"`
	Tasks     []TaskReference `json:"tasks"`
	Unchanged []string        `json:"unchanged,omitempty"`
	Receipt   *ApplyView      `json:"receipt,omitempty"`
	Next      string          `json:"next"`
}

// Only runtime-owned types are projected. In particular, maps, agent values,
// stored workflow reports, and historical messages are never rewritten.
func publicValue(s *State, value any) any {
	switch v := value.(type) {
	case *Task:
		return PresentTask(s, v)
	case *Publication:
		return PresentPublication(s, v)
	case worktree.Snapshot:
		return presentCommit(s, v)
	case *IntegrationCandidate:
		return PresentIntegration(s, v)
	case *IntegrationRefresh:
		if v == nil {
			return nil
		}
		return &integrationRefreshView{IntegrationView: PresentIntegration(s, v.IntegrationCandidate), Changed: v.Changed}
	case *ApplyRecord:
		return PresentApply(s, v)
	case *IntegrationOutcome:
		if v == nil {
			return nil
		}
		return &integrationOutcomeView{Status: v.Status, Candidate: v.Candidate, Tasks: v.Tasks, Unchanged: v.Unchanged, Receipt: PresentApply(s, v.Receipt), Next: v.Next}
	default:
		return value
	}
}

func publicNeedsState(value any) bool {
	switch value.(type) {
	case *Task, *Publication, worktree.Snapshot, *IntegrationCandidate, *IntegrationRefresh, *ApplyRecord, *IntegrationOutcome:
		return true
	}
	return false
}

func (r *Runtime) publicResult(ctx context.Context, value any, err error) (any, error) {
	var failure *workflow.Error
	errors.As(err, &failure)
	var s *State
	if publicNeedsState(value) || failure != nil && publicNeedsState(failure.Result) {
		var readErr error
		// Projection must not discard a completed effect's receipt on cancellation.
		s, readErr = r.read(context.WithoutCancel(ctx))
		if readErr != nil {
			return nil, errors.Join(err, readErr)
		}
	}
	if failure != nil {
		copy := *failure
		copy.Result = publicValue(s, failure.Result)
		copy.Message, copy.Cause = err.Error(), err
		err = &copy
	}
	return publicValue(s, value), err
}
