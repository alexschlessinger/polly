package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	ui "github.com/metaspartan/gotui/v5"
)

// scriptedTrials stands in for RunTrial: each call returns the next result
// and records the candidate it ran with.
type scriptedTrials struct {
	results    []tools.TrialResult
	candidates []sandbox.Config
}

func (s *scriptedTrials) run(_ context.Context, _ string, candidate sandbox.Config) (tools.TrialResult, error) {
	s.candidates = append(s.candidates, candidate)
	if len(s.results) == 0 {
		return tools.TrialResult{}, errors.New("no scripted trial left")
	}
	result := s.results[0]
	s.results = s.results[1:]
	return result, nil
}

func denied(denials ...sandbox.Denial) tools.TrialResult {
	return tools.TrialResult{ExitCode: 1, Observation: sandbox.Observation{Denials: denials}}
}

// sandboxTryState is a session in the profile test workspace, its bash under
// a no-op sandbox, with the workspace's profile opened the way a start opens
// it.
func sandboxTryState(t *testing.T) (home string, state *conversationState) {
	t.Helper()
	home, _ = profileTestHome(t)
	state = &conversationState{
		session:        testAcquireSession(t, testOpenMemoryStore(t, nil), "ctx"),
		toolRegistry:   addDirRegistry(t),
		sandboxProfile: openSandboxProfile(&Config{}),
	}
	if _, err := state.toolRegistry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	return home, state
}

func scriptedTry(t *testing.T, state *conversationState, results ...tools.TrialResult) (*sandboxTry, *scriptedTrials) {
	t.Helper()
	try, err := newSandboxTry(state, "make")
	if err != nil {
		t.Fatal(err)
	}
	trials := &scriptedTrials{results: results}
	try.run = trials.run
	return try, trials
}

func rowByLabel(t *testing.T, try *sandboxTry, label string) (int, *sandboxProposal) {
	t.Helper()
	for i, p := range try.rows {
		if p.label() == label {
			return i, p
		}
	}
	t.Fatalf("no row %q among %v", label, rowLabels(try))
	return 0, nil
}

func rowLabels(try *sandboxTry) []string {
	labels := make([]string, len(try.rows))
	for i, p := range try.rows {
		labels[i] = p.label() + " [" + p.badge() + "]"
	}
	return labels
}

