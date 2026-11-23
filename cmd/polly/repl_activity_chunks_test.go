package main

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

// onlyChunks returns the chunks of the model's one activity line.
func onlyChunks(t *testing.T, m *replModel) []activityChunk {
	t.Helper()
	blocks := activityBlocks(m, 120)
	if len(blocks) != 1 {
		t.Fatalf("got %d activity lines, want one", len(blocks))
	}
	return blocks[0].chunks
}

// chunkHeaders are the chunks' labels with their failure cues, as the folded
// line reads before sentence case.
func chunkHeaders(chunks []activityChunk) []string {
	var out []string
	for _, c := range chunks {
		text := c.label
		if c.mark != "" {
			text += chunkMarkSeparator + c.mark
		}
		out = append(out, text)
	}
	return out
}

// chunkCalls are the identities of a chunk's calls, in the order it lists
// them.
func chunkCalls(c activityChunk) []string {
	var out []string
	for _, p := range c.parts {
		if p.thought == nil {
			out = append(out, callIdentity(p.tools.rows[p.row]))
		}
	}
	return out
}

func bashCall(id, command string) messages.ChatMessageToolCall {
	return messages.ChatMessageToolCall{ID: id, Name: "bash", Arguments: fmt.Sprintf(`{"command":%q}`, command)}
}

func editCall(id string) messages.ChatMessageToolCall {
	return messages.ChatMessageToolCall{ID: id, Name: "edit_file", Arguments: fmt.Sprintf(`{"path":"%s.go"}`, id)}
}

// chunkTurn runs one live turn: each step is one model response whose calls
// start together and end in order. fail names the calls that return an
// error; deny names the ones the user refuses.
type chunkTurn struct {
	steps [][]messages.ChatMessageToolCall
	fail  []string
	deny  []string
}

func (turn chunkTurn) run(t *testing.T) *replModel {
	t.Helper()
	r, _ := turn.start(t)
	return r.model
}

// start runs the turn's steps and leaves the turn running, with the screen
// it reports to, so a test can go on to answer or end it.
func (turn chunkTurn) start(t *testing.T) (*managedREPL, *gotuiTurnUI) {
	t.Helper()
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("work")
	ui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	for step, calls := range turn.steps {
		ui.ModelAttemptStarted(step, 0)
		ui.AppendToolStart(calls)
		for _, c := range calls {
			switch {
			case slices.Contains(turn.deny, c.ID):
				ui.AppendToolEnd(c, llm.ToolDeniedContent, 0, nil)
			case slices.Contains(turn.fail, c.ID):
				ui.AppendToolEnd(c, "boom", time.Millisecond, errors.New("exit 1"))
			default:
				ui.AppendToolEnd(c, "ok", time.Millisecond, nil)
			}
		}
	}
	return r, ui
}

