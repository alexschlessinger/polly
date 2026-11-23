package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func registerPlanCommands(r *replCommandRegistry) {
	r.register(replCommand{name: "/plan", usage: "/plan [goal | review | accept | exit | edit | view | continue]", summary: "draft, review, and implement a plan",
		// Dispatch immediately to refuse unsafe transitions, never queue an approval.
		busySafe: true, run: replPlanCommand, complete: func(_ *replCommandContext, fields []string, prefix string) []string {
			if completionArgPos(fields, prefix) != 1 {
				return nil
			}
			return matchingWords([]string{"review", "accept", "exit", "edit", "view", "continue"}, prefix)
		}})
	r.register(replCommand{name: "/todo", usage: "/todo", summary: "inspect this session’s checklist (read-only)", busySafe: true, run: replTodoCommand})
}

// A review belongs to the frontend which actually displayed it, not the session
// database: reopening a session never restores permission to implement it.
func (c *replCommandContext) planSession() (sessions.PlanSession, error) {
	if c.state != nil {
		if s, ok := c.state.session.(sessions.PlanSession); ok {
			return s, nil
		}
	}
	return nil, errors.New("planning needs a session with plan storage")
}

func planRequiresWritableMode(name string, args []string) bool {
	switch name {
	case "/spawn", "/workflow", "/swarm", "/sandbox-init", "/screenshot":
		return true
	case "/sandbox":
		return !sandboxCommandBusySafe(args)
	case "/tools":
		return !toolsCommandBusySafe(args)
	case "/add-dir":
		return len(args) > 1
	default:
		return false
	}
}

func writablePlanAction(ctx context.Context, s *conversationState) error {
	p, err := currentPlan(ctx, s)
	if err != nil {
		return err
	}
	if p != nil && p.Mode == sessions.PlanDraft {
		return errors.New("write-capable actions are disabled while planning; use /plan exit first")
	}
	if s != nil && s.resumeExecution != nil {
		return errors.New("execution tools are unavailable; reopen the session or use /plan continue for an accepted plan")
	}
	return nil
}

func (r *managedREPL) planBusy() bool {
	if r.model.busy || r.opening != "" || r.quitting || len(r.spawnRequests) != 0 || len(r.model.queue) > 0 {
		return true
	}
	for _, tab := range r.tabs {
		if tab.turnDone != nil || tab.state != nil && (tab.state.planWork.Load() > 0 || tab.state.swarm != nil && tab.state.swarm.HasActive()) {
			return true
		}
	}
	return false
}

func (c *replCommandContext) refreshChecklist(ctx context.Context) error {
	if c.planChanged == nil {
		return nil
	}
	p, todos, err := readChecklist(ctx, c.state)
	if err != nil {
		return err
	}
	c.planChanged(p, todos)
	return nil
}

func planDraftNotice(path string) string {
	return "PLAN · Draft: " + planDisplayText(path) + " · /plan review"
}

// Display control characters explicitly rather than letting user-edited Markdown
// move the terminal cursor or hide text at the approval boundary.
func planDisplayText(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			return '\uFFFD'
		}
		return r
	}, s)
}
func planTextLines(s string) []string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = "  " + planDisplayText(strings.TrimSuffix(line, "\r"))
	}
	return lines
}
func replTodoCommand(c *replCommandContext, args []string) replCommandResult {
	if len(args) != 1 {
		return replCommandResult{err: c.replyLine("usage: /todo (read-only)")}
	}
	p, todos, err := readChecklist(c.operationContext(), c.state)
	if err != nil {
		return c.planError(err)
	}
	if c.planChanged != nil {
		c.planChanged(p, todos)
	}
	if c.openTodoPanel != nil {
		c.openTodoPanel()
		return replCommandResult{}
	}
	for _, line := range projectChecklist(p, todos).checklistLines() {
		if err := c.replyLine(line); err != nil {
			return replCommandResult{err: err}
		}
	}
	return replCommandResult{}
}
func (c *replCommandContext) planError(err error) replCommandResult {
	if err == nil || terminalSessionError(err) || context.Cause(c.operationContext()) != nil {
		return replCommandResult{err: err}
	}
	return replCommandResult{err: c.replyLine("Plan: " + err.Error())}
}
func replPlanCommand(c *replCommandContext, args []string) replCommandResult {
	return c.planError(c.planCommand(args))
}

