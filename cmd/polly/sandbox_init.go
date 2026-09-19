package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// /init sets up the workspace's sandbox with the model's help. It starts a
// turn with the builtin sandbox-setup skill, and while the run lasts the
// session's model has two tools. sandbox_trial runs a command as a sandbox
// trial, adding at most env items that point into the workspace's own
// directories, since those reach nothing of the user's. sandbox_propose
// opens a review of the items the model suggests, in the /sandbox try
// dialog, which only the user answers: the model learns what the user
// allowed and never allows anything itself. Subagents and swarm members never
// get the tools.

const (
	sandboxSetupSkill  = "sandbox-setup"
	sandboxTrialTool   = "sandbox_trial"
	sandboxProposeTool = "sandbox_propose"
	// sandboxInitCancels is how many proposals the user can cancel before
	// the run ends, and the tools refuse until the next /init.
	sandboxInitCancels = 2
	// sandboxInitItems bounds the items one call names.
	sandboxInitItems = 20
	// sandboxInitOutputLines and sandboxInitOutputBytes bound the output a
	// trial returns to the model; its end is kept.
	sandboxInitOutputLines = 200
	sandboxInitOutputBytes = 16 << 10
	// sandboxInitListed bounds the denials and items a report lists.
	sandboxInitListed = 50
	// sandboxInitReason bounds the reason the model gives for an item: two
	// rows of the dialog.
	sandboxInitReason = 200
)

// The codes the tools' errors carry.
const (
	sandboxInitInactive     = "INIT_NOT_ACTIVE"
	sandboxInitBadItem      = "INVALID_ITEM"
	sandboxInitProposalOpen = "PROPOSAL_OPEN"
	sandboxInitTrialFailed  = "TRIAL_FAILED"
	sandboxInitNoUser       = "USER_UNREACHABLE"
)

// sandboxInit is a session's /init: whether a run is live, and how many of
// its proposals the user cancelled.
type sandboxInit struct {
	state *conversationState
	mu    sync.Mutex
	live  bool
	// ended says why the last run ended.
	ended     string
	cancelled int
	proposing bool
	// trial runs the tools' trials one at a time.
	trial sync.Mutex
}

// startSandboxInit starts a /init run in the session state holds. The first
// adds the tools to the session's registry; they stay, and refuse between
// runs.
func startSandboxInit(state *conversationState) {
	if state.sandboxInit == nil {
		state.sandboxInit = &sandboxInit{state: state}
		state.sandboxInit.register(state.toolRegistry)
	}
	s := state.sandboxInit
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live, s.ended, s.cancelled = true, "", 0
}

func (s *sandboxInit) register(registry *tools.ToolRegistry) {
	registry.Register(&tools.Func{
		Name:        sandboxTrialTool,
		LongRunning: true,
		Desc: "Run a command as a sandbox trial during /init: in the workspace, under the sandbox this session's commands get, " +
			"and report what the sandbox denied it. Returns the exit code, the end of the output, each denial, and the " +
			"profile items polly would propose for them. items adds env items for this trial only, and only ones whose " +
			"value is under @cache or @workspace; every other item needs the user, through " + sandboxProposeTool + ".",
		Params: schema.Params{
			"command": schema.S("The command to run with bash -c in the workspace."),
			"items":   schema.Strings("Env items to add for this trial, in /sandbox allow syntax: \"env NAME=@cache/<dir>\" or \"env NAME=@workspace/<dir>\"."),
		},
		Required: []string{"command"},
		Run:      s.runTrial,
	})
	registry.Register(&tools.Func{
		Name:        sandboxProposeTool,
		LongRunning: true,
		Exclusive:   true,
		Desc: "Propose workspace profile items for the user to review during /init. Opens a review polly draws, where every " +
			"item starts unticked: the user ticks what to allow, can run the command again with the ticked items, and saves " +
			"them to the workspace profile, keeps them for this session only, or cancels. Returns what the user allowed and " +
			"where, what they left unticked, and what polly refused and why. Each item is in /sandbox allow syntax: " +
			"\"read <path>\", \"write <path>\", \"env NAME=@cache/<dir>\", \"passenv NAME\" or \"passenv NAME --members\".",
		Params: schema.Params{
			"command": schema.S("The command the user's Try again runs: the one whose failure the items fix."),
			"items": schema.Array("The items, each with the reason the user reads next to it.", map[string]any{
				"type": "object",
				"properties": map[string]any{
					"item":   schema.S("The item, in /sandbox allow syntax."),
					"reason": schema.S("One short sentence: what failed, and why this item fixes it."),
				},
				"required":             []string{"item", "reason"},
				"additionalProperties": false,
			}),
		},
		Required: []string{"command", "items"},
		Run:      s.runPropose,
	})
	registry.MarkAlwaysAllowed(sandboxTrialTool)
	registry.MarkAlwaysAllowed(sandboxProposeTool)
}

