package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"runtime"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
)

// sandboxInitCommands bounds the failed commands /init names to the model.
const sandboxInitCommands = 5

// replInitCommand implements /init: it starts a run of the sandbox setup
// and a turn that hands it to the model with the sandbox-setup skill.
func replInitCommand(ctx *replCommandContext, _ []string) replCommandResult {
	lines, err := sandboxInitCommand(ctx)
	if replyErr := ctx.replyLines(lines); replyErr != nil {
		return replCommandResult{err: replyErr}
	}
	return replCommandResult{err: err}
}

// sandboxInitCommand runs /init and returns the lines to print, and an
// error only when the session cannot go on: the line frontend runs the
// turn inside the command.
func sandboxInitCommand(ctx *replCommandContext) ([]string, error) {
	if _, why := sandboxProfileFor(ctx); why != "" {
		return []string{why}, nil
	}
	if ctx.startTurn == nil {
		return []string{"init: it needs a REPL that can start a turn"}, nil
	}
	if err := sandboxInitReady(ctx); err != nil {
		return []string{"init: " + err.Error()}, nil
	}
	draft := strings.TrimSpace("/init " + commandArgument(ctx.line, 1))
	msg := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{
			{Type: "text", Text: draft},
			{Type: "text", Text: sandboxInitBrief(ctx, commandArgument(ctx.line, 1))},
		},
	}
	writeComposerMetadata(&msg, composerMetadata{Version: 1, Draft: draft, Skills: []string{sandboxSetupSkill}})
	startSandboxInit(ctx.state)
	if err := ctx.startTurn(draft, msg); err != nil {
		ctx.state.sandboxInit.finish()
		if terminalSessionError(err) || context.Cause(ctx.operationContext()) != nil {
			return nil, err
		}
		return []string{"init: " + err.Error()}, nil
	}
	return nil, nil
}

// sandboxInitReady says why /init cannot run in the command's session, nil
// when it can: it needs trials, a top-level session, since the profile is
// the workspace's and an agent's tab is not where the user sets it up, and
// the sandbox-setup skill.
func sandboxInitReady(ctx *replCommandContext) error {
	state := ctx.state
	if err := sandboxTryReady(state); err != nil {
		return err
	}
	md, err := state.session.GetMetadata(ctx.operationContext())
	if err != nil {
		return err
	}
	if md.Parent != "" {
		return errors.New("it runs in a top-level session, not in an agent's")
	}
	if state.skillRuntime == nil || state.skillCatalog == nil {
		return errors.New("it needs polly's skills, which --noskills turns off; /sandbox try works without them")
	}
	if _, ok := state.skillCatalog.Get(sandboxSetupSkill); !ok {
		return fmt.Errorf("it needs the %s skill, which is missing", sandboxSetupSkill)
	}
	return nil
}

