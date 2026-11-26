package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// hostCommand runs a slash command as the composer would and waits for the
// question it put to the host to be settled.
func hostCommand(t *testing.T, r *managedREPL, line string) {
	t.Helper()
	r.model.mu.Lock()
	r.runCommand(line)
	r.model.mu.Unlock()
	pumpUI(t, r, func() bool { return r.panes.asked == nil })
}

// answering makes the host answer its next questions with the row id.
func answering(fake *fakePanes, id string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.choose = func([]paneChoice) (string, error) { return id, nil }
}

func TestHelpListsTheCommandsInTheHost(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	fake := hostPanes(t, r)

	// A command that needs an argument waits in the composer for it.
	answering(fake, "/rename")
	hostCommand(t, r, "/help")
	if r.model.modal != nil || r.model.ed.text() != "/rename " {
		t.Fatalf("choosing /rename left the composer at %q, modal %v", r.model.ed.text(), r.model.modal != nil)
	}
	list := fake.getLists()[0]
	spawn, ok := choiceTitled(list, "/spawn [--read-only] [--review] <brief>")
	if !ok || spawn.id != "/spawn" || spawn.icon != paneIconCommand || spawn.subtitle == "" {
		t.Fatalf("the host listed /spawn as %+v", spawn)
	}
	if _, hidden := choiceTitled(list, "/model"); hidden || list[len(list)-1].id != pickHelpPage {
		t.Fatalf("the host listed a hidden command, or no way to the full help: %+v", list[len(list)-1])
	}
	if sessions, _ := choiceTitled(list, "/sessions"); sessions.detail != "/resume" {
		t.Fatalf("a command's other name is not on its row: %+v", sessions)
	}

	// A draft is not that command's argument, and stays.
	r.model.ed.setText("half a thought")
	hostCommand(t, r, "/help")
	if r.model.ed.text() != "half a thought" || !strings.Contains(notices(r), "usage: /rename <name>") {
		t.Fatalf("choosing /rename over a draft left %q:\n%s", r.model.ed.text(), notices(r))
	}

	// A command that needs nothing more runs, and the draft is still there.
	answering(fake, "/new")
	hostCommand(t, r, "/help")
	pumpUI(t, r, func() bool { return fake.opens() == 1 })
	if r.model.ed.text() != "half a thought" {
		t.Fatalf("running /new from the list left the draft at %q", r.model.ed.text())
	}

	// The last row leads to polly's help page, and a host that cannot ask
	// leaves the list to polly's own screen.
	answering(fake, pickHelpPage)
	hostCommand(t, r, "/help")
	if m := r.model.modal; m == nil || m.title != "Help" {
		t.Fatal("the full help did not open from the list")
	}
	r.model.mu.Lock()
	r.closeModal()
	r.model.mu.Unlock()
	fake.setChoose(nil)
	hostCommand(t, r, "/help")
	if m := r.model.modal; m == nil || m.title != "Polly commands" || !strings.Contains(listLabels(m), "/new") || r.panes.listless {
		t.Fatal("a busy host did not leave the list to polly's own screen")
	}

	// Help for one command is a page to read, not a list.
	asked := fake.listed()
	r.model.mu.Lock()
	r.closeModal()
	r.runCommand("/help /spawn")
	modal := r.model.modal
	r.model.mu.Unlock()
	if modal == nil || fake.listed() != asked {
		t.Fatal("help for one command went to the host")
	}
}