// An edit, a test and an agent launch each end a run. Reads, commands,
// viewed images and every other tool share one, counted by kind in a fixed
// order; a run of one call names that call. Calls asked together stay
// together: the batch joins the run of its weightiest call, whose header
// counts what came with it.
func TestRunsEndAtEditsTestsAndAgentLaunches(t *testing.T) {
	type calls = []messages.ChatMessageToolCall
	view := messages.ChatMessageToolCall{ID: "v", Name: "view_image", Arguments: `{"source":"shots/logo.png"}`}
	lookup := messages.ChatMessageToolCall{ID: "l", Name: "lookup", Arguments: `{}`}
	for _, tc := range []struct {
		name  string
		steps []calls
		want  []string
	}{
		{"looking around is one run", []calls{{readCall("a")}, {bashCall("ls", "ls -la")}, {view}, {lookup}, {readCall("b")}},
			[]string{"read 2 files, viewed logo.png, ran a command and used a tool"}},
		{"an edit ends the run", []calls{{readCall("a")}, {editCall("a2")}, {readCall("b")}},
			[]string{"read a.go", "edited a2.go", "read b.go"}},
		{"a test ends the run", []calls{{readCall("a")}, {bashCall("t", "go test ./...")}, {bashCall("ls", "ls -la")}},
			[]string{"read a.go", "tests passed", "ran ls -la"}},
		{"an agent launch ends the run", []calls{{readCall("a")}, {agentCall("child", `{}`)}, {readCall("b")}},
			[]string{"read a.go", "1 agent", "read b.go"}},
		{"edits share a run", []calls{{editCall("a")}, {messages.ChatMessageToolCall{ID: "w", Name: "write_file", Arguments: `{"path":"b.go"}`}}},
			[]string{"edited 2 files"}},
		{"test runs share a run", []calls{{bashCall("t1", "go test ./a")}, {bashCall("t2", "go test ./b")}},
			[]string{"tests passed"}},
		{"a batch of one kind joins its run", []calls{{readCall("a")}, {readCall("b"), readCall("c")}},
			[]string{"read 3 files"}},
		{"a batch with an edit is an edit run", []calls{{readCall("a")}, {readCall("b"), editCall("c")}, {readCall("d")}},
			[]string{"read a.go", "edited c.go and read b.go", "read d.go"}},
		{"an edit run takes in what was asked beside its edits", []calls{{editCall("a")}, {bashCall("rg", "rg -n needle ."), editCall("b")}, {editCall("c")}},
			[]string{"edited 3 files and ran a command"}},
		{"a test beside other commands is one of them", []calls{{bashCall("b", "go build ./...")}, {bashCall("t", "go test ./..."), bashCall("v", "go vet ./...")}, {bashCall("ls", "ls -la")}},
			[]string{"ran 4 commands"}},
		{"tests asked together are a test run", []calls{{readCall("a")}, {bashCall("t1", "go test ./a"), bashCall("t2", "go test ./b")}},
			[]string{"read a.go", "tests passed"}},
		{"a launch takes in what was asked beside it", []calls{{readCall("a")}, {agentCall("child", `{}`), readCall("b")}, {readCall("c")}},
			[]string{"read a.go", "1 agent and read b.go", "read c.go"}},
		{"one command is named", []calls{{bashCall("b", "go build ./...")}}, []string{"ran go build ./..."}},
		{"one tool is named", []calls{{lookup}}, []string{"used lookup"}},
		{"one read is named", []calls{{readCall("main")}}, []string{"read main.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := chunkTurn{steps: tc.steps}.run(t)
			chunks := onlyChunks(t, m)
			if got := chunkHeaders(chunks); !slices.Equal(got, tc.want) {
				t.Fatalf("headers %q, want %q", got, tc.want)
			}
			// Every call is behind exactly one header, in the order asked.
			var listed, asked []string
			for _, c := range chunks {
				listed = append(listed, chunkCalls(c)...)
			}
			for _, step := range tc.steps {
				for _, c := range step {
					_, row := m.toolDisclosureRowForCall(c.ID)
					asked = append(asked, callIdentity(*row))
				}
			}
			if !slices.Equal(listed, asked) {
				t.Fatalf("chunks list %q, the turn asked %q", listed, asked)
			}
		})
	}
}

// A call that failed or was denied marks its run and stays in it: the run
// is what was asked, so a late result cannot split a chunk. A lone call that
// did not succeed is named as it was asked.
func TestFailureMarksItsRunAndNeverSplitsIt(t *testing.T) {
	type calls = []messages.ChatMessageToolCall
	for _, tc := range []struct {
		name       string
		steps      []calls
		fail, deny []string
		want       []string
	}{
		{"one of several failed", []calls{{readCall("a")}, {readCall("b")}, {readCall("c")}}, []string{"b"}, nil,
			[]string{"read 3 files · 1 failed"}},
		{"denied and failed are told apart", []calls{{readCall("a")}, {readCall("b")}, {readCall("c")}}, []string{"b"}, []string{"c"},
			[]string{"read 3 files · 1 denied, 1 failed"}},
		{"a command that failed did run", []calls{{bashCall("b", "go build ./...")}}, []string{"b"}, nil,
			[]string{"ran go build ./... · failed"}},
		{"a denied command did not", []calls{{bashCall("b", "rm -rf build")}}, nil, []string{"b"},
			[]string{"run rm -rf build · denied"}},
		{"a denied edit edited nothing", []calls{{editCall("main")}}, nil, []string{"main"},
			[]string{"edit main.go · denied"}},
		{"a denied tool call was not used", []calls{{messages.ChatMessageToolCall{ID: "l", Name: "lookup"}}}, nil, []string{"l"},
			[]string{"use lookup · denied"}},
		{"a failed test is the whole label", []calls{{bashCall("t", "go test ./...")}}, []string{"t"}, nil,
			[]string{"tests failed"}},
		{"one of two test runs failed", []calls{{bashCall("t1", "go test ./a")}, {bashCall("t2", "go test ./b")}}, []string{"t2"}, nil,
			[]string{"2 test runs · 1 failed"}},
		{"a failure does not end the run", []calls{{readCall("a")}, {bashCall("b", "false")}, {readCall("c")}}, []string{"b"}, nil,
			[]string{"read 2 files and ran a command · 1 failed"}},
		{"a failed test among commands is a failed command", []calls{{bashCall("t", "go test ./..."), bashCall("v", "go vet ./...")}}, []string{"t"}, nil,
			[]string{"ran 2 commands · 1 failed"}},
		{"a failure asked beside an edit marks the edit run", []calls{{editCall("a"), bashCall("b", "false")}}, []string{"b"}, nil,
			[]string{"edited a.go and ran a command · 1 failed"}},
		{"a failure asked beside a launch marks the launch's run", []calls{{agentCall("child", `{}`), bashCall("b", "false")}}, []string{"b"}, nil,
			[]string{"1 agent and ran a command · 1 failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks := onlyChunks(t, chunkTurn{steps: tc.steps, fail: tc.fail, deny: tc.deny}.run(t))
			if got := chunkHeaders(chunks); !slices.Equal(got, tc.want) {
				t.Fatalf("headers %q, want %q", got, tc.want)
			}
			for _, c := range chunks {
				if !c.failed || c.live {
					t.Fatalf("chunk %q: failed=%v live=%v, want a settled failure", c.label, c.failed, c.live)
				}
			}
		})
	}
}