func TestSandboxTryProposesAGrantForEachDenial(t *testing.T) {
	home, state := sandboxTryState(t)
	at := func(rel string) string { return filepath.Join(home, filepath.FromSlash(rel)) }
	writeFile(t, at("Library/Application Support/tool/env"), "")
	writeFile(t, at("Library/Application Support/tool/other"), "")
	for _, name := range []string{"a", "b", "c", "d"} {
		writeFile(t, at(".m2/repository/"+name+".jar"), "")
	}
	writeFile(t, at(".npmrc"), "")
	writeFile(t, at(".zshrc"), "")
	mkdirs(t, at(".cache"), at("Library/Caches"))
	read := func(path string, cause sandbox.DenialCause) sandbox.Denial {
		return sandbox.Denial{Access: sandbox.AccessRead, Path: path, Cause: cause, Count: 1, Process: "tool"}
	}
	write := func(path string) sandbox.Denial {
		return sandbox.Denial{Access: sandbox.AccessWrite, Path: path, Cause: sandbox.CauseNotWritable, Count: 1, Process: "tool"}
	}
	try, trials := scriptedTry(t, state,
		denied(
			read(at("Library/Application Support/tool/env"), sandbox.CausePrivate),
			read(at(".m2/repository/a.jar"), sandbox.CausePrivate),
			read(at(".m2/repository/b.jar"), sandbox.CausePrivate),
			read(at(".m2/repository/c.jar"), sandbox.CausePrivate),
			read(at(".m2/repository/d.jar"), sandbox.CausePrivate),
			read(at(".npmrc"), sandbox.CauseMasked),
			write(at("Library/Caches/go-build/01/x-d")),
			write(at(".cache/newtool")),
			write(at(".lesshst")),
			write(at(".zshrc")),
			write(home),
			read(at(".config"), sandbox.CausePrivate),
			sandbox.Denial{Access: sandbox.AccessNetwork, Path: "*:443", Cause: sandbox.CauseNetwork, Count: 1},
			read("/dev/ptmx", sandbox.CauseUnexplained),
		),
		denied(
			write(at(".cache/newtool/sub/file")),
			read(at("Library/Application Support/tool/other"), sandbox.CausePrivate),
		),
	)
	if _, err := try.trial(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"read ~/.npmrc [credential]",
		"write ~/Library/Caches/go-build [new directory]",
		"write ~/.cache/newtool [new directory]",
		"write ~/.lesshst [refused]",
		"write ~/.zshrc [refused]",
		"write ~ [not proposed]",
		"read ~/.config [not proposed]",
		"network *:443 [not proposed]",
		"read /dev/ptmx [not proposed]",
		// A program's configuration file keeps its own path; four reads in
		// one program's directory make one row for the directory.
		"read ~/Library/Application Support/tool/env []",
		"read ~/.m2 []",
	}
	if got := rowLabels(try); !slices.Equal(got, want) {
		t.Fatalf("rows after trial 1:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for label, reason := range map[string]string{
		"write ~/.lesshst": "cannot tell whether the command meant a file or a directory",
		"write ~/.zshrc":   "where the host runs code from",
		"write ~":          "home directory itself",
		"read ~/.config":   "holds every program's files",
		"network *:443":    "keeps the network off",
		"read /dev/ptmx":   "reason polly does not model",
	} {
		if _, p := rowByLabel(t, try, label); !strings.Contains(p.refused, reason) {
			t.Errorf("%s refused %q, want it to say %q", label, p.refused, reason)
		}
	}
	for _, p := range try.rows {
		if p.ticked {
			t.Fatalf("%s starts ticked; every row starts unticked", p.label())
		}
	}

	i, _ := rowByLabel(t, try, "write ~/.zshrc")
	if err := try.toggle(i); err == nil || !strings.Contains(err.Error(), "cannot be allowed") {
		t.Fatalf("ticking a refused row = %v", err)
	}
	if n := try.tickReads(); n != 2 {
		t.Fatalf("tickReads ticked %d rows, want the two plain reads and not the credential", n)
	}
	i, _ = rowByLabel(t, try, "write ~/Library/Caches/go-build")
	if err := try.toggle(i); err != nil {
		t.Fatal(err)
	}

	if _, err := try.trial(context.Background()); err != nil {
		t.Fatal(err)
	}
	candidate := trials.candidates[1]
	if !slices.Equal(candidate.ReadPaths, []string{at("Library/Application Support/tool/env"), at(".m2")}) || !slices.Equal(candidate.WritablePaths, []string{at("Library/Caches/go-build")}) {
		t.Fatalf("trial 2 ran with %+v", candidate)
	}
	if info, err := os.Stat(at("Library/Caches/go-build")); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("the ticked write's directory = %v, %v; want it created 0700 before the trial", info, err)
	}
	for label, badge := range map[string]string{
		"read ~/Library/Application Support/tool/env": "✓ cleared",
		"read ~/.m2":                      "✓ cleared",
		"write ~/Library/Caches/go-build": "✓ cleared",
		"write ~/.cache/newtool":          "new directory",
		// A read seen for the first time in the directory of a row that
		// keeps its own path gets its own row.
		"read ~/Library/Application Support/tool/other": "",
	} {
		if _, p := rowByLabel(t, try, label); p.badge() != badge {
			t.Errorf("%s badge %q after trial 2, want %q", label, p.badge(), badge)
		}
	}

	lines, err := try.allow(true)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "allowed write ~/Library/Caches/go-build, read ~/Library/Application Support/tool/env, read ~/.m2 and saved to the workspace profile") {
		t.Fatalf("allow(save) = %q", got)
	}
	saved, err := readSandboxProfile(state.sandboxProfile.ws.profile)
	if err != nil || len(saved.Items) != 3 {
		t.Fatalf("profile file = %+v, %v; want the three ticked items", saved, err)
	}
	if cfg, _, err := state.toolRegistry.SandboxReadPolicy(); err != nil || !slices.Contains(cfg.ReadPaths, at(".m2")) || !slices.Contains(cfg.WritablePaths, at("Library/Caches/go-build")) {
		t.Fatalf("the session's policy after save = %+v, %v", cfg, err)
	}

	// A credential goes to this session alone, and never to the file.
	for _, p := range try.rows {
		p.ticked = false
	}
	i, _ = rowByLabel(t, try, "read ~/.npmrc")
	if err := try.toggle(i); err != nil {
		t.Fatal(err)
	}
	lines, err = try.allow(false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "allowed read ~/.npmrc for this session only") || !strings.Contains(got, "is a credential") {
		t.Fatalf("allow(session) = %q", got)
	}
	if saved, _ := readSandboxProfile(state.sandboxProfile.ws.profile); len(saved.Items) != 3 {
		t.Fatalf("a session allow wrote the file: %+v", saved.Items)
	}
	if got := state.sandboxProfile.summary(); got != "profile: 4 items (1 this session only)" {
		t.Fatalf("summary = %q", got)
	}

	var out bytes.Buffer
	ctx := newWriterReplCommandContext(&Config{}, state, &out)
	if _, _, err := defaultReplCommands.dispatch("/sandbox", ctx); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "4. read ~/.npmrc · this session only · credential") {
		t.Fatalf("/sandbox show = %q", got)
	}
	out.Reset()
	if _, _, err := defaultReplCommands.dispatch("/sandbox forget 4", ctx); err != nil {
		t.Fatal(err)
	}
	if len(state.sandboxProfile.session) != 0 || len(state.sandboxProfile.profile.Items) != 3 {
		t.Fatalf("forget of the session item left session %v, file %v", state.sandboxProfile.session, state.sandboxProfile.profile.Items)
	}
	if cfg, _, _ := state.toolRegistry.SandboxReadPolicy(); slices.Contains(cfg.ReadPaths, at(".npmrc")) {
		t.Fatal("the forgotten session item still applies")
	}
}

