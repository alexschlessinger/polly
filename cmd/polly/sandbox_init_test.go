package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
	ui "github.com/metaspartan/gotui/v5"
)

// scriptedReviewer stands in for the user: it answers each proposal's review
// with answer, which may tick rows and run trials first.
type scriptedReviewer struct {
	TurnUI
	answer  func(*sandboxTry) string
	reviews []*sandboxTry
}

func (s *scriptedReviewer) ReviewSandboxProposal(_ context.Context, try *sandboxTry) sandboxReview {
	s.reviews = append(s.reviews, try)
	return sandboxReview{outcome: s.answer(try)}
}

// callSandboxTool runs one of /sandbox-init's tools as the model would, and
// returns
// its result, or the code of the error it answered.
func callSandboxTool(t *testing.T, ctx context.Context, state *conversationState, name string, args map[string]any) (string, string) {
	t.Helper()
	tool, ok := state.toolRegistry.Get(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	out, err := tool.Execute(ctx, args)
	if err != nil {
		var toolErr *tools.ToolError
		if !errors.As(err, &toolErr) {
			t.Fatalf("%s failed without a tool error: %v", name, err)
		}
		return toolErr.Message, toolErr.Code
	}
	return out, ""
}

func proposal(items ...string) map[string]any {
	var list []any
	for i := 0; i < len(items); i += 2 {
		list = append(list, map[string]any{"item": items[i], "reason": items[i+1]})
	}
	return map[string]any{"command": "make", "items": list}
}

func TestSandboxSetupJudgesTheModelsItems(t *testing.T) {
	home, state := sandboxTryState(t)
	at := func(rel string) string { return filepath.Join(home, filepath.FromSlash(rel)) }
	writeFile(t, at(".config/tool/config.toml"), "")
	writeFile(t, at(".npmrc"), "")
	mkdirs(t, at(".cache"))
	t.Setenv("NPM_TOKEN", "secret")
	try, trials := scriptedTry(t, state, tools.TrialResult{})
	ws := state.sandboxProfile.ws

	for _, c := range []struct {
		item, label, badge, refused string
	}{
		{item: "read ~/.config/tool/config.toml", label: "read ~/.config/tool/config.toml"},
		{item: "read ~/.npmrc", label: "read ~/.npmrc", badge: "credential"},
		{item: "read ~/.missing", label: "read ~/.missing", refused: "does not exist now"},
		{item: "write ~/.cache/tool", label: "write ~/.cache/tool", badge: "new directory"},
		{item: "write ~/.cache", label: "write ~/.cache", refused: "holds every program's files"},
		{item: "write ~", label: "write ~", refused: "home directory itself"},
		{item: "read src", label: "read " + homeRelativePath(filepath.Join(ws.dir, "src")), refused: "inside the workspace"},
		{item: "env TOOL_CACHE=@cache/tool", label: "env TOOL_CACHE=@cache/tool"},
		{item: "env PATH=@cache/bin", label: "env PATH=@cache/bin", refused: "shell's own environment"},
		{item: "env TOOL_HOME=/tmp/tool", label: "env TOOL_HOME=/tmp/tool", refused: "must start with @workspace or @cache"},
		{item: "passenv NPM_TOKEN", label: "passenv NPM_TOKEN", badge: "credential"},
		{item: "passenv LANG", label: "passenv LANG", refused: "does not strip it"},
	} {
		p, err := try.suggested(c.item, "because")
		if err != nil {
			t.Fatalf("%s: %v", c.item, err)
		}
		if p.label() != c.label || p.badge() != c.badge && c.refused == "" || !strings.Contains(p.refused, c.refused) || (c.refused == "") != (p.refused == "") {
			t.Errorf("%s = %s [%s] refused %q; want %s [%s] refused %q", c.item, p.label(), p.badge(), p.refused, c.label, c.badge, c.refused)
		}
		if p.ticked || p.reason != "because" {
			t.Errorf("%s: ticked %v, reason %q", c.item, p.ticked, p.reason)
		}
		try.rows = append(try.rows, p)
	}
	for _, bad := range []string{"read", "mount /opt", "passenv A B"} {
		if _, err := try.suggested(bad, ""); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	env := rowFor(t, try, "env TOOL_CACHE=@cache/tool")
	if details := strings.Join(env.details(), " "); !strings.Contains(details, "TOOL_CACHE set to "+homeRelativePath(filepath.Join(ws.cache, "tool"))) || env.reasonLine() != "The model's reason: because" {
		t.Fatalf("an env row's details = %q, reason %q", details, env.reasonLine())
	}
	if got := plainStyledText(sandboxTryRowText(env, false, 98)); !strings.Contains(got, "env      TOOL_CACHE=@cache/tool") {
		t.Fatalf("an env row reads %q", got)
	}
	// The model's reason takes what room polly's own lines leave, so it never
	// hides a warning.
	credential := rowFor(t, try, "read ~/.npmrc")
	credential.reason = strings.Repeat("this is needed ", 40)
	if rows := plainStyledText(strings.Join(sandboxTryDetailRowsOf(credential, 60), "\n")); !strings.Contains(rows, "A credential") || !strings.Contains(rows, "The model's reason") || len(strings.Split(rows, "\n")) != sandboxTryDetailRows {
		t.Fatalf("a credential's detail rows with a long reason:\n%s", rows)
	}

	for _, label := range []string{"write ~/.cache/tool", "env TOOL_CACHE=@cache/tool", "passenv NPM_TOKEN"} {
		rowFor(t, try, label).ticked = true
	}
	if _, err := try.trial(context.Background()); err != nil {
		t.Fatal(err)
	}
	candidate := trials.candidates[0]
	if candidate.Env["TOOL_CACHE"] != filepath.Join(ws.cache, "tool") || !slices.Equal(candidate.PassEnv, []string{"NPM_TOKEN"}) || !slices.Equal(candidate.WritablePaths, []string{at(".cache/tool"), ws.cache}) {
		t.Fatalf("the trial ran with %+v", candidate)
	}
	if info, err := os.Stat(ws.cache); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the workspace cache directory = %v, %v; want it made 0700 for the trial", info, err)
	}
	// A trial speaks for paths, never for a variable.
	if p := rowFor(t, try, "env TOOL_CACHE=@cache/tool"); p.cleared || p.badge() != "" {
		t.Fatalf("an env row after its trial: cleared %v, badge %q", p.cleared, p.badge())
	}

	lines, err := try.allow(false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "allowed write ~/.cache/tool, env TOOL_CACHE=@cache/tool, passenv NPM_TOKEN for this session only") {
		t.Fatalf("allow = %q", got)
	}
	session := state.sandboxProfile.session
	if len(session) != 3 || !session[2].Credential || session[1].Value != "@cache/tool" {
		t.Fatalf("session items = %+v", session)
	}
}

func rowFor(t *testing.T, try *sandboxTry, label string) *sandboxProposal {
	t.Helper()
	_, p := rowByLabel(t, try, label)
	return p
}

func TestSandboxTrialTool(t *testing.T) {
	_, state := sandboxTryState(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	startSandboxInit(state)
	ctx := context.Background()

	// The test registry's sandbox is a stand-in that neither applies a
	// policy nor can be observed, so the trial runs and says it saw nothing.
	out, code := callSandboxTool(t, ctx, state, sandboxTrialTool, map[string]any{
		"command": `printf 'ran\n'; exit 3`,
		"items":   []any{"env TOOL_CACHE=@cache/tool"},
	})
	if code != "" {
		t.Fatalf("sandbox_trial = %s: %s", code, out)
	}
	var report sandboxTrialReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.ExitCode != 3 || report.Output != "ran" || !slices.Equal(report.With, []string{"env TOOL_CACHE=@cache/tool"}) {
		t.Fatalf("report = %+v", report)
	}
	if _, err := os.Stat(state.sandboxProfile.ws.cache); err != nil {
		t.Fatalf("the trial did not make the workspace cache directory its env item names: %v", err)
	}
	if len(report.Notes) == 0 || !strings.Contains(report.Notes[0], "Denials were not all seen") {
		t.Fatalf("notes = %v", report.Notes)
	}

	for _, c := range []struct{ item, want string }{
		{"read ~/.npmrc", "only env items"},
		{"env PATH=@cache/bin", "is refused"},
		{"env", "an item is a kind"},
	} {
		msg, code := callSandboxTool(t, ctx, state, sandboxTrialTool, map[string]any{"command": "true", "items": []any{c.item}})
		if code != sandboxInitBadItem || !strings.Contains(msg, c.want) {
			t.Errorf("a trial with %q = %s %q", c.item, code, msg)
		}
	}
}

func TestSandboxProposeTool(t *testing.T) {
	home, state := sandboxTryState(t)
	writeFile(t, filepath.Join(home, ".toolrc"), "")
	startSandboxInit(state)
	reviewer := &scriptedReviewer{}
	ctx := withParentTurnUI(context.Background(), reviewer)
	propose := func(args map[string]any) (sandboxProposalReport, string) {
		t.Helper()
		out, code := callSandboxTool(t, ctx, state, sandboxProposeTool, args)
		var report sandboxProposalReport
		if code == "" {
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatal(err)
			}
		}
		return report, code
	}

	// The user ticks the read, runs the command with it, and keeps it for
	// the session; the report names what was allowed, never the output.
	reviewer.answer = func(try *sandboxTry) string {
		if !try.proposed || len(try.rows) != 2 {
			t.Fatalf("the review got %v", rowLabels(try))
		}
		try.run = (&scriptedTrials{results: []tools.TrialResult{{Output: "the secret it read"}}}).run
		rowFor(t, try, "read ~/.toolrc").ticked = true
		if _, err := try.trial(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := try.allow(false); err != nil {
			t.Fatal(err)
		}
		return sandboxReviewSession
	}
	report, code := propose(proposal("read ~/.toolrc", "the build reads its settings", "write ~", "it writes there", "read ~/.toolrc", "twice"))
	if code != "" {
		t.Fatalf("sandbox_propose = %s", code)
	}
	if report.Outcome != sandboxReviewSession || !slices.Equal(report.Allowed, []string{"read ~/.toolrc"}) || len(report.Refused) != 1 || report.Refused[0].Item != "write ~" || len(report.Trials) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if data, _ := json.Marshal(report); strings.Contains(string(data), "the secret it read") {
		t.Fatalf("the report carries the trial's output: %s", data)
	}
	if len(state.sandboxProfile.session) != 1 {
		t.Fatalf("session items = %+v", state.sandboxProfile.session)
	}

	// Nothing the rules allow means nobody is asked.
	report, _ = propose(proposal("write ~", "it writes there"))
	if report.Outcome != sandboxReviewRefused || len(reviewer.reviews) != 1 {
		t.Fatalf("an all-refused proposal = %+v after %d reviews", report, len(reviewer.reviews))
	}
	if _, code := propose(proposal("mount /opt", "why not")); code != sandboxInitBadItem {
		t.Fatalf("an unreadable item = %s", code)
	}
	if _, code := callSandboxTool(t, context.Background(), state, sandboxProposeTool, proposal("read ~/.toolrc", "again")); code != sandboxInitNoUser {
		t.Fatalf("a proposal nobody can answer = %s", code)
	}

	// The second cancel ends the run until the next /sandbox-init.
	reviewer.answer = func(*sandboxTry) string { return sandboxReviewCancelled }
	report, _ = propose(proposal("read ~/.toolrc", "again"))
	if report.Outcome != sandboxReviewCancelled || !strings.Contains(report.Note, "Do not propose these items again") || strings.Contains(report.Note, "second cancelled") {
		t.Fatalf("the first cancel = %+v", report)
	}
	report, _ = propose(proposal("read ~/.toolrc", "again"))
	if !strings.Contains(report.Note, "second cancelled proposal") {
		t.Fatalf("the second cancel = %+v", report)
	}
	for _, name := range []string{sandboxProposeTool, sandboxTrialTool} {
		if msg, code := callSandboxTool(t, ctx, state, name, proposal("read ~/.toolrc", "again")); code != sandboxInitInactive || !strings.Contains(msg, "run /sandbox-init to start again") {
			t.Fatalf("%s after the run ended = %s %q", name, code, msg)
		}
	}
	startSandboxInit(state)
	if _, code := propose(proposal("read ~/.toolrc", "again")); code != "" {
		t.Fatalf("a proposal after /sandbox-init again = %s", code)
	}
}

// sandboxInitState is a sandboxTryState session that has polly's builtin
// skills, as /sandbox-init needs.
func sandboxInitState(t *testing.T) *conversationState {
	t.Helper()
	_, state := sandboxTryState(t)
	catalog, err := skills.LoadBuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := newSkillRuntime(catalog, state.toolRegistry)
	if err != nil {
		t.Fatal(err)
	}
	state.skillCatalog, state.skillRuntime = catalog, runtime
	return state
}

func TestSandboxInitCacheDirs(t *testing.T) {
	cache := t.TempDir()
	for _, dir := range []string{"build", "mod", "orphan"} {
		if err := os.MkdirAll(filepath.Join(cache, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cache, "note"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	items := []sandboxProfileItem{
		{Kind: profileEnv, Name: "BUILD_CACHE", Value: "@cache/build"},
		{Kind: profileEnv, Name: "MOD_CACHE", Value: "@cache/mod/v2"},
		{Kind: profileEnv, Name: "MOD_TMP", Value: "@cache/./mod/tmp"},
		{Kind: profileEnv, Name: "OUT", Value: "@workspace/build"},
		{Kind: profileRead, Path: "/opt/build"},
	}
	want := "build (BUILD_CACHE), mod (MOD_CACHE, MOD_TMP), orphan (none)"
	if got := sandboxInitCacheDirs(cache, items); got != want {
		t.Fatalf("sandboxInitCacheDirs = %q, want %q", got, want)
	}
	if got := sandboxInitCacheDirs(filepath.Join(cache, "missing"), items); got != "" {
		t.Fatalf("sandboxInitCacheDirs(missing) = %q, want nothing", got)
	}
}

func TestInitCommandStartsTheSetupTurn(t *testing.T) {
	state := sandboxInitState(t)
	if _, ok := state.toolRegistry.Get(sandboxTrialTool); ok {
		t.Fatal("the setup tools exist before /sandbox-init")
	}
	var out bytes.Buffer
	ctx := newWriterReplCommandContext(&Config{}, state, &out)
	var display string
	var started messages.ChatMessage
	ctx.startTurn = func(d string, msg messages.ChatMessage) error {
		display, started = d, msg
		return nil
	}
	if err := os.MkdirAll(filepath.Join(state.sandboxProfile.ws.cache, "tool-cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := defaultReplCommands.dispatch("/sandbox-init  the build is make ci", ctx); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || display != "/sandbox-init the build is make ci" {
		t.Fatalf("/sandbox-init printed %q and started %q", out.String(), display)
	}
	md, ok := readComposerMetadata(started)
	if !ok || md.Draft != display || !slices.Equal(md.Skills, []string{sandboxSetupSkill}) {
		t.Fatalf("the turn's metadata = %+v", md)
	}
	brief := started.Parts[1].Text
	for _, want := range []string{"The user ran /sandbox-init", "Workspace: ", "Sandbox: ", "What a trial sees here: ", "Workspace profile, ", ": empty", "@cache is ", "already in @cache, with the profile variables pointing into each: tool-cache (none)", "The user's notes: the build is make ci"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief lacks %q:\n%s", want, brief)
		}
	}
	for _, name := range []string{sandboxTrialTool, sandboxProposeTool} {
		if _, ok := state.toolRegistry.Get(name); !ok {
			t.Fatalf("/sandbox-init did not add %s", name)
		}
	}
	// The skill activates as the turn runs, and teaches the tools.
	prepared, err := activateComposerSkills(context.Background(), state, started)
	if err != nil {
		t.Fatal(err)
	}
	if last := prepared.Parts[len(prepared.Parts)-1]; last.FileName != "skill:"+sandboxSetupSkill || !strings.Contains(last.Text, sandboxProposeTool) {
		t.Fatalf("the activated skill part = %+v", last)
	}

	for _, c := range []struct {
		name  string
		setup func(*conversationState)
		want  string
	}{
		{"no skills", func(s *conversationState) { s.skillCatalog, s.skillRuntime = nil, nil }, "--noskills"},
		{"no sandbox", func(s *conversationState) { s.sandboxProfile = nil }, "relaunch with --sandbox default"},
		{"an agent's session", func(s *conversationState) {
			store := testOpenMemoryStore(t, nil)
			testAcquireSession(t, store, "root")
			child, err := store.Acquire(context.Background(), "child", sessions.AcquireOptions{Parent: "root"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = child.Close() })
			s.session = child
		}, "top-level session"},
	} {
		state := sandboxInitState(t)
		c.setup(state)
		out.Reset()
		ctx := newWriterReplCommandContext(&Config{}, state, &out)
		ctx.startTurn = func(string, messages.ChatMessage) error {
			t.Fatalf("%s: /sandbox-init started a turn", c.name)
			return nil
		}
		if _, _, err := defaultReplCommands.dispatch("/sandbox-init", ctx); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), c.want) {
			t.Errorf("/sandbox-init with %s = %q, want %q", c.name, out.String(), c.want)
		}
	}
}

func TestSandboxProposalReviewsAsText(t *testing.T) {
	home, state := sandboxTryState(t)
	writeFile(t, filepath.Join(home, ".toolrc"), "")
	try, trials := scriptedTry(t, state, tools.TrialResult{Output: "built"})
	try.proposed = true
	p, err := try.suggested("read ~/.toolrc", "the build reads its settings")
	if err != nil {
		t.Fatal(err)
	}
	try.rows = append(try.rows, p)
	var errOut bytes.Buffer
	line := &lineTurnUI{config: &Config{}, writer: &bytes.Buffer{}, errWriter: &errOut, interactive: true}
	line.input = bufio.NewReader(strings.NewReader("output\ntick 1\ntry\nsave\n"))
	review := line.ReviewSandboxProposal(context.Background(), try)
	if review.outcome != sandboxReviewSaved || len(trials.candidates) != 1 {
		t.Fatalf("review = %+v after %d trials", review, len(trials.candidates))
	}
	got := errOut.String()
	for _, want := range []string{
		"sandbox setup: the model proposes 1 item for make",
		"1. [ ] read ~/.toolrc",
		"The model's reason: the build reads its settings",
		"sandbox setup: no trial has run yet",
		"sandbox setup: trial 1: running make with 1 ticked item",
		"allowed read ~/.toolrc and saved to the workspace profile",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the text review lacks %q:\n%s", want, got)
		}
	}

	// Without a terminal to answer from, nothing is asked.
	line.input = nil
	if review := line.ReviewSandboxProposal(context.Background(), try); review.outcome != sandboxReviewClosed || !strings.Contains(review.why, "not a terminal") {
		t.Fatalf("a review without a terminal = %+v", review)
	}
}

func TestSandboxProposalDialog(t *testing.T) {
	home, _ := profileTestHome(t)
	writeFile(t, filepath.Join(home, ".toolrc"), "")
	r := sandboxCommandREPL(t, &Config{})
	newProposal := func() *sandboxTry {
		try, err := newSandboxTry(r.state, "make")
		if err != nil {
			t.Fatal(err)
		}
		try.proposed = true
		for _, item := range []string{"read ~/.toolrc", "write ~"} {
			p, err := try.suggested(item, "the build needs it")
			if err != nil {
				t.Fatal(err)
			}
			try.rows = append(try.rows, p)
		}
		return try
	}
	key := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }
	paint := func() string {
		return strings.NewReplacer("\ufdd0", "[", "\ufdd1", "]").Replace(plainStyledText(r.model.modal.text(40, 100)))
	}
	turn := &gotuiTurnUI{repl: r, model: r.model, config: &Config{}, state: r.state}
	review := func(ctx context.Context, try *sandboxTry) chan sandboxReview {
		done := make(chan sandboxReview, 1)
		go func() { done <- turn.ReviewSandboxProposal(ctx, try) }()
		runUITask(t, r)
		return done
	}

	// The review opens on the turn's tab before any trial, the user's keys
	// counting once it is painted, and saving answers even though the turn
	// that waits on it is running.
	try := newProposal()
	done := review(context.Background(), try)
	if r.model.modal == nil || r.model.modal.title != "Sandbox setup" {
		t.Fatalf("modal = %+v", r.model.modal)
	}
	key(" ")
	if try.rows[0].ticked {
		t.Fatal("a key before the review was painted ticked a row")
	}
	if got := paint(); !strings.Contains(got, "The model proposes 2 items") || !strings.Contains(got, "The model's reason: the build needs it") || !strings.Contains(got, "› [ Cancel ]") {
		t.Fatalf("the proposal = %q", got)
	}
	key(" ")
	r.model.busy = true
	r.model.modal.sandboxTry.focus = sandboxTrySaveButton
	key("<Enter>")
	if got := <-done; got.outcome != sandboxReviewSaved {
		t.Fatalf("Save answered %+v", got)
	}
	if r.model.modal != nil {
		t.Fatal("Save left the dialog open")
	}
	if saved, _ := readSandboxProfile(r.state.sandboxProfile.ws.profile); len(saved.Items) != 1 {
		t.Fatalf("profile file = %+v", saved.Items)
	}

	// A proposal waits for the dialog open before it, and survives another
	// dialog opening over it; Escape cancels it.
	r.openContextPopover()
	try = newProposal()
	done = review(context.Background(), try)
	if r.model.modal.sandboxTry != nil || r.model.pendingModal == nil {
		t.Fatal("the proposal replaced the open dialog")
	}
	key("<Escape>")
	if r.model.modal == nil || r.model.modal.sandboxTry == nil {
		t.Fatal("closing the dialog did not show the waiting proposal")
	}
	r.openContextPopover()
	key("<Escape>")
	if r.model.modal == nil || r.model.modal.sandboxTry == nil {
		t.Fatal("a dialog opened over the proposal lost it")
	}
	paint()
	clearTranscriptForTest(r.model)
	key("<Escape>")
	if got := <-done; got.outcome != sandboxReviewCancelled {
		t.Fatalf("Escape answered %+v", got)
	}
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "sandbox setup: nothing allowed") {
		t.Fatalf("after Escape = %q", got)
	}

	// A turn that ends takes its proposal away.
	ctx, cancel := context.WithCancel(context.Background())
	done = review(ctx, newProposal())
	cancel()
	runUITask(t, r)
	if got := <-done; got.outcome != sandboxReviewClosed || r.model.modal != nil {
		t.Fatalf("after the turn ended: %+v, modal %v", got, r.model.modal)
	}
}