// A turn that looks around, edits and checks folds to three rows, however its
// calls were batched: a search asked beside an edit stays in the edit run, and
// the full check asked at once (tests, build, vet and a script that failed)
// stays with the commands before and after it.
func TestBatchesFoldIntoTheRunsAroundThem(t *testing.T) {
	type calls = []messages.ChatMessageToolCall
	title := messages.ChatMessageToolCall{ID: "title", Name: "set_session_title", Arguments: `{"title":"t"}`}
	script := func(id string) messages.ChatMessageToolCall {
		return bashCall(id, "set -e -o pipefail; git diff --check; git ls-files -z | xargs -0 gofmt -l")
	}
	m := chunkTurn{
		steps: []calls{
			{title, bashCall("status", "git status --short")},
			{bashCall("rg1", "rg -n holds ."), readCall("a"), readCall("b")},
			{readCall("c"), bashCall("rg2", "rg -n label .")},
			{editCall("d"), editCall("e")},
			{bashCall("rg3", "rg -n time ."), editCall("f")},
			{editCall("g")},
			{bashCall("fmt", "set -e -o pipefail; gofmt -w d.go; go test ./cmd/polly -run TestHold")},
			{bashCall("test", "go test ./..."), bashCall("build", "go build ./..."), bashCall("vet", "go vet ./..."), script("check")},
			{script("recheck")},
		},
		fail: []string{"check"},
	}.run(t)
	chunks := onlyChunks(t, m)
	want := []string{
		"read 3 files, ran 3 commands and used a tool",
		"edited 4 files and ran a command",
		"ran 6 commands · 1 failed",
	}
	if got := chunkHeaders(chunks); !slices.Equal(got, want) {
		t.Fatalf("headers %q, want %q", got, want)
	}
	if got := []int{chunks[0].calls, chunks[1].calls, chunks[2].calls}; !slices.Equal(got, []int{7, 5, 6}) {
		t.Fatalf("calls per chunk = %v", got)
	}
}

// While the calls of a batch run, the header counts them when more than one
// is in flight and names the one left when the others have settled. A test
// asked beside another command is a command in both.
func TestBatchLiveHeaderCountsThenNamesTheCallInFlight(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("check")
	ui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	test, vet := bashCall("t", "go test ./..."), bashCall("v", "go vet ./...")
	header := func(want string, live bool) {
		t.Helper()
		chunks := onlyChunks(t, m)
		if got := chunkHeaders(chunks); len(got) != 1 || got[0] != want || chunks[0].live != live {
			t.Fatalf("headers %q (live %v), want %q (live %v)", got, chunks[0].live, want, live)
		}
	}
	ui.AppendToolStart([]messages.ChatMessageToolCall{test, vet})
	header("running 2 commands", true)
	ui.AppendToolEnd(vet, "ok", time.Millisecond, nil)
	header("running go test ./...", true)
	ui.AppendToolEnd(test, "ok", time.Millisecond, nil)
	header("ran 2 commands", false)
}