// active returns nil while a run is live, else the error the tools answer.
func (s *sandboxInit) active() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live {
		return nil
	}
	return tools.NewToolError(s.ended+"; the user can run /init to start again", sandboxInitInactive)
}

// openProposal claims the one proposal a run has open at a time.
func (s *sandboxInit) openProposal() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proposing {
		return false
	}
	s.proposing = true
	return true
}

// closeProposal releases the proposal after the user's answer, counting a
// cancel, and reports whether it ended the run.
func (s *sandboxInit) closeProposal(cancelled bool) (ended bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proposing = false
	if !cancelled || !s.live {
		return false
	}
	s.cancelled++
	if s.cancelled < sandboxInitCancels {
		return false
	}
	s.live = false
	s.ended = fmt.Sprintf("the user cancelled %d proposals, which ended the sandbox setup", s.cancelled)
	return true
}

// runTrial is sandbox_trial.
func (s *sandboxInit) runTrial(ctx context.Context, args tools.Args) (string, error) {
	if err := s.active(); err != nil {
		return "", err
	}
	command := strings.TrimSpace(args.String("command"))
	if command == "" {
		return "", tools.NewToolError("command is empty", sandboxInitBadItem)
	}
	items := args.StringSlice("items")
	if len(items) > sandboxInitItems {
		return "", tools.NewToolError(fmt.Sprintf("a trial takes at most %d items", sandboxInitItems), sandboxInitBadItem)
	}
	try, err := newSandboxTry(s.state, command)
	if err != nil {
		return "", tools.NewToolError(err.Error(), sandboxInitTrialFailed)
	}
	for _, text := range items {
		p, err := try.suggested(text, "")
		switch {
		case err != nil:
			return "", tools.NewToolError(fmt.Sprintf("%s: %v", text, err), sandboxInitBadItem)
		case p.kind != profileEnv:
			return "", tools.NewToolError(fmt.Sprintf("%s: a trial takes only env items; propose the others with %s, where the user can try them", text, sandboxProposeTool), sandboxInitBadItem)
		case p.refused != "":
			return "", tools.NewToolError(fmt.Sprintf("%s is refused: %s", text, p.refused), sandboxInitBadItem)
		}
		if try.findItem(p) == nil {
			p.ticked = true
			try.rows = append(try.rows, p)
		}
	}
	with := len(try.rows)
	s.trial.Lock()
	defer s.trial.Unlock()
	dropped, err := try.trial(ctx)
	if err != nil {
		return "", tools.NewToolError("the trial did not run: "+err.Error(), sandboxInitTrialFailed)
	}
	return sandboxTrialReportJSON(try, with, dropped)
}