func TestSandboxTryDiscardedWritesAndDirectories(t *testing.T) {
	home, state := sandboxTryState(t)
	at := func(rel string) string { return filepath.Join(home, filepath.FromSlash(rel)) }
	discarded := func(path string, dir bool, count int) sandbox.Denial {
		return sandbox.Denial{Access: sandbox.AccessWrite, Path: path, Cause: sandbox.CauseNotWritable, Count: count, Discarded: true, Directory: dir}
	}
	try, _ := scriptedTry(t, state, denied(
		discarded(at(".cache/go-build"), true, 120),
		discarded(at(".npm"), true, 40),
		discarded(at(".lesshst"), false, 1),
		// mkdir says what it made where nothing else does.
		sandbox.Denial{Access: sandbox.AccessWrite, Path: at(".newtool"), Cause: sandbox.CauseNotWritable, Count: 1, Process: "mkdir"},
	))
	if _, err := try.trial(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"write ~/.cache/go-build [new directory]",
		"write ~/.npm [new directory]",
		"write ~/.lesshst [refused]",
		"write ~/.newtool [new directory]",
	}
	if got := rowLabels(try); !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if _, p := rowByLabel(t, try, "write ~/.lesshst"); !strings.Contains(p.refused, "wrote a file there") {
		t.Fatalf("a discarded file's row refused %q", p.refused)
	}
	if _, p := rowByLabel(t, try, "write ~/.cache/go-build"); !strings.Contains(strings.Join(p.details(), " "), "wrote 120 entries here in trial 1") {
		t.Fatalf("a discarded write's details = %q", p.details())
	}
}

func TestSandboxTryReviewsAsText(t *testing.T) {
	home, state := sandboxTryState(t)
	writeFile(t, filepath.Join(home, ".toolrc"), "")
	try, trials := scriptedTry(t, state,
		denied(sandbox.Denial{Access: sandbox.AccessRead, Path: filepath.Join(home, ".toolrc"), Cause: sandbox.CausePrivate, Count: 1}),
		tools.TrialResult{Output: "built"},
	)
	var out bytes.Buffer
	ctx := newWriterReplCommandContext(&Config{}, state, &out)
	answers := []string{"tick 9", "tick 1", "try", "output", "save"}
	ctx.readInput = func(string) (string, error) {
		if len(answers) == 0 {
			return "", errors.New("no answer left")
		}
		answer := answers[0]
		answers = answers[1:]
		return answer, nil
	}
	lines := reviewSandboxTryLines(ctx, try)
	got := out.String() + strings.Join(lines, "\n")
	for _, want := range []string{
		"sandbox try: trial 1: running make",
		"1. [ ] read ~/.toolrc",
		"sandbox try: there is no row 9",
		"1. [x] read ~/.toolrc",
		"sandbox try: trial 2: running make with 1 ticked item",
		"1. [x] read ~/.toolrc · ✓ cleared",
		"built",
		"allowed read ~/.toolrc and saved to the workspace profile",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the text review lacks %q:\n%s", want, got)
		}
	}
	if len(trials.candidates) != 2 {
		t.Fatalf("ran %d trials, want 2", len(trials.candidates))
	}

	// Where nothing can be asked, the review is printed and nothing allowed.
	try, _ = scriptedTry(t, state, denied())
	out.Reset()
	ctx.readInput = nil
	lines = reviewSandboxTryLines(ctx, try)
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "nothing allowed; answering needs a terminal") {
		t.Fatalf("a review without a terminal = %q", got)
	}
}