func TestTitleIsAskedForInTheHost(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	fake := hostPanes(t, r)
	fake.setAnswer(func(context.Context, paneQuestion) (paneAnswer, error) {
		return paneAnswer{given: true, text: "Ranking fixes"}, nil
	})
	hostCommand(t, r, "/title")
	if q := fake.asked(); !q.text || q.title != "Session title" || len(q.choices) != 0 {
		t.Fatalf("the host was asked %+v", q)
	}
	md, err := r.state.session.GetMetadata(context.Background())
	if err != nil || md.Title != "Ranking fixes" || md.TitleSource != sessions.TitleSourceUser || r.model.status.title != "Ranking fixes" {
		t.Fatalf("the title typed in the host was saved as %+v (%v), shown as %q", md, err, r.model.status.title)
	}

	// The next question starts from the title there is, and one closed
	// changes nothing.
	fake.setAnswer(func(context.Context, paneQuestion) (paneAnswer, error) { return paneAnswer{}, nil })
	hostCommand(t, r, "/title")
	if fake.asked().value != "Ranking fixes" || r.model.status.title != "Ranking fixes" || r.model.modal != nil {
		t.Fatalf("a closed question left title %q", r.model.status.title)
	}

	// A host that cannot ask leaves it to polly's own field.
	fake.setAnswer(nil)
	hostCommand(t, r, "/title")
	m := r.model.modal
	if m == nil || !m.inputMode || m.input.text() != "Ranking fixes" {
		t.Fatal("polly's own title field did not open")
	}
	m.input.setText("Typed here")
	r.model.mu.Lock()
	m.onSubmit(m.input.text())
	r.model.mu.Unlock()
	if r.model.status.title != "Typed here" {
		t.Fatalf("the field saved %q", r.model.status.title)
	}
}

func TestModelsAreListedUnderTheNameSetTakes(t *testing.T) {
	text := llm.ModelCapabilities{}
	catalog := llm.ModelCatalog{Models: []llm.ModelInfo{
		{ID: "zeta", Name: "Zeta Large", ModelCapabilities: text},
		{ID: "alpha", Name: "alpha", ModelCapabilities: text},
	}}
	choices := modelChoices("acme", "acme/zeta", catalog)
	if len(choices) != 2 || choices[0].id != "acme/alpha" || choices[0].title != "alpha" || choices[0].subtitle != "" {
		t.Fatalf("models listed as %+v", choices)
	}
	if choices[1].id != "acme/zeta" || choices[1].subtitle != "Zeta Large" || choices[1].detail != "current" {
		t.Fatalf("the session's model is listed as %+v", choices[1])
	}
}

// awaitingApproval gives r's visible tab a batch of calls to approve.
func awaitingApproval(r *managedREPL, commands ...string) *approvalState {
	a := &approvalState{reply: make(chan []bool, 1)}
	for _, command := range commands {
		a.calls = append(a.calls, messages.ChatMessageToolCall{Name: "bash", Arguments: `{"command":"` + command + `"}`})
	}
	m := r.model
	m.mu.Lock()
	defer m.mu.Unlock()
	m.approvalQueue = append(m.approvalQueue, a)
	m.advanceApprovalLocked()
	return a
}

func focus(r *managedREPL, focused bool) {
	r.model.mu.Lock()
	r.model.focusKnown, r.model.focused = true, focused
	r.model.mu.Unlock()
}

func TestAnApprovalNobodyAnswersIsAskedInTheHost(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	fake := hostPanes(t, r)
	a := awaitingApproval(r, "go test ./...", "git push")

	// While the terminal has the keys, polly's own prompt has it first.
	focus(r, true)
	r.syncHostAsks()
	if fake.listed() != 0 || r.panes.waking == nil {
		t.Fatalf("an approval just shown was put to the host (%d questions), or nothing will bring it there", fake.listed())
	}
	// A key pressed since shows the user is here, reading it.
	r.model.mu.Lock()
	a.waiting = time.Now().Add(-hostAskDelay)
	r.model.mu.Unlock()
	r.keyAt = time.Now()
	r.syncHostAsks()
	if fake.listed() != 0 {
		t.Fatal("an approval the user is reading was put to the host")
	}
	// Left alone for long enough, it goes to the host too.
	r.keyAt = time.Time{}
	answering(fake, approveYes)
	r.syncHostAsks()
	pumpUI(t, r, func() bool { return r.panes.asked == nil })
	q := fake.asked()
	if !strings.Contains(q.title, "Allow bash") || !strings.Contains(q.title, "go test") || q.typed || q.text {
		t.Fatalf("the host was asked %+v", q)
	}
	if len(q.choices) != 4 || q.choices[0].id != askLater || q.choices[1].id != approveYes || !strings.Contains(q.choices[1].subtitle, "go test ./...") || q.choices[3].id != approveRest {
		t.Fatalf("the host offered %+v", q.choices)
	}
	if a.index != 1 || !a.out[0] || r.model.approval != a {
		t.Fatalf("Allow in the host left the batch at call %d, %v", a.index, a.out)
	}

	// With the keys elsewhere the next call is asked about at once, and an
	// answer in polly takes the host's question down.
	focus(r, false)
	fake.mu.Lock()
	fake.choose = nil
	fake.answer = func(ctx context.Context, _ paneQuestion) (paneAnswer, error) {
		<-ctx.Done()
		return paneAnswer{given: true, id: approveYes}, nil
	}
	fake.mu.Unlock()
	r.syncHostAsks()
	if !r.hostAsking(approvalAsk{model: r.model, request: a, call: 1}) {
		t.Fatal("the second call was not put to the host")
	}
	// It is not asked twice while it is up.
	r.syncHostAsks()
	for deadline := time.Now().Add(5 * time.Second); fake.listed() != 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the host was asked %d questions, want 2", fake.listed())
		}
	}
	r.model.mu.Lock()
	r.model.handleApprovalAnswer('n')
	r.model.mu.Unlock()
	r.syncHostAsks()
	pumpUI(t, r, func() bool { return r.panes.asked == nil })
	if got := <-a.reply; len(got) != 2 || !got[0] || got[1] || fake.listed() != 2 {
		t.Fatalf("the batch was answered %v over %d questions", got, fake.listed())
	}
}