// A hundred calls are one honest count behind one header, none dropped.
func TestChunkCountsEveryCall(t *testing.T) {
	var steps [][]messages.ChatMessageToolCall
	for batch := range 10 {
		var calls []messages.ChatMessageToolCall
		for i := range 10 {
			calls = append(calls, readCall(fmt.Sprintf("f%02d", batch*10+i)))
		}
		steps = append(steps, calls)
	}
	chunks := onlyChunks(t, chunkTurn{steps: steps}.run(t))
	if len(chunks) != 1 || chunks[0].label != "read 100 files" || chunks[0].calls != 100 || len(chunkCalls(chunks[0])) != 100 {
		t.Fatalf("chunks = %q with %d calls", chunkHeaders(chunks), chunks[0].calls)
	}
}

// The same transcript folds the same way when reloaded. Neither the bounded
// reasoning preview nor the order parallel calls finish in is an event
// stream. Two thoughts here are parts of one record, and two calls reuse a
// provider ID across responses.
func TestChunksLiveAndHydrated(t *testing.T) {
	withDisplayTTY(t)
	call := func(id, host, name, args string) messages.ChatMessageToolCall {
		return messages.ChatMessageToolCall{ID: id, DisplayID: host, Name: name, Arguments: args}
	}
	steps := [][]messages.ChatMessageToolCall{
		{call("a", "A", "read_file", `{"path":"src/n.go"}`), call("b", "B", "read_file", `{"path":"src/n.go"}`), call("i", "I", "view_image", `{"source":"https://assets.example/img/logo.png?key=secret"}`)},
		{call("a", "C", "edit_file", `{"path":"src/n.go"}`), call("d", "D", "write_file", `{"path":"test/n.go"}`)},
		{call("t", "T", "bash", `{"command":"go test ./..."}`)},
	}
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	ui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "inspect"}}
	for step, calls := range steps {
		ui.ModelAttemptStarted(step, 0)
		response := messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: calls}
		if step < 2 {
			response.Reasoning = "look"
			d := time.Duration(step+1) * time.Second
			response.SetThinkingDuration(d)
			ui.ShowThinking(response.Reasoning)
			m.thinkingSegmentStart = time.Now().Add(-d)
		}
		history = append(history, response)
		ui.AppendToolStart(calls)
		if step == 0 {
			// Calls of different kinds in flight are counted, not named.
			if got := onlyChunks(t, m); len(got) != 1 || got[0].label != "running 3 tools…" || !got[0].live {
				t.Fatalf("parallel start: %q", chunkHeaders(got))
			}
		}
		for _, c := range slices.Backward(calls) {
			var err error
			if step == 2 {
				err = errors.New("exit 1")
			}
			ui.AppendToolEnd(c, "result", time.Millisecond, err)
			result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolName: c.Name, ToolCallID: c.ID, Content: "result", Metadata: map[string]any{messages.MetadataKeyDisplayID: c.DisplayID}}
			result.SetToolSucceeded(err == nil)
			history = append(history, result)
		}
	}
	ui.AppendAssistantText("done")
	r.endTurn(nil)
	history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done"})
	h := newReplModel()
	h.hydrateHistory(history, "ctx")
	for name, model := range map[string]*replModel{"live": m, "hydrated": h} {
		got := onlyChunks(t, model)
		if want := []string{"read n.go and viewed logo.png", "edited 2 files", "tests failed"}; !slices.Equal(chunkHeaders(got), want) {
			t.Fatalf("%s headers %q, want %q", name, chunkHeaders(got), want)
		}
		if !slices.Equal(chunkCalls(got[0]), []string{"A", "B", "I"}) || !slices.Equal(chunkCalls(got[1]), []string{"C", "D"}) || !slices.Equal(chunkCalls(got[2]), []string{"T"}) {
			t.Fatalf("%s calls: %q %q %q", name, chunkCalls(got[0]), chunkCalls(got[1]), chunkCalls(got[2]))
		}
		// A fold is keyed by its chunk's first call, the same live and saved.
		if got[0].key != "call:A" || got[1].key != "call:C" || got[2].key != "call:T" {
			t.Fatalf("%s keys: %q %q %q", name, got[0].key, got[1].key, got[2].key)
		}
		// Each step's thought leads its own chunk: two parts of one record.
		first, second := got[0].parts[0], got[1].parts[0]
		if first.thought == nil || second.thought != first.thought || first.segment == second.segment || got[2].parts[0].thought != nil {
			t.Fatalf("%s thoughts: %+v / %+v", name, first, second)
		}
		if a, _ := model.thoughtLabel(first, false); a != "thought · 1s" {
			t.Fatalf("%s first thought row = %q", name, a)
		}
		if b, _ := model.thoughtLabel(second, false); b != "thought · 2s" {
			t.Fatalf("%s second thought row = %q", name, b)
		}
		if got[2].live || !got[2].failed || got[0].failed || got[1].failed {
			t.Fatalf("%s outcomes: %+v", name, chunkHeaders(got))
		}
	}
}