// TestSandboxTrialToolUnderTheRealSandbox runs sandbox_trial under the real
// sandbox: a command writing into its own cache directory is denied it, and
// with an env item pointing the cache into @cache it runs, writing there.
func TestSandboxTrialToolUnderTheRealSandbox(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process sandbox")
	}
	home, _ := profileTestHome(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	mkdirs(t, filepath.Join(home, ".cache"))
	opts, probe, profile, err := sandboxRegistryOptionsWithWarnings(&Config{SandboxPreset: "base+private-home"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewToolRegistry(nil, opts...)
	t.Cleanup(func() { _ = registry.Close() })
	state := &conversationState{session: testAcquireSession(t, testOpenMemoryStore(t, nil), "ctx"), toolRegistry: registry, sandboxProfile: profile}
	startSandboxInit(state)
	command := `mkdir -p "${TOOL_CACHE:-$HOME/.cache/trial-tool}/objects" && echo cached > "${TOOL_CACHE:-$HOME/.cache/trial-tool}/objects/a" && echo done`

	out, code := callSandboxTool(t, context.Background(), state, sandboxTrialTool, map[string]any{"command": command})
	if code != "" {
		t.Fatalf("sandbox_trial = %s: %s", code, out)
	}
	var report sandboxTrialReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(report.Proposals, func(row sandboxInitRow) bool { return row.Item == "write ~/.cache/trial-tool" }) {
		t.Fatalf("the trial without the item proposed %+v; report %s", report.Proposals, out)
	}

	out, code = callSandboxTool(t, context.Background(), state, sandboxTrialTool, map[string]any{"command": command, "items": []any{"env TOOL_CACHE=@cache/trial-tool"}})
	if code != "" {
		t.Fatalf("sandbox_trial = %s: %s", code, out)
	}
	report = sandboxTrialReport{}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.ExitCode != 0 || !strings.Contains(report.Output, "done") {
		t.Fatalf("the trial with the redirect = %s", out)
	}
	written := filepath.Join(state.sandboxProfile.ws.cache, "trial-tool", "objects", "a")
	if data, err := os.ReadFile(written); err != nil || string(data) != "cached\n" {
		t.Fatalf("the redirected cache holds %q, %v", data, err)
	}
}

func TestInitIterationCapPersistsPartialTurnAndEndsSetup(t *testing.T) {
	model := &scriptedStreamLLM{}
	for i := 0; i < sandboxInitIterations+1; i++ {
		model.responses = append(model.responses, messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: fmt.Sprintf("call-%d", i), Name: "probe", Arguments: "{}"}}})
	}
	calls := 0
	state, session := newInterruptedTurnState(t, model, &tools.Func{Name: "probe", Run: func(context.Context, tools.Args) (string, error) { calls++; return "ran", nil }})
	state.sandboxInit = &sandboxInit{state: state, live: true}
	var out, errOut bytes.Buffer
	ui := newLineTurnUI(&Config{}, nil)
	ui.writer, ui.errWriter = &out, &errOut
	code, err := executeTurnWithUserMessage(context.Background(), &Config{}, state, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "/sandbox-init"}, nil, nil, ui, false)
	if code != 3 || !errors.Is(err, llm.ErrMaxIterations) || !strings.Contains(err.Error(), "/sandbox-init reached its iteration limit") || calls != sandboxInitIterations || model.calls != sandboxInitIterations {
		t.Fatalf("code=%d err=%v tools=%d calls=%d", code, err, calls, model.calls)
	}
	if state.sandboxInit.active() == nil {
		t.Fatal("init remained active")
	}
	history := testSessionHistory(t, session)
	if len(history) != 2*sandboxInitIterations+2 {
		t.Fatalf("partial turn lost messages: %d", len(history))
	}
	assertInterruptedMarker(t, history[len(history)-1], "/sandbox-init reached its iteration limit")
}