func TestSandboxTryCommandRunsATrial(t *testing.T) {
	_, state := sandboxTryState(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	var out bytes.Buffer
	ctx := newWriterReplCommandContext(&Config{}, state, &out)
	// The no-op sandbox cannot be observed, so the trial runs and says so;
	// the command keeps its quoting.
	if _, _, err := defaultReplCommands.dispatch(`/sandbox try  printf '%s  %s' a b; exit 3`, ctx); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"trial 1: running printf '%s  %s' a b; exit 3", "trial 1 · exit 3", "Denials were not all seen", "nothing allowed; answering needs a terminal"} {
		if !strings.Contains(got, want) {
			t.Errorf("/sandbox try output lacks %q:\n%s", want, got)
		}
	}

	state.sandboxProfile = nil
	out.Reset()
	if _, _, err := defaultReplCommands.dispatch("/sandbox try make", ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "the sandbox is off") {
		t.Fatalf("/sandbox try under --nosandbox = %q", out.String())
	}
}

func TestRecentFailedCommands(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "ctx")
	call := func(id, command string) messages.ChatMessageToolCall {
		return messages.ChatMessageToolCall{ID: id, Name: "bash", Arguments: `{"command":` + strconv.Quote(command) + `}`}
	}
	result := func(id string, code int) messages.ChatMessage {
		return messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: id, Content: "out", Metadata: map[string]any{"tool_data": map[string]any{"exitCode": float64(code)}}}
	}
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "build it"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call("1", "go build ./..."), call("2", "ls")}},
		result("1", 1), result("2", 0),
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call("3", "npm test"), call("4", "go build ./...")}},
		result("3", 2), result("4", 1),
	}
	if err := session.AddMessages(context.Background(), history); err != nil {
		t.Fatal(err)
	}
	got, err := recentFailedCommands(context.Background(), &conversationState{session: session})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"go build ./...", "npm test"}; !slices.Equal(got, want) {
		t.Fatalf("recentFailedCommands = %v, want %v", got, want)
	}
}