// A thought goes with the calls its response made, as the first row of their
// chunk; one that led no call is the last row of the run before it, and
// stands alone only when there is none. Neither changes the key a chunk's
// fold is kept under: that is the chunk's first call, whatever joins the
// chunk later.
func TestThoughtsGoWithTheCallsTheyLed(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	ui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}

	// A response that only thinks: its thought stands alone, as a sentence.
	ui.ModelAttemptStarted(0, 0)
	ui.ShowThinking("where to start")
	m.thinkingSegmentStart = time.Now().Add(-3 * time.Second)
	alone := onlyChunks(t, m)
	if len(alone) != 1 || alone[0].group != chunkThought || alone[0].kind != activityThought || !alone[0].live || !strings.HasPrefix(alone[0].label, "thinking… 3s") {
		t.Fatalf("a streaming thought = %+v", chunkHeaders(alone))
	}
	thoughtKey := alone[0].key

	// Its calls arrive: the thought becomes the first row of their chunk,
	// which is keyed by the first call.
	ui.AppendToolStart([]messages.ChatMessageToolCall{readCall("a")})
	ui.AppendToolEnd(readCall("a"), "ok", time.Millisecond, nil)
	led := onlyChunks(t, m)
	if len(led) != 1 || led[0].group != chunkShared || led[0].key == thoughtKey || !strings.HasPrefix(led[0].key, "call:") || led[0].parts[0].thought == nil || led[0].calls != 1 {
		t.Fatalf("a thought that led a call = %+v key %q", chunkHeaders(led), led[0].key)
	}
	key := led[0].key
	if label, live := m.thoughtLabel(led[0].parts[0], false); label != "thought · 3s" || live {
		t.Fatalf("the thought's row = %q live=%v", label, live)
	}

	// A later call joins the run and a later thought is taken in with it:
	// same chunk, same key.
	ui.ModelAttemptStarted(1, 0)
	ui.ShowThinking("and the caller")
	ui.AppendToolStart([]messages.ChatMessageToolCall{readCall("b")})
	ui.AppendToolEnd(readCall("b"), "ok", time.Millisecond, nil)
	joined := onlyChunks(t, m)
	if len(joined) != 1 || joined[0].key != key || joined[0].label != "read 2 files" || len(joined[0].parts) != 4 {
		t.Fatalf("a joining call = %+v key %q parts %d", chunkHeaders(joined), joined[0].key, len(joined[0].parts))
	}
	for i, wantThought := range []bool{true, false, true, false} {
		if (joined[0].parts[i].thought != nil) != wantThought {
			t.Fatalf("part %d: thought=%v, want each step's thought above its call", i, joined[0].parts[i].thought != nil)
		}
	}

	// A last thought that leads no call closes the run it followed: one more
	// row behind the same fold, under the same key and the same header.
	ui.ModelAttemptStarted(2, 0)
	ui.ShowThinking("that is enough")
	ui.ThinkingEnded()
	last := onlyChunks(t, m)
	if len(last) != 1 || last[0].key != key || last[0].label != "read 2 files" || last[0].live || len(last[0].parts) != 5 || last[0].parts[4].thought == nil {
		t.Fatalf("a trailing thought = %+v with %d parts", chunkHeaders(last), len(last[0].parts))
	}
	if !strings.HasPrefix(thoughtKey, "thought:") {
		t.Fatalf("a standalone thought is keyed by its own part: %q", thoughtKey)
	}
}

// A notice between two batches splits the activity into two lines; the
// later thought stays with the later calls.
func TestNoticeKeepsLaterCallsWithLaterThought(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	u := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	a, b := readCall("a"), readCall("b")
	u.AppendToolStart([]messages.ChatMessageToolCall{a})
	u.AppendWarning("A notice during the first batch")
	u.AppendToolEnd(a, "ok", time.Millisecond, nil)
	u.ShowThinking("Now check the next file")
	u.AppendToolStart([]messages.ChatMessageToolCall{b})
	u.AppendToolEnd(b, "ok", time.Millisecond, nil)
	blocks := activityBlocks(m, 80)
	if len(blocks) != 2 || len(blocks[0].chunks) != 1 || len(blocks[1].chunks) != 1 {
		t.Fatalf("a notice wants two lines of one chunk each: %d lines", len(blocks))
	}
	first, second := blocks[0].chunks[0], blocks[1].chunks[0]
	if first.label != "read a.go" || len(first.parts) != 1 || second.label != "read b.go" || len(second.parts) != 2 || second.parts[0].thought == nil {
		t.Fatalf("chunks span a notice out of order: %q (%d parts), %q (%d parts)", first.label, len(first.parts), second.label, len(second.parts))
	}
}

