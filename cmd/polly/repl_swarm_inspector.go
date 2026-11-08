package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

// Navigation metadata survives view eviction; records and rendered pages do not.
// Each section has its own viewState, so switching sections keeps its position.
type swarmInspectorPageState struct {
	after, next, revision, renderedAfter string
	previous                             []string
	positions                            map[string]swarmInspectorPosition
	status                               swarm.DisplayStatus
	parent                               string
}

// Parent turn/wait/work outcomes are in-memory presentation, not coordination
// commits. Only the first members page renders them. Comparing this small key
// keeps an otherwise idle inspector current without waking durable subscribers.
func (r *managedREPL) swarmInspectorParentChanged(target viewTarget) bool {
	if target.kind != swarmViewKind || target.item != "members" || r.state == nil || r.state.swarm == nil {
		return false
	}
	page := r.workspace().viewState(target).swarm
	return page != nil && page.after == "" && page.parent != r.state.swarm.ParentDisplayState(page.status).Display
}

func readSwarmInspector(ctx context.Context, runtime *swarm.Runtime, target viewTarget, previous viewSource, page *swarmInspectorPageState) (viewSource, *swarmInspectorPageState, error) {
	next := swarmInspectorPageState{}
	if page != nil {
		next = *page
		next.previous = append([]string(nil), page.previous...)
	}
	known := next.revision
	if previous.model == nil {
		known = ""
	}
	selected, err := runtime.ReadInspectorPage(ctx, swarm.InspectorQuery{Section: target.item, After: next.after, Revision: known})
	if err != nil {
		return viewSource{}, page, err
	}
	status := next.status
	if !selected.Unchanged {
		status = selected.Status
	}
	parent := runtime.ParentDisplayState(status)
	if selected.Unchanged && next.parent == parent.Display {
		return previous, &next, nil
	}
	// An in-memory parent turn can change without a durable record change.
	// Only the first members page prints it; re-read that bounded page, never raw.
	if selected.Unchanged && target.item == "members" && next.after == "" {
		selected, err = runtime.ReadInspectorPage(ctx, swarm.InspectorQuery{Section: target.item, After: next.after})
		if err != nil {
			return viewSource{}, page, err
		}
	} else if selected.Unchanged {
		next.parent = parent.Display
		return previous, &next, nil
	}
	body := swarmInspectorPageText(selected, &parent)
	if next.after != "" || selected.Next != "" {
		navigation := fmt.Sprintf("Page %d", len(next.previous)+1)
		if next.after != "" {
			navigation += " · Ctrl-Left: previous page"
		}
		if selected.Next != "" {
			navigation += " · Ctrl-Right: next page"
		}
		body = navigation + "\n\n" + body + "\n" + navigation + "\n"
	}
	model := newReplModel()
	model.appendLine(style.Escape(body))
	revision := selected.Revision
	if revision == "" { // Compatibility has no durable equality tokens.
		revision = fmt.Sprintf("swarm-compat:%d", time.Now().UnixNano())
	}
	revision = "swarm:" + revision + "/" + parent.Display
	next.next, next.revision, next.status, next.parent = selected.Next, selected.Revision, selected.Status, parent.Display
	next.renderedAfter = next.after
	return viewSource{model: model, revision: revision, info: &sessions.SessionView{ID: target.session.ID, Metadata: &sessions.Metadata{Name: target.session.Name}, Revision: revision}}, &next, nil
}