// sandboxInitBrief is what /init tells the model besides the skill: the
// workspace, the sandbox, what a trial sees here, the profile as it stands,
// the session's recent failed commands, and the user's notes.
func sandboxInitBrief(ctx *replCommandContext, notes string) string {
	state := ctx.state
	profile := state.sandboxProfile
	ws := profile.ws
	var b strings.Builder
	fmt.Fprintf(&b, "The user ran /init to set up polly's sandbox for this workspace. Follow the %s skill; its %s, %s and %s tools are available now.\n\n",
		sandboxSetupSkill, sandboxPrepareTool, sandboxTrialTool, sandboxProposeTool)
	fmt.Fprintf(&b, "This /init has at most %d model calls. Save useful commands and observed failures to AGENTS.md early, finish within that budget, and do not turn setup into open-ended debugging. A failed test remains a failed test; report Incomplete instead of repeatedly repairing or filtering the suite.\n\n", sandboxInitIterations)
	b.WriteString("Prepare predictable cache, dependency state and non-secret configuration before the first build; ordinary managed preparation needs no permission review. Use existing toolchains, preserve explicit settings, and review new host access. Record bootstrap commands for new worktrees. Report Verified, Verified with sandbox exclusions, or Incomplete; ordinary test failures never qualify as exclusions.\n\n")
	b.WriteString("Finish by updating this workspace's AGENTS.md with build and test commands verified through ordinary sandboxed bash under the resulting settings. Preserve unrelated instructions, record required profile settings, and skip only tests confirmed incompatible with the sandbox, using tested runner filters and explaining the exclusions.\n\n")
	b.WriteString("Before reporting success, read back the dedicated section and repair missing fields even when its commands already work. It must explicitly name the platform and a relative working directory (for example Working directory: repository root). Remove checkout-specific absolute paths from prose as well as commands; use saved shell variables for managed paths.\n\n")
	workspace := homeRelativePath(ws.dir)
	switch {
	case ws.commonDir == "":
		workspace += " (not a Git repository)"
	case ws.origin != "":
		workspace += " (a Git repository; origin " + ws.origin + ")"
	default:
		workspace += " (a Git repository without an origin remote)"
	}
	fmt.Fprintf(&b, "Workspace: %s\n", workspace)
	fmt.Fprintf(&b, "Host platform to record in AGENTS.md: %s/%s (OS and architecture, in addition to tool versions).\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "Sandbox: %s\n", currentSandboxPosture(ctx.configOrDefault(), state).summaryLine(false))
	fmt.Fprintf(&b, "What a trial sees here: %s\n", sandboxInitSight(runtime.GOOS))
	listed := profile.listed()
	fmt.Fprintf(&b, "Workspace profile, %s:", homeRelativePath(ws.profile))
	if len(listed) == 0 {
		b.WriteString(" empty\n")
	} else {
		b.WriteString("\n")
		for i, item := range listed {
			line := "  - " + item.String()
			if item.Automatic {
				line += " (automatic)"
			}
			if profile.sessionOnly(i) {
				line += " (this session only)"
			}
			if i < len(profile.judged) && profile.judged[i].problem != "" {
				line += " (not applied: " + profile.judged[i].problem + ")"
			}
			b.WriteString(line + "\n")
		}
	}
	if profile.off != "" {
		fmt.Fprintf(&b, "The profile is off this launch (%s): what the user saves applies from the next launch, and trials run without it.\n", profile.off)
	}
	for _, a := range profile.profile.Storage.Allocations {
		fmt.Fprintf(&b, "Managed allocation: %s; purpose %q; recipe %q; shared %t. Reuse this declaration.\n", a.Key(), a.Purpose, a.Recipe, a.Shared)
	}
	fmt.Fprintf(&b, "@cache is %s, and @workspace is %s.\n", homeRelativePath(ws.cache), homeRelativePath(ws.dir))
	if dirs := sandboxInitCacheDirs(ws.cache, listed); dirs != "" {
		fmt.Fprintf(&b, "Directories already in @cache, with the profile variables pointing into each: %s\n", dirs)
	}
	if commands, err := recentFailedCommands(ctx.operationContext(), state); err == nil && len(commands) > 0 {
		b.WriteString("Bash commands that failed earlier in this session, the latest first:\n")
		for _, command := range commands[:min(len(commands), sandboxInitCommands)] {
			b.WriteString("  - " + commandLine(command) + "\n")
		}
	}
	if notes != "" {
		fmt.Fprintf(&b, "The user's notes: %s\n", notes)
	}
	return strings.TrimRight(b.String(), "\n")
}

// sandboxInitCacheDirsLimit bounds how many @cache directories the brief
// names.
const sandboxInitCacheDirsLimit = 20

// sandboxInitCacheDirs lists the directories @cache already holds, each with
// the variables among items that point into it, so the model can reuse a
// warm cache under its old name rather than start a cold one beside it. It
// is empty when @cache is missing or holds no directory.
func sandboxInitCacheDirs(cache string, items []sandboxProfileItem) string {
	entries, err := os.ReadDir(cache)
	if err != nil {
		return ""
	}
	users := map[string][]string{}
	for _, item := range items {
		rest, ok := strings.CutPrefix(item.Value, profileCacheVar+"/")
		if item.Kind != profileEnv || item.managed() || !ok {
			continue
		}
		dir, _, _ := strings.Cut(path.Clean(rest), "/")
		users[dir] = append(users[dir], item.Name)
	}
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "managed" {
			continue
		}
		names := "none"
		if len(users[entry.Name()]) > 0 {
			names = strings.Join(users[entry.Name()], ", ")
		}
		dirs = append(dirs, entry.Name()+" ("+names+")")
	}
	if len(dirs) > sandboxInitCacheDirsLimit {
		dirs = append(dirs[:sandboxInitCacheDirsLimit], fmt.Sprintf("and %d more", len(dirs)-sandboxInitCacheDirsLimit))
	}
	return strings.Join(dirs, ", ")
}

// sandboxInitSight says what a trial observes on goos.
func sandboxInitSight(goos string) string {
	switch goos {
	case "darwin":
		return "every read and write the sandbox denies."
	case "linux":
		return "with readable home, command output only: Linux does not report denied operations. With private-home, trials also report discarded writes into the disposable home; hidden reads still look like missing files."
	}
	return "no denials: this platform does not report them."
}

// submitCommandTurnLocked starts a turn a command composed on the visible
// tab, showing display as its prompt. Commands that start turns run only
// while the tab is idle. Caller holds r.model.mu, as commands do.
func (r *managedREPL) submitCommandTurnLocked(display string, msg messages.ChatMessage) error {
	m := r.model
	if m.busy {
		return errors.New("a turn is running")
	}
	turn := cloneManagedTurn(managedTurnInput{displayText: display, userMessage: msg})
	select {
	case r.pending <- pendingTurn{model: m, turn: turn}:
		m.currentPersistence = nil
		m.restoreDraftNext = false
		m.beginManagedTurn(turn)
		return nil
	default:
		return errors.New("the turn queue is unavailable")
	}
}

// ReviewSandboxProposal shows the model's proposal in the sandbox setup
// dialog on this turn's tab and waits for the user's answer. It runs on the
// tool goroutine of the sandbox_propose call; closing the turn withdraws
// the dialog.
func (t *gotuiTurnUI) ReviewSandboxProposal(ctx context.Context, try *sandboxTry) sandboxReview {
	closed := func(why string) sandboxReview { return sandboxReview{outcome: sandboxReviewClosed, why: why} }
	r := t.repl
	if r == nil {
		return closed("nothing on screen can show it")
	}
	reply := make(chan sandboxReview, 1)
	shown := make(chan *sandboxTryDialog, 1)
	if !r.postUI(ctx, func() {
		t.model.mu.Lock()
		defer t.model.mu.Unlock()
		var d *sandboxTryDialog
		if ctx.Err() == nil && t.acceptingLocked() {
			d = r.openSandboxProposal(t.model, try, reply)
		}
		shown <- d
	}) {
		return closed("the turn ended")
	}
	var d *sandboxTryDialog
	select {
	case d = <-shown:
	case <-r.work.ctx.Done():
		return closed("polly is closing")
	}
	if d == nil {
		return closed("the turn ended")
	}
	select {
	case review := <-reply:
		return review
	case <-ctx.Done():
		select {
		case review := <-reply:
			return review
		default:
		}
		r.postUI(r.work.ctx, func() {
			t.model.mu.Lock()
			defer t.model.mu.Unlock()
			r.withdrawSandboxProposal(t.model, d)
		})
		return closed("the turn ended")
	}
}