func TestTheFirstRowOfAnApprovalDecidesNothing(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	fake := hostPanes(t, r)
	a := awaitingApproval(r, "rm -rf build")
	focus(r, false)
	for _, id := range []string{askLater, "", "no such row"} {
		r.model.mu.Lock()
		a.hosted = 0
		r.model.mu.Unlock()
		answering(fake, id)
		r.syncHostAsks()
		pumpUI(t, r, func() bool { return r.panes.asked == nil })
		if r.model.approval != a || a.index != 0 {
			t.Fatalf("answer %q settled the approval", id)
		}
	}
	if len(fake.asked().choices) != 3 {
		t.Fatalf("a single call was offered %+v", fake.asked().choices)
	}
	// Deny is an answer.
	r.model.mu.Lock()
	a.hosted = 0
	r.model.mu.Unlock()
	answering(fake, approveNo)
	r.syncHostAsks()
	pumpUI(t, r, func() bool { return r.panes.asked == nil })
	if got := <-a.reply; len(got) != 1 || got[0] || r.model.approval != nil {
		t.Fatalf("Deny in the host answered %v", got)
	}
}

func TestAQuestionFromTheModelIsAskedInTheHost(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	fake := hostPanes(t, r)
	focus(r, false)
	ask := func(answer paneAnswer) askAnswer {
		t.Helper()
		reply := make(chan askAnswer, 1)
		d := newAskDialog(askRequest{
			Question: "Which database should the cache use?\n\nIt only holds derived rows.",
			Options:  []askOption{{Label: "Postgres", Description: "one more\nservice"}, {Label: "SQLite"}},
			Other:    true,
		}, reply)
		r.model.mu.Lock()
		r.model.modal, d.shown = d.modal, time.Now()
		r.model.mu.Unlock()
		fake.mu.Lock()
		fake.answer = func(context.Context, paneQuestion) (paneAnswer, error) { return answer, nil }
		fake.mu.Unlock()
		r.syncHostAsks()
		pumpUI(t, r, func() bool { return r.panes.asked == nil })
		select {
		case got := <-reply:
			if r.model.modal != nil {
				t.Fatal("an answered question stayed on screen")
			}
			return got
		default:
			if r.model.modal != d.modal {
				t.Fatal("an unanswered question left the screen")
			}
			r.model.mu.Lock()
			r.closeModal()
			r.model.mu.Unlock()
			return <-reply
		}
	}

	if got := ask(paneAnswer{given: true, id: "1"}); got.Outcome != askOutcomeAnswered || got.Index != 1 || got.Choice != "SQLite" {
		t.Fatalf("choosing a row answered %+v", got)
	}
	q := fake.asked()
	if q.title != "Which database should the cache use?" || !q.typed || len(q.choices) != 3 || q.choices[0].id != askLater {
		t.Fatalf("the host was asked %+v", q)
	}
	if q.choices[1].title != "Postgres" || q.choices[1].subtitle != "one more service" {
		t.Fatalf("an option is listed as %+v", q.choices[1])
	}
	if got := ask(paneAnswer{given: true, text: " DuckDB "}); got.Outcome != askOutcomeAnswered || got.Index != 2 || got.Text != "DuckDB" {
		t.Fatalf("typing an answer gave %+v", got)
	}
	// The first row and a closed list leave the question to polly.
	for _, answer := range []paneAnswer{{given: true, id: askLater}, {}} {
		if got := ask(answer); got.Outcome != askOutcomeDismissed {
			t.Fatalf("host answer %+v gave the model %+v", answer, got)
		}
	}
}