// runPropose is sandbox_propose.
func (s *sandboxInit) runPropose(ctx context.Context, args tools.Args) (string, error) {
	if err := s.active(); err != nil {
		return "", err
	}
	command := strings.TrimSpace(args.String("command"))
	if command == "" {
		return "", tools.NewToolError("command is empty", sandboxInitBadItem)
	}
	suggestions, err := proposedItems(args["items"])
	if err != nil {
		return "", tools.NewToolError(err.Error(), sandboxInitBadItem)
	}
	try, err := newSandboxTry(s.state, command)
	if err != nil {
		return "", tools.NewToolError(err.Error(), sandboxInitTrialFailed)
	}
	try.proposed = true
	for _, suggestion := range suggestions {
		p, err := try.suggested(suggestion.item, suggestion.reason)
		if err != nil {
			return "", tools.NewToolError(fmt.Sprintf("%s: %v", suggestion.item, err), sandboxInitBadItem)
		}
		if try.findItem(p) == nil {
			try.rows = append(try.rows, p)
		}
	}
	if !s.openProposal() {
		return "", tools.NewToolError("another proposal is open; wait for the user's answer", sandboxInitProposalOpen)
	}
	if !slices.ContainsFunc(try.rows, func(p *sandboxProposal) bool { return p.refused == "" }) {
		s.closeProposal(false)
		return sandboxProposalReportJSON(try, sandboxReview{outcome: sandboxReviewRefused}, false)
	}
	reviewer, ok := parentTurnUIFrom(ctx).(sandboxReviewer)
	if !ok {
		s.closeProposal(false)
		return "", tools.NewToolError("the user cannot be asked from here", sandboxInitNoUser)
	}
	review := reviewer.ReviewSandboxProposal(ctx, try)
	ended := s.closeProposal(review.outcome == sandboxReviewCancelled)
	return sandboxProposalReportJSON(try, review, ended)
}

// sandboxSuggestion is an item the model proposes, with its reason.
type sandboxSuggestion struct{ item, reason string }

// proposedItems reads sandbox_propose's items argument.
func proposedItems(raw any) ([]sandboxSuggestion, error) {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, errors.New("items must list at least one item")
	}
	if len(list) > sandboxInitItems {
		return nil, fmt.Errorf("a proposal takes at most %d items", sandboxInitItems)
	}
	suggestions := make([]sandboxSuggestion, 0, len(list))
	for i, entry := range list {
		fields, _ := entry.(map[string]any)
		item, _ := fields["item"].(string)
		reason, _ := fields["reason"].(string)
		item, reason = strings.TrimSpace(item), strings.Join(strings.Fields(reason), " ")
		if item == "" || reason == "" {
			return nil, fmt.Errorf("item %d needs both item and reason", i+1)
		}
		if len(reason) > sandboxInitReason {
			return nil, fmt.Errorf("item %d: keep the reason to one short sentence, at most %d bytes", i+1, sandboxInitReason)
		}
		suggestions = append(suggestions, sandboxSuggestion{item: item, reason: reason})
	}
	return suggestions, nil
}

// suggested is the row for an item the model names in /sandbox allow's
// syntax, judged like a row a trial proposes, and unticked. A path may start
// with ~, and a relative one is taken from the workspace; unlike /sandbox
// allow, a write's directory need not exist, since polly creates a missing
// one in the home directory as it does for a trial's rows. Like a trial's
// rows, no item names the home directory or one of its shared directories
// whole. An item that cannot be read is an error; one the rules refuse is a
// refused row, which the review lists.
func (t *sandboxTry) suggested(text, reason string) (*sandboxProposal, error) {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return nil, errors.New(`an item is a kind and what it names, such as "read ~/.config/tool/config.toml"`)
	}
	p := &sandboxProposal{kind: fields[0], reason: reason}
	switch p.kind {
	case profileRead, profileWrite:
		path := expandHomePath(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), p.kind)))
		if !filepath.IsAbs(path) {
			path = filepath.Join(t.profile.ws.dir, path)
		}
		p.path = filepath.Clean(path)
	case profileEnv, profilePassEnv:
		item, err := parseSandboxProfileItem(t.profile.ws, fields)
		if err != nil {
			return nil, err
		}
		p.name, p.value, p.members = item.Name, item.Value, item.Members
	default:
		return nil, fmt.Errorf("unknown kind %q: an item is read, write, env or passenv", p.kind)
	}
	judge, base := t.judge()
	switch p.kind {
	case profileRead, profileWrite:
		if _, _, why := t.programDir(canonicalProfilePath(p.path)); why != "" {
			p.refused = why
			return p, nil
		}
	case profileEnv:
		if path, err := judge.envValuePath(p.value); err == nil {
			p.path = path
		}
	}
	t.judgeRow(p, judge, base, sandbox.Denial{})
	return p, nil
}