func (c *replCommandContext) planCommand(args []string) error {
	ctx := c.operationContext()
	ps, err := c.planSession()
	if err != nil {
		return err
	}
	p, err := ps.GetPlan(ctx)
	if err != nil {
		return err
	}
	action := ""
	if len(args) > 1 {
		action = strings.ToLower(args[1])
	}
	switch action {
	case "review", "accept", "exit", "edit", "view", "continue":
		if len(args) != 2 {
			return fmt.Errorf("usage: /plan %s", action)
		}
	default:
		action = "draft"
	}
	if action == "view" {
		if p == nil || p.Mode != sessions.PlanAccepted {
			return errors.New("no accepted plan to view; use /plan review for a draft")
		}
		return c.replyLines(append([]string{"Accepted plan (read-only) · " + p.Revision}, planTextLines(p.Snapshot)...))
	}
	if p != nil && p.Mode == sessions.PlanAccepted && action != "continue" {
		return c.replyLine("This session already has an accepted, read-only plan. View it with /plan view, see progress with /todo, or explicitly continue with /plan continue. Use /new for another plan.")
	}
	if action == "edit" {
		if p == nil || p.Mode != sessions.PlanDraft {
			return errors.New("open a draft with /plan first")
		}
		// Validate the managed path again; never substitute an arbitrary user path.
		if _, _, err := readPlanDraft(p.Path); err != nil {
			return fmt.Errorf("cannot open draft %s: %w", p.Path, err)
		}
		if c.openPlanEditor == nil {
			return c.replyLine("Open this draft in your editor: " + p.Path)
		}
		if err := c.openPlanEditor(p.Path); err != nil {
			return fmt.Errorf("could not open an editor: %w. Open %s manually", err, p.Path)
		}
		return c.replyLine("Requested an editor for " + p.Path + ". If none opened, open that path manually; edits are used until acceptance.")
	}
	if p != nil && p.Mode == sessions.PlanDraft && action == "draft" && len(args) == 1 {
		return c.replyLine(planDraftNotice(p.Path))
	}
	if p != nil && (action == "accept" || action == "continue" || action == "draft") {
		if err := verifyPlanWorkspace(p.Base); err != nil {
			return err
		}
	}
	if c.planBusy != nil && c.planBusy() || c.state.planWork.Load() > 0 || c.state.workspaceChangesPending() || c.state.swarm != nil && c.state.swarm.HasActive() {
		return errors.New("work is still running; wait for the turn and workers to finish, then retry (nothing queued)")
	}
	switch action {
	case "draft":
		return c.enterPlan(ctx, ps, p, commandArgument(c.line, 1))
	case "continue":
		if p == nil || p.Mode != sessions.PlanAccepted {
			return errors.New("no accepted plan to continue")
		}
		if len(p.Todos) > 0 && allPlanOutcomesDone(p.Todos) {
			return c.replyLine("All approved outcomes are done; no turn started.")
		}
		if err := c.state.restoreExecutionTools(ctx); err != nil {
			return err
		}
		return c.startPlanTurn("/plan continue", "Continue the accepted plan from recorded progress. Inspect existing work and blocked reasons before acting; do not replay completed actions.")
	}
	if p == nil || p.Mode != sessions.PlanDraft {
		return errors.New("no active draft; use /plan first")
	}
	if action == "exit" {
		if err := ps.ExitPlan(ctx); err != nil {
			return err
		}
		c.state.planReview = ""
		if err := c.refreshChecklist(ctx); err != nil {
			return err
		}
		if err := c.replyLine("Left plan mode without starting a turn. Return to the draft with /plan."); err != nil {
			return err
		}
		if err := c.state.restoreExecutionTools(ctx); err != nil {
			return fmt.Errorf("%w; reopen the session after fixing the tool configuration", err)
		}
		return nil
	}
	return c.reviewOrAcceptPlan(ctx, ps, p, action == "accept")
}

// A session can be reopened from another directory. Viewing it there is fine;
// approval for one workspace is not approval to implement in a different one.
func planWorkspace() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(wd)
}

func verifyPlanWorkspace(base string) error {
	wd, err := planWorkspace()
	if err != nil {
		return err
	}
	if wd != base {
		return fmt.Errorf("reopen this session from its plan workspace %q to work on the plan", base)
	}
	return nil
}