// A host that has not been set up to ask is left alone for a while: every
// command would otherwise wait on its answer before asking on polly's own
// screen. It is asked again once the while is over, and is not given up on.
func TestAHostNotSetUpToAskIsLeftAloneForAWhile(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	fake := hostPanes(t, r)
	fake.setAnswer(func(context.Context, paneQuestion) (paneAnswer, error) { return paneAnswer{}, errAskUnconfigured })
	ask := func() (started, asked bool) {
		t.Helper()
		done := false
		started = r.askHost(paneQuestion{title: "Which?"}, "which", func(_ paneAnswer, ok bool) { asked, done = ok, true })
		if started {
			pumpUI(t, r, func() bool { return done })
		}
		return started, asked
	}
	if started, asked := ask(); !started || asked || fake.listed() != 1 {
		t.Fatalf("the first question: started=%v asked=%v of %d", started, asked, fake.listed())
	}
	if r.panes.listless || !time.Now().Before(r.panes.askRetryAt) {
		t.Fatalf("a host not set up was given up on (%v), or will be asked at once", r.panes.listless)
	}
	if started, _ := ask(); started || fake.listed() != 1 {
		t.Fatalf("a host not set up was asked again at once (%d questions)", fake.listed())
	}
	// Meanwhile a command's list opens on polly's own screen without a wait,
	// and an approval is not marked as taken to the host.
	r.model.mu.Lock()
	r.runCommand("/help")
	list := r.model.modal
	r.closeModal()
	r.model.mu.Unlock()
	if list == nil || list.title != "Polly commands" || fake.listed() != 1 {
		t.Fatalf("/help did not list the commands here: %v after %d questions", list, fake.listed())
	}
	a := awaitingApproval(r, "go test ./...")
	focus(r, false)
	r.syncHostAsks()
	if a.hosted != 0 || fake.listed() != 1 {
		t.Fatalf("an approval was marked as asked in a host that was not asked (%d questions)", fake.listed())
	}

	// The while over, the host is asked again, and one set up since answers.
	r.panes.askRetryAt = time.Now().Add(-time.Second)
	fake.mu.Lock()
	fake.answer = nil
	fake.mu.Unlock()
	answering(fake, approveYes)
	r.syncHostAsks()
	pumpUI(t, r, func() bool { return fake.listed() == 2 && r.panes.asked == nil })
	if got := <-a.reply; len(got) != 1 || !got[0] || !r.panes.askRetryAt.IsZero() {
		t.Fatalf("the host's answer was %v, and it is held off until %v", got, r.panes.askRetryAt)
	}
}

// Keys and clicks in one of polly's panes show that the user is here as much
// as keys in polly's own terminal do. The approval beside them is not taken
// to the host's list while they work there, though polly's terminal has lost
// the keys to the pane.
func TestWorkingInAPaneIsBeingHere(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	p, _ := openTestPane(t, r, fake)
	a := awaitingApproval(r, "go test ./...")
	focus(r, false)
	paneKey(r, p, "<Down>")
	r.syncHostAsks()
	if fake.listed() != 0 || r.panes.asked != nil || a.hosted != 0 {
		t.Fatalf("an approval was put to the host while the user worked in a pane (%d questions)", fake.listed())
	}
	// Left alone for long enough, it goes to the host as before.
	r.model.mu.Lock()
	r.paneInputAt = time.Now().Add(-hostAskDelay)
	r.keyAt = r.paneInputAt
	r.model.mu.Unlock()
	answering(fake, approveYes)
	r.syncHostAsks()
	pumpUI(t, r, func() bool { return fake.listed() == 1 && r.panes.asked == nil })
	if got := <-a.reply; len(got) != 1 || !got[0] {
		t.Fatalf("the host's answer was %v", got)
	}
}