// Recognize literal test invocations, not an occurrence of a test command
// in a pipeline, a script, an argument or a quoted string.
func TestDirectTestCommands(t *testing.T) {
	for _, cmd := range []string{"go test", "go test -race -count=1 ./...", "npm test", "cargo test", "pytest tests/test_file.py"} {
		if !isTestCommand(cmd) {
			t.Fatalf("missed direct test %q", cmd)
		}
	}
	for _, cmd := range []string{"echo go test", "go test ./... | cat", "go test ./... || true", "go test -c", "go test -list .", "go test ./...\necho done", "npm test -- --version", "pytest $(test-command)", "cd app && go test", "go test; false"} {
		if isTestCommand(cmd) {
			t.Fatalf("classified opaque command %q as tests", cmd)
		}
	}
}

// A saved call with no recorded outcome, or with no result at all, is
// neither running nor passed: its chunk counts calls and claims nothing.
func TestSavedCallsWithoutOutcomesClaimNothing(t *testing.T) {
	h := newReplModel()
	result := func(c messages.ChatMessageToolCall) messages.ChatMessage {
		msg := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: c.ID, ToolName: c.Name, Content: "ok"}
		msg.SetToolSucceeded(true)
		return msg
	}
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "look"}}
	for _, c := range []messages.ChatMessageToolCall{
		readCall("a"), readCall("b"), bashCall("shell", "go test || echo SECRET"),
		bashCall("test", "go test"),
		{ID: "unknown", Name: "custom", Arguments: `{"text":"SECRET"}`},
	} {
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{c}}, result(c))
	}
	// No result or success marker for this one.
	history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{bashCall("legacy", "go test ./...")}})
	h.hydrateHistory(history, "ctx")
	got := onlyChunks(t, h)
	for _, c := range got {
		if c.live || c.failed {
			t.Fatalf("saved chunk %q: live=%v failed=%v", c.label, c.live, c.failed)
		}
	}
	// A command that is not a direct test run is a command, and a header
	// that counts it shows none of it. A run of one call reads as that
	// call's own row does. The call without an outcome is counted and not
	// called passed.
	if want := []string{"read 2 files and ran a command", "tests passed", `used custom text="SECRET"`, "1 test run"}; !slices.Equal(chunkHeaders(got), want) {
		t.Fatalf("headers %q, want %q", chunkHeaders(got), want)
	}
	if !slices.Equal(chunkCalls(got[0]), []string{"tool:1:a", "tool:2:b", "tool:3:shell"}) {
		t.Fatalf("first run lists %q", chunkCalls(got[0]))
	}
}

// A turn whose last batch was denied is saved without that batch's
// messages; the reloaded line still shows it, after the thought that led it.
func TestStrippedDeniedBatchReloadsAfterItsThought(t *testing.T) {
	read := readCall("read")
	edit := messages.ChatMessageToolCall{ID: "edit", Name: "edit_file", Arguments: `{"path":"do-not-save"}`}
	ok := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: read.ID, ToolName: read.Name, Content: "ok"}
	ok.SetToolSucceeded(true)
	generated := []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, Reasoning: "initial", ToolCalls: []messages.ChatMessageToolCall{read}}, ok,
		{Role: messages.MessageRoleAssistant, Reasoning: "again", ToolCalls: []messages.ChatMessageToolCall{edit}},
		{Role: messages.MessageRoleTool, ToolCallID: edit.ID, ToolName: edit.Name, Content: llm.ToolDeniedContent},
	}
	m := newReplModel()
	m.hydrateHistory(append([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "fix"}}, durableTurnMessages(generated)...), "ctx")
	got := onlyChunks(t, m)
	// The denied call's arguments were not saved, so its header names no file.
	if want := []string{"read read.go", "edit a file · denied"}; !slices.Equal(chunkHeaders(got), want) {
		t.Fatalf("headers %q, want %q", chunkHeaders(got), want)
	}
	if got[0].failed || !got[1].failed || !slices.Equal(chunkCalls(got[1]), []string{"tool:2:edit"}) {
		t.Fatalf("stripped terminal batch: %q lists %q", chunkHeaders(got), chunkCalls(got[1]))
	}
	for i, c := range got {
		if c.parts[0].thought == nil || len(c.parts) != 2 {
			t.Fatalf("chunk %d wants its step's thought above its call: %d parts", i, len(c.parts))
		}
	}
}