func (c *replCommandContext) enterPlan(ctx context.Context, ps sessions.PlanSession, p *sessions.Plan, goal string) error {
	if goal != "" && c.startTurn == nil {
		return errors.New("this frontend cannot start a planning turn")
	}
	if p == nil || p.Mode == sessions.PlanExited {
		md, err := c.state.session.GetMetadata(ctx)
		if err != nil {
			return err
		}
		if md != nil && md.Parent != "" {
			return errors.New("enter planning in the parent session, not a delegated agent")
		}
		if p == nil {
			base, err := planWorkspace()
			if err != nil {
				return err
			}
			if promote := c.state.storage.promote; promote != nil {
				if err := promote(ctx); err != nil {
					return fmt.Errorf("persist plan session: %w", err)
				}
			}
			path, err := newPlanDraftPath()
			if err != nil {
				return err
			}
			p = &sessions.Plan{Path: path, Base: base}
		}
		if err := ps.StartPlan(ctx, p.Path, p.Base); err != nil {
			return fmt.Errorf("record draft %s: %w", p.Path, err)
		}
		if err := c.refreshChecklist(ctx); err != nil {
			return err
		}
	}
	c.state.planReview = ""
	if err := c.replyLine(planDraftNotice(p.Path)); err != nil {
		return err
	}
	if goal == "" {
		return nil
	}
	return c.startPlanTurn(c.line, goal)
}

func allPlanOutcomesDone(items []sessions.PlanTodo) bool {
	for _, item := range items {
		if item.State != sessions.TodoDone {
			return false
		}
	}
	return true
}

// Keep the literal document intact and visually distinct from the controls.
// No table, fixed-width rule or horizontal button bar is needed on small TTYs.
func (c *replCommandContext) displayPlanReview(text, revision string, steps []string) error {
	line := func(text, role, modifier string) error {
		if c.replyMarkup != nil {
			c.replyMarkup(style.Styled(text, role, modifier))
			return nil
		}
		return c.replyLine(text)
	}
	if err := line("Plan review · "+revision[:12], "accent", "bold"); err != nil {
		return err
	}
	for _, raw := range planTextLines(text) {
		if err := line(raw, "", ""); err != nil {
			return err
		}
	}
	noun := "outcomes"
	if len(steps) == 1 {
		noun = "outcome"
	}
	if err := line(fmt.Sprintf("For approval · %d %s", len(steps), noun), "accent", "bold"); err != nil {
		return err
	}
	for i, step := range steps {
		if err := line(fmt.Sprintf("  %d. %s", i+1, planDisplayText(step)), "", ""); err != nil {
			return err
		}
	}
	if err := line("/plan accept · Start implementation", "accent", "bold"); err != nil {
		return err
	}
	if err := line("/plan exit · Leave without starting", "", ""); err != nil {
		return err
	}
	return line("Or send a message to revise the draft.", "muted", "")
}

func (c *replCommandContext) reviewOrAcceptPlan(ctx context.Context, ps sessions.PlanSession, p *sessions.Plan, accept bool) error {
	text, revision, err := readPlanDraft(p.Path)
	if err != nil {
		c.state.planReview = ""
		return fmt.Errorf("cannot read draft %s: %w (restore the file to review it)", p.Path, err)
	}
	if !accept || c.state.planReview != revision {
		changed := accept && c.state.planReview != ""
		c.state.planReview = ""
		if changed {
			if err := c.replyLine("Draft changed since review. Nothing started; review this revision and accept again."); err != nil {
				return err
			}
		}
		steps, err := sessions.ParsePlanSteps(text)
		if err != nil {
			return fmt.Errorf("draft not ready for review: %w. Edit %s or ask Polly to revise it", err, p.Path)
		}
		if err := c.displayPlanReview(text, revision, steps); err != nil {
			return err
		}
		c.state.planReview = revision
		return nil
	}
	if c.startTurn == nil {
		return errors.New("this frontend cannot start implementation")
	}
	started, err := ps.AcceptPlan(ctx, c.state.planReview, text)
	if err != nil {
		return err
	}
	c.state.planReview = ""
	if err := c.refreshChecklist(ctx); err != nil {
		return err
	}
	if !started {
		return c.replyLine("Already accepted; no new turn started.")
	}
	if err := c.replyLine("Accepted the reviewed plan. Its outcomes are fixed; progress is in /todo."); err != nil {
		return err
	}
	if err := c.state.restoreExecutionTools(ctx); err != nil {
		return fmt.Errorf("%w; approval was saved, use /plan continue to retry", err)
	}
	return c.startPlanTurn("/plan accept", "Implement the accepted plan. Work through the approved outcomes in order and record truthful progress.")
}
func (c *replCommandContext) startPlanTurn(display, text string) error {
	if c.startTurn == nil {
		return errors.New("this frontend cannot start a turn")
	}
	msg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: text}
	writeComposerMetadata(&msg, composerMetadata{Version: 1, Draft: display})
	return c.startTurn(display, msg)
}