func TestSandboxTryDialog(t *testing.T) {
	home, _ := profileTestHome(t)
	writeFile(t, filepath.Join(home, ".toolrc"), "")
	r := sandboxCommandREPL(t, &Config{})
	try, err := newSandboxTry(r.state, "make")
	if err != nil {
		t.Fatal(err)
	}
	trials := &scriptedTrials{results: []tools.TrialResult{
		denied(sandbox.Denial{Access: sandbox.AccessRead, Path: filepath.Join(home, ".toolrc"), Cause: sandbox.CausePrivate, Count: 1}),
		{Output: "built"},
	}}
	try.run = trials.run
	key := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }
	paint := func() string {
		return strings.NewReplacer("\ufdd0", "[", "\ufdd1", "]").Replace(plainStyledText(r.model.modal.text(40, 100)))
	}

	r.model.mu.Lock()
	r.openSandboxTry(try)
	r.model.mu.Unlock()
	d := r.model.modal.sandboxTry
	if got := paint(); !strings.Contains(got, "trial 1 running") {
		t.Fatalf("the running dialog = %q", got)
	}
	runUITask(t, r)

	// Keys count only once the review is on screen, and never inside a paste.
	key(" ")
	if try.rows[0].ticked {
		t.Fatal("a key before the review was painted ticked a row")
	}
	if got := paint(); !strings.Contains(got, "› [ ] read     ~/.toolrc") || !strings.Contains(got, "› [ Cancel ]") || !strings.Contains(got, "Denied 1× in trial 1.") {
		t.Fatalf("the review = %q", got)
	}
	key(pasteStartID)
	key(" ")
	key(pasteEndID)
	if try.rows[0].ticked {
		t.Fatal("a pasted space ticked a row")
	}
	key(" ")
	if !try.rows[0].ticked {
		t.Fatal("Space did not tick the selected row")
	}

	// Tab moves from Cancel to Try again, which runs the second trial.
	key("<Tab>")
	key("<Enter>")
	if !d.running {
		t.Fatal("Try again did not start a trial")
	}
	runUITask(t, r)
	paint()
	if got := try.rows[0].badge(); got != "✓ cleared" {
		t.Fatalf("badge after the second trial = %q", got)
	}

	// Allowing waits for an idle session.
	d.focus = sandboxTrySaveButton
	r.model.busy = true
	key("<Enter>")
	if r.model.modal == nil || !strings.Contains(d.status, "a turn is running") {
		t.Fatalf("Save during a turn: modal %v, status %q", r.model.modal, d.status)
	}
	r.model.busy = false
	clearTranscriptForTest(r.model)
	key("<Enter>")
	if r.model.modal != nil {
		t.Fatal("Save left the dialog open")
	}
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "allowed read ~/.toolrc and saved to the workspace profile") {
		t.Fatalf("after Save = %q", got)
	}
	if saved, _ := readSandboxProfile(r.state.sandboxProfile.ws.profile); len(saved.Items) != 1 {
		t.Fatalf("profile file = %+v", saved.Items)
	}

	// Escape closes the dialog and stops a running trial.
	try, _ = newSandboxTry(r.state, "make")
	started := make(chan struct{})
	try.run = func(ctx context.Context, _ string, _ sandbox.Config) (tools.TrialResult, error) {
		close(started)
		<-ctx.Done()
		return tools.TrialResult{}, ctx.Err()
	}
	r.model.mu.Lock()
	r.openSandboxTry(try)
	r.model.mu.Unlock()
	<-started
	clearTranscriptForTest(r.model)
	key("<Escape>")
	if r.model.modal != nil {
		t.Fatal("Escape left the dialog open")
	}
	runUITask(t, r)
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "sandbox try: nothing allowed") {
		t.Fatalf("after Escape = %q", got)
	}
}

// TestSandboxTryUnderTheRealSandbox tries a command that reads a hidden file
// and makes a cache directory in the private home: the first trial proposes
// both (Linux sees only the write), the second, with them ticked, is denied
// neither, and saving them lets the session's bash run the command.
func TestSandboxTryUnderTheRealSandbox(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process sandbox")
	}
	home, _ := profileTestHome(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	writeFile(t, filepath.Join(home, ".toolrc"), "setting\n")
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
	command := "cat ~/.toolrc; mkdir -p ~/.cache/trial-tool/objects && echo x > ~/.cache/trial-tool/objects/a"
	try, err := newSandboxTry(state, command)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := try.trial(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"write ~/.cache/trial-tool"}
	if runtime.GOOS == "darwin" {
		want = append(want, "read ~/.toolrc")
	}
	for _, label := range want {
		i, p := rowByLabel(t, try, label)
		if p.refused != "" {
			t.Fatalf("%s refused: %s", label, p.refused)
		}
		if err := try.toggle(i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := try.trial(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code := try.trials[1].result.ExitCode; code != 0 {
		t.Fatalf("trial 2 exited %d: %s", code, try.trials[1].result.Output)
	}
	for _, label := range want {
		if _, p := rowByLabel(t, try, label); !p.cleared {
			t.Fatalf("%s after trial 2 = %q; rows %v", label, p.badge(), rowLabels(try))
		}
	}
	if _, err := try.allow(true); err != nil {
		t.Fatal(err)
	}
	bash, err := registry.LoadToolAuto("bash")
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := registry.Get(bash.Servers[0].ToolNames[0])
	out, err := tool.Execute(context.Background(), map[string]any{"command": command + " && echo done"})
	if err != nil || !strings.Contains(out, "done") {
		t.Fatalf("bash under the saved profile: %q %v", out, err)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(out, "setting") {
		t.Fatalf("bash did not read ~/.toolrc under the saved profile: %q", out)
	}
}