// Rendering consumes only owned selected records and explicit display facts.
// It must never reconstruct a partial swarm.State for semantic helpers.
func swarmInspectorPageText(page swarm.InspectorPage, parent *swarm.AgentPresentation) string {
	jsonText := func(value any) string { data, _ := json.MarshalIndent(value, "", "  "); return string(data) }
	if page.Section == "raw" {
		return jsonText(page.Raw)
	}
	var b strings.Builder
	for _, item := range page.Items {
		switch item.Kind {
		case "parent":
			if parent != nil {
				fmt.Fprintf(&b, "Parent — %s\n\n", parent.Display)
			}
		case "decisions_heading":
			fmt.Fprintf(&b, "Needs decision (%d)\n", item.Total)
		case "working_heading":
			fmt.Fprintf(&b, "Working (%d)\n", item.Total)
		case "decision":
			d := item.Decision
			if d.Why != "" {
				fmt.Fprintf(&b, "%s: %s; %s\n", d.Label, d.Why, d.Action)
			} else {
				fmt.Fprintf(&b, "%s: %s\n", d.Label, d.Action)
			}
		case "working":
			w := item.Working
			fmt.Fprintf(&b, "%s · %s", w.Label, w.State)
			if w.Task != "" {
				fmt.Fprintf(&b, " · task %s", w.Task)
			}
			if w.Agents > 0 {
				fmt.Fprintf(&b, " · %d agents", w.Agents)
			}
			b.WriteByte('\n')
		case "space":
			b.WriteByte('\n')
		case "counts":
			fmt.Fprintf(&b, "%d members · %d tasks · %d publications · %d workflows\n\n", item.Counts[0], item.Counts[1], item.Counts[2], item.Counts[3])
		case "run":
			run := item.Run
			status := run.Status
			if status == "running" {
				status = "open"
			}
			fmt.Fprintf(&b, "Run %s\n%s · active executions: %d · pending tasks: %d · %d / %d logical executions\n\n", run.ID, status, item.Active, item.Pending, run.Starts, run.Limit)
		case "member":
			m := item.Member
			label := m.Label
			if label == "" {
				label = m.Name
			}
			fmt.Fprintf(&b, "%s — %s\n%s\nTask: %s\n", label, item.Display, m.ID, m.Task)
			if e := item.Execution; e != nil {
				fmt.Fprintf(&b, "Execution: %s\nModel calls: %d / %d\n", e.Status, e.Iterations, e.Request.MaxIterations)
				if e.Error != "" {
					fmt.Fprintf(&b, "Reason: %s\n", e.Error)
				}
			}
			if c := item.Context; c != nil {
				fmt.Fprintf(&b, "Workspace: %s", c.Root)
				if c.Scratch != "" {
					fmt.Fprintf(&b, " · scratch %s", c.Scratch)
				}
				if c.Release == swarm.WorkspaceRetained {
					fmt.Fprintf(&b, " · retained · %s", c.Reason)
				}
				b.WriteByte('\n')
			} else {
				b.WriteString("Workspace: released\n")
			}
			if m.Controller != "" {
				fmt.Fprintf(&b, "Workflow reservation: %s\n", m.Controller)
			}
			b.WriteByte('\n')
		case "no_members":
			b.WriteString("No members yet. Agents join when you delegate work.\n")
		case "footer":
			b.WriteString("Use Agents to inspect private conversations. /swarm stop ID pauses a member; /swarm resume ID continues it. If its iteration allowance is exhausted, /swarm resume ID N grants N additional model calls.\n")
		case "task":
			task := item.Task
			fmt.Fprintf(&b, "%s · revision %d\n%s\n%s\nOwner: %s\nAcceptance criteria: %s\n", item.Display, task.Revision, item.ID, task.Description, task.Owner, task.Criteria)
			fmt.Fprintf(&b, "Requirement: %s\n", task.Requirement)
			if task.Delivery != nil {
				fmt.Fprintf(&b, "Delivered via %s at %s\n", task.Delivery.Via, task.Delivery.At.Format(time.RFC3339))
			}
			if item.Deferred {
				fmt.Fprintf(&b, "Deferred: %s\n", task.Deferral.Note)
			}
			if task.AcceptedRevision > 0 {
				fmt.Fprintf(&b, "Accepted revision: %d\n", task.AcceptedRevision)
			}
			if len(task.Dependencies) > 0 {
				fmt.Fprintf(&b, "Dependencies: %s\n", strings.Join(task.Dependencies, ", "))
			}
			if task.Feedback != "" {
				fmt.Fprintf(&b, "Feedback: %s\n", task.Feedback)
			}
			if task.BaseCommit != "" {
				fmt.Fprintf(&b, "Base commit: %s\n", task.BaseCommit)
			}
			if task.ResultCommit != "" {
				fmt.Fprintf(&b, "Result commit: %s\n", task.ResultCommit)
			}
			if task.Result != nil {
				fmt.Fprintf(&b, "Result: %s\n", jsonText(task.Result))
			}
			b.WriteByte('\n')
		case "mail":
			mail := item.Mail
			status := "pending delivery"
			if mail.Delivered {
				status = "delivered"
			}
			fmt.Fprintf(&b, "%s · %s\n%s → %s\n%s\n%s\n", mail.Kind, status, mail.From, mail.To, item.ID, clipSwarmMessage(mail.Text))
			if mail.ReplyTo != "" {
				fmt.Fprintf(&b, "Reply to: %s\n", mail.ReplyTo)
			}
			b.WriteByte('\n')
		case "publication":
			pub := item.Publication
			fmt.Fprintf(&b, "%s\nPublished by %s\n%s\n", item.ID, pub.Author, pub.Text)
			if pub.Supersedes != "" {
				fmt.Fprintf(&b, "Corrects: %s\n", pub.Supersedes)
			}
			if len(pub.Sources) > 0 {
				fmt.Fprintf(&b, "Sources: %s\n", strings.Join(pub.Sources, ", "))
			}
			if item.Commit != "" {
				fmt.Fprintf(&b, "Commit: %s\n", item.Commit)
			}
			for _, artifact := range pub.Artifacts {
				fmt.Fprintf(&b, "Artifact: %s\n", artifact.ID)
			}
			b.WriteByte('\n')
		case "workflow":
			w := item.Workflow
			fmt.Fprintf(&b, "%s · %s\n%s\n%d saved steps\n", w.Name, w.Status, item.ID, len(w.Steps))
			if w.Error != nil {
				fmt.Fprintf(&b, "Error (%s): %s\n", w.Error.Code, w.Error.Message)
			}
			if w.Acknowledged {
				b.WriteString("Failure acknowledged by parent\n")
				if item.DeferredItems > 0 {
					fmt.Fprintf(&b, "%d deferred items retained for later review\n", item.DeferredItems)
				}
			}
			if w.Output != nil {
				fmt.Fprintf(&b, "Result:\n%s\n", jsonText(w.Output))
			}
			for index, step := range w.Steps {
				fmt.Fprintf(&b, "  %d. %s · %s\n", index+1, step.Kind, step.Status)
				if step.Error != nil {
					fmt.Fprintf(&b, "     %s\n", step.Error.Message)
				}
			}
			b.WriteString("Source, inputs and full step values are available in raw.\n\n")
		case "integration":
			c := item.Integration
			fmt.Fprintf(&b, "%s · %s\nDrift: %s · Accepted: %t\nCandidate commit: %s\n", c.ID, c.Status, c.Drift, c.Accepted, c.Merged.Commit)
			if c.Predecessor != "" {
				fmt.Fprintf(&b, "Predecessor: %s\n", c.Predecessor)
			}
			if c.Successor != "" {
				fmt.Fprintf(&b, "Superseded by: %s\n", c.Successor)
			}
			fmt.Fprintf(&b, "Inputs: %s\nRepairs: %s\nPending: %d\n", jsonText(c.Inputs), jsonText(c.Repairs), len(c.Pending))
			for _, conflict := range c.Conflicts {
				fmt.Fprintf(&b, "%s · %s\n%s\n", conflict.Type, strings.Join(conflict.Paths, ", "), jsonText(conflict))
			}
			if receipt := item.Apply; receipt != nil {
				fmt.Fprintf(&b, "Apply: %s\nParent at application: %s\n%s\n", receipt.Status, receipt.ObservedParent.Commit, receipt.Error)
			}
			b.WriteByte('\n')
		case "preview":
			preview := item.Preview
			status := "ready for accepted task integration"
			if preview.Conflicts != "" {
				status = "conflicts require review"
			}
			fmt.Fprintf(&b, "%s\n%s\nCandidate: %s\nDirectory: %s\n", status, item.ID, preview.Candidate.ID, preview.Checkout.Path)
			if preview.Conflicts != "" {
				fmt.Fprintf(&b, "%s\n", preview.Conflicts)
			}
			b.WriteByte('\n')
		}
	}
	if b.Len() == 0 && page.Next == "" {
		if page.Total > 0 {
			return "No more " + page.Section + " on this page."
		}
		return "No " + page.Section + " yet."
	}
	return b.String()
}