// The worked example: six reads in three batches with a thought between two
// of them, two edits and a failing test fold to three rows. Open, the reads
// are six rows with the thought above the batch its response asked for. A
// read after the failed test is a new run, and the test's row still says it
// failed. A reloaded session folds the same way.
func TestWorkedExampleFoldsToThreeRows(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("simplify the fold")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "simplify the fold"}}
	step := func(n int, reasoning string, fail bool, calls ...messages.ChatMessageToolCall) {
		tui.ModelAttemptStarted(n, 0)
		if reasoning != "" {
			tui.ShowThinking(reasoning)
		}
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Reasoning: reasoning, ToolCalls: calls})
		tui.AppendToolStart(calls)
		// Parallel calls finish in the reverse of the order asked.
		for _, c := range slices.Backward(calls) {
			var err error
			if fail {
				err = errors.New("exit 1")
			}
			tui.AppendToolEnd(c, "ok", time.Millisecond, err)
			result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: c.ID, ToolName: c.Name, Content: "ok"}
			result.SetToolSucceeded(err == nil)
			history = append(history, result)
		}
	}
	step(0, "", false, readCall("a"), readCall("b"))
	step(1, "", false, readCall("c"), readCall("d"))
	step(2, "the callers matter too", false, readCall("e"), readCall("f"))
	step(3, "", false, editCall("x"), editCall("y"))
	step(4, "", true, bashCall("t", "go test ./..."))
	folded := "▸ Read 6 files\n▸ Edited 2 files\n▸ Tests failed"
	if got := shown(m, width); got != "▎ simplify the fold\n"+folded {
		t.Fatalf("the running turn folds to %q", got)
	}

	// Work goes on after the failure: the test's row keeps saying so.
	tui.ModelAttemptStarted(5, 0)
	history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{readCall("next")}})
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("next")})
	if got := shown(m, width); got != "▎ simplify the fold\n"+folded+"\n▸ Reading next.go" {
		t.Fatalf("a read after the failed test: %q", got)
	}
	tui.AppendToolEnd(readCall("next"), "ok", time.Millisecond, nil)
	result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "next", ToolName: "read_file", Content: "ok"}
	result.SetToolSucceeded(true)
	history = append(history, result, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "Done."})
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	folded += "\n▸ Read next.go"

	reloaded := newReplModel()
	reloaded.hydrateHistory(history, "ctx")
	for name, model := range map[string]*replModel{"live": m, "reloaded": reloaded} {
		rows := strings.Split(shown(model, width), "\n")
		if got := strings.Join(rows[1:5], "\n"); got != folded || rows[5] != "Done." {
			t.Fatalf("%s: settled turn folds to %q", name, rows)
		}
		chunks := onlyChunks(t, model)
		if len(chunks) != 4 || chunks[0].live || !chunks[2].failed || chunks[3].failed {
			t.Fatalf("%s: chunks %q", name, chunkHeaders(chunks))
		}
		// Open the reads alone: six calls in the order asked, the thought
		// above the batch it led, and the other runs still folded.
		model.setChunkOpen(chunks[0].key, nil, nil, width, true)
		rows = strings.Split(shown(model, width), "\n")
		want := []string{"✓ read a.go", "✓ read b.go", "✓ read c.go", "✓ read d.go", thoughtGlyph + " thought", "✓ read e.go", "✓ read f.go"}
		if len(rows) != 6+len(want) || rows[1] != "▾ Read 6 files" || rows[2+len(want)] != "▸ Edited 2 files" {
			t.Fatalf("%s: open reads: %q", name, rows)
		}
		for i, prefix := range want {
			if !strings.HasPrefix(rows[2+i], "  "+prefix) {
				t.Fatalf("%s: row %d under the reads = %q, want %q", name, i, rows[2+i], prefix)
			}
		}
		if strings.Contains(strings.Join(rows, "\n"), "the callers matter too") {
			t.Fatalf("%s: opening a run showed its thought's text", name)
		}
	}
}