// findItem returns the row granting what p's item does, nil when none.
func (t *sandboxTry) findItem(p *sandboxProposal) *sandboxProposal {
	for _, row := range t.rows {
		if row.kind == p.kind && row.label() == p.label() {
			return row
		}
	}
	return nil
}

// sandboxTrialReport is what sandbox_trial tells the model.
type sandboxTrialReport struct {
	Command  string   `json:"command"`
	ExitCode int      `json:"exit_code"`
	Seconds  float64  `json:"seconds"`
	With     []string `json:"with_items,omitempty"`
	Output   string   `json:"output"`
	// OutputShown says how much of the output Output is, when not all.
	OutputShown string              `json:"output_shown,omitempty"`
	Denials     []sandboxInitDenial `json:"denials"`
	// Proposals are the items polly would propose for the denials, and the
	// denials it cannot propose anything for, with why.
	Proposals []sandboxInitRow `json:"proposals,omitempty"`
	Notes     []string         `json:"notes,omitempty"`
}

type sandboxInitDenial struct {
	Access  string `json:"access"`
	Path    string `json:"path"`
	Process string `json:"process,omitempty"`
	Count   int    `json:"count,omitempty"`
	Cause   string `json:"cause,omitempty"`
	// Discarded marks writes into the private home directory of a Linux
	// sandbox, which succeeded and were thrown away with it.
	Discarded bool `json:"discarded,omitempty"`
	Directory bool `json:"directory,omitempty"`
}

type sandboxInitRow struct {
	Item       string `json:"item"`
	Refused    string `json:"refused,omitempty"`
	Credential bool   `json:"credential,omitempty"`
	NewDir     bool   `json:"creates_directory,omitempty"`
}

// sandboxTrialReportJSON reports the trial try just ran, whose first with
// rows are the items the model added, less those dropped because the rules
// refused them by the time it ran.
func sandboxTrialReportJSON(try *sandboxTry, with int, dropped []*sandboxProposal) (string, error) {
	trial := try.trials[len(try.trials)-1]
	report := sandboxTrialReport{
		Command:  try.command,
		ExitCode: trial.result.ExitCode,
		Seconds:  trial.elapsed.Round(100 * time.Millisecond).Seconds(),
		Denials:  []sandboxInitDenial{},
		Notes:    try.notes(),
	}
	report.Output, report.OutputShown = sandboxInitOutput(trial.result.Output)
	for _, p := range try.rows[:with] {
		if p.ticked {
			report.With = append(report.With, p.label())
		}
	}
	for _, p := range dropped {
		report.Notes = append(report.Notes, fmt.Sprintf("%s was refused, so the trial ran without it: %s.", p.label(), p.refused))
	}
	denials := trial.result.Observation.Denials
	for _, d := range denials[:min(len(denials), sandboxInitListed)] {
		path := d.Path
		if d.Access != sandbox.AccessNetwork {
			path = homeRelativePath(path)
		}
		report.Denials = append(report.Denials, sandboxInitDenial{
			Access: string(d.Access), Path: path, Process: d.Process, Count: d.Count,
			Cause: string(d.Cause), Discarded: d.Discarded, Directory: d.Directory,
		})
	}
	if len(denials) > sandboxInitListed {
		report.Notes = append(report.Notes, fmt.Sprintf("%d more denials are not listed.", len(denials)-sandboxInitListed))
	}
	for _, p := range try.rows[with:] {
		if len(report.Proposals) == sandboxInitListed {
			report.Notes = append(report.Notes, "More proposals are not listed.")
			break
		}
		report.Proposals = append(report.Proposals, sandboxInitRowOf(p))
	}
	return marshalSandboxInitReport(report)
}

func sandboxInitRowOf(p *sandboxProposal) sandboxInitRow {
	return sandboxInitRow{Item: p.label(), Refused: p.refused, Credential: p.credential, NewDir: p.create}
}

