package main

import (
	"context"

	"github.com/alexschlessinger/pollytool/sessions"
)

// The first prompt on resume projects stored todos even without a plan. Later
// prompts need only the mode; live progress uses the stderr activity channel.
func linePlanPrompt(ctx context.Context, c *replCommandContext, first bool) (string, error) {
	if c == nil || c.state == nil {
		return "> ", nil
	}
	var p *sessions.Plan
	var err error
	if first {
		var todos []sessions.PlanTodo
		p, todos, err = readChecklist(ctx, c.state)
		if err == nil && (len(todos) > 0 || p != nil && p.Mode == sessions.PlanAccepted) {
			err = c.replyLine(lineTodoStatus(p, todos) + " · /todo for checklist")
		}
	} else {
		p, err = currentPlan(ctx, c.state)
	}
	if err != nil {
		return "", err
	}
	if p != nil && p.Mode == sessions.PlanDraft {
		if first {
			if err := c.replyLine(planDraftNotice(p.Path)); err != nil {
				return "", err
			}
		}
		return "PLAN> ", nil
	}
	return "> ", nil
}

func lineTodoStatus(plan *sessions.Plan, todos []sessions.PlanTodo) string {
	if len(todos) == 0 {
		if plan != nil && plan.Mode == sessions.PlanAccepted {
			return "PLAN · Accepted · checklist not started"
		}
		return "No todos yet."
	}
	p := projectChecklist(plan, todos)
	return "TODO " + p.count + " · " + cleanActivityText(p.current)
}

// Keep progress off stdout and out of interactive approval prompts.
func (ui *lineTurnUI) TodoProgress(p *sessions.Plan, todos []sessions.PlanTodo) {
	if !ui.config.Quiet && (len(todos) > 0 || p != nil && p.Mode == sessions.PlanAccepted) {
		ui.AppendNotice(lineTodoStatus(p, todos))
	}
}