// The keyboard reaches what the pointer does: each chunk header, each
// thought row and each call row is a stop of its own, and Enter acts on the
// one selected.
func TestKeyboardReachesEveryHeaderThoughtAndCall(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	screen.SetSize(100, 30)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	for step, id := range []string{"a", "b"} {
		tui.ModelAttemptStarted(step, 0)
		tui.ShowThinking("reasoning about " + id)
		tui.AppendToolStart([]messages.ChatMessageToolCall{readCall(id)})
		tui.AppendToolEnd(readCall(id), "ok", time.Millisecond, nil)
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	m.ed.setText("keep draft")
	record := m.reasoningRecords.get(m.reasoningOrder[0])

	// Folded, the header is the only stop.
	r.render()
	if actions := r.inlineKeyboardActions(); len(actions) != 1 || !strings.HasPrefix(actions[0].key, "chunk:") {
		t.Fatalf("a folded chunk's stops: %+v", actions)
	}
	chooseInlineTestAction(t, r, r.inlineKeyboardActions()[0].key)
	inlineTestKey(r, "<Enter>")
	if _, open := foldStates(m, 100); !slices.Equal(open, []bool{true}) {
		t.Fatalf("Enter on the header did not open its chunk: %v", open)
	}

	// Open: the header, two thought rows and two call rows, each its own.
	stops := map[string]int{}
	for _, action := range r.inlineKeyboardActions() {
		switch {
		case strings.HasPrefix(action.key, "chunk:"):
			stops["header"]++
		case strings.HasSuffix(action.key, ":"+thoughtPartAction):
			stops["thought"]++
		default:
			stops["call"]++
		}
	}
	if len(r.inlineKeyboardActions()) != 5 || stops["header"] != 1 || stops["thought"] != 2 || stops["call"] != 2 {
		t.Fatalf("an open chunk's stops: %v in %+v", stops, r.inlineKeyboardActions())
	}
	second := toolOutputLink{recordID: record.id, key: m.thoughtPartKey(record, 1), action: thoughtPartAction}.keyboardKey()
	chooseInlineTestAction(t, r, second)
	inlineTestKey(r, "<Enter>")
	if record.partOpen(0) || !record.partOpen(1) || m.ed.text() != "keep draft" {
		t.Fatalf("Enter on the second thought: parts open %v/%v, draft %q", record.partOpen(0), record.partOpen(1), m.ed.text())
	}
	if text := shown(m, 100); !strings.Contains(text, "reasoning about b") || strings.Contains(text, "reasoning about a") {
		t.Fatalf("the selected thought alone wants its text: %q", text)
	}
}

// A cached layout block reaches no record. The inspector sizes a view by
// walking it and counts a record once for every pointer that leads to it, so
// a block that kept its chunks' parts would cost a walk of the whole record
// per call and count a thousand-call run a thousand times over.
func TestCachedLayoutReachesNoRecord(t *testing.T) {
	records := map[reflect.Type]bool{
		reflect.TypeFor[toolDisclosureRecord](): true,
		reflect.TypeFor[reasoningRecord]():      true,
	}
	seen := map[reflect.Type]bool{}
	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		if records[typ] {
			t.Fatalf("a cached layout block reaches %s through %s", typ, path)
		}
		if seen[typ] {
			return
		}
		seen[typ] = true
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(typ.Elem(), path)
		case reflect.Map:
			walk(typ.Key(), path)
			walk(typ.Elem(), path)
		case reflect.Struct:
			for i := range typ.NumField() {
				walk(typ.Field(i).Type, path+"."+typ.Field(i).Name)
			}
		}
	}
	walk(reflect.TypeFor[transcriptVisualBlock](), "transcriptVisualBlock")

	// And in practice: laying a thousand-call run out, open, adds the rows
	// it painted to the view's measured size and little else.
	var calls []messages.ChatMessageToolCall
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "read them"}}
	for i := range 1000 {
		calls = append(calls, messages.ChatMessageToolCall{ID: fmt.Sprintf("c%04d", i), Name: "lookup"})
	}
	history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: calls})
	for _, c := range calls {
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: c.ID, ToolName: c.Name, Content: "ok"})
	}
	m := newReplModel()
	m.hydrateHistory(history, "ctx")
	m.depth = depthExpanded
	before := childViewSize(m)
	rows := m.transcriptRows(100)
	if len(rows) < 1000 {
		t.Fatalf("fixture laid out %d rows", len(rows))
	}
	cells := int64(0)
	for _, row := range rows {
		cells += int64(cap(row))
	}
	painted := cells * int64(reflect.TypeFor[ui.Cell]().Size())
	if grew := childViewSize(m) - before; grew > 4*painted+before {
		t.Fatalf("laying out 1000 calls grew the measured view by %d bytes; it painted %d bytes of cells over a %d byte model", grew, painted, before)
	}
}