// sandboxInitOutput is the end of a trial's output: at most
// sandboxInitOutputLines lines and sandboxInitOutputBytes bytes, and when
// that is not all of it, how much it is.
func sandboxInitOutput(output string) (tail, shown string) {
	if output == "" {
		return "", ""
	}
	lines := strings.Split(output, "\n")
	total := len(lines)
	lines = lines[max(0, total-sandboxInitOutputLines):]
	tail = strings.Join(lines, "\n")
	if len(tail) > sandboxInitOutputBytes {
		tail = tail[len(tail)-sandboxInitOutputBytes:]
		if i := strings.IndexByte(tail, '\n'); i >= 0 {
			tail = tail[i+1:]
		}
		tail = strings.ToValidUTF8(tail, "")
	}
	if kept := strings.Count(tail, "\n") + 1; kept < total {
		shown = fmt.Sprintf("the last %d of %d lines", kept, total)
	}
	return tail, shown
}

// The outcomes of a proposal's review.
const (
	sandboxReviewSaved     = "saved"
	sandboxReviewSession   = "session"
	sandboxReviewCancelled = "cancelled"
	// sandboxReviewClosed: the review ended without the user's answer.
	sandboxReviewClosed = "closed"
	// sandboxReviewRefused: the rules refuse every item, so nobody was asked.
	sandboxReviewRefused = "refused"
)

// sandboxReview is how a proposal's review ended: its outcome, and why when
// it closed without the user's answer.
type sandboxReview struct {
	outcome string
	why     string
}

// sandboxReviewer is the TurnUI capability sandbox_propose needs: a review
// of the model's proposal that only the user answers. It returns once the
// user allows or cancels, or when ctx ends.
type sandboxReviewer interface {
	ReviewSandboxProposal(ctx context.Context, try *sandboxTry) sandboxReview
}

// sandboxProposalReport is what sandbox_propose tells the model: never the
// output of a trial the user ran, which may hold what a ticked credential
// let the command read.
type sandboxProposalReport struct {
	Outcome  string           `json:"outcome"`
	Allowed  []string         `json:"allowed,omitempty"`
	Unticked []string         `json:"left_unticked,omitempty"`
	Refused  []sandboxInitRow `json:"refused,omitempty"`
	Trials   []string         `json:"user_trials,omitempty"`
	Note     string           `json:"note,omitempty"`
}

func sandboxProposalReportJSON(try *sandboxTry, review sandboxReview, ended bool) (string, error) {
	report := sandboxProposalReport{Outcome: review.outcome}
	allowed := review.outcome == sandboxReviewSaved || review.outcome == sandboxReviewSession
	for _, p := range try.rows {
		switch {
		case p.refused != "":
			if len(report.Refused) < sandboxInitListed {
				report.Refused = append(report.Refused, sandboxInitRowOf(p))
			}
		case allowed && p.ticked:
			report.Allowed = append(report.Allowed, p.label())
		case len(report.Unticked) < sandboxInitListed:
			report.Unticked = append(report.Unticked, p.label())
		}
	}
	for i := range try.trials {
		report.Trials = append(report.Trials, try.trials[i].summary(i+1))
	}
	switch review.outcome {
	case sandboxReviewSaved:
		report.Note = "The user allowed these and saved them to the workspace profile; they apply to every sandboxed command now, your bash included."
	case sandboxReviewSession:
		report.Note = "The user allowed these for this session only; they apply to every sandboxed command now, your bash included, and end with the session."
	case sandboxReviewCancelled:
		report.Note = "The user cancelled, and nothing was allowed. Do not propose these items again; ask the user what they want."
		if ended {
			report.Note += " That was the second cancelled proposal, so the sandbox setup tools stop until the user runs /init again."
		}
	case sandboxReviewClosed:
		report.Note = "The review closed without the user's answer, and nothing was allowed"
		if review.why != "" {
			report.Note += ": " + review.why
		}
		report.Note += "."
	case sandboxReviewRefused:
		report.Note = "Polly refuses every item, so the user was not asked."
	}
	return marshalSandboxInitReport(report)
}

func marshalSandboxInitReport(report any) (string, error) {
	data, err := json.Marshal(report)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
