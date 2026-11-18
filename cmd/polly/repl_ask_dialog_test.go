package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
)

func askTestRequest() askRequest {
	return askRequest{
		Question: "Target PostgreSQL 16, which prod already runs.\n\n**One week** of porting.",
		Header:   "Database migration",
		Options: []askOption{
			{Label: "PostgreSQL", Description: "the existing prod instance, no schema port"},
			{Label: "SQLite", Description: "keep local development only"},
		},
		Other: true,
	}
}

// openAskDialog installs a question through the real bridge and returns the
// dialog, with AskUser's answer on the returned channel.
func openAskDialog(t *testing.T, r *managedREPL, tui *gotuiTurnUI, ctx context.Context, req askRequest) (*askDialog, <-chan askAnswer) {
	t.Helper()
	answers := make(chan askAnswer, 1)
	go func() { answers <- tui.AskUser(ctx, req) }()
	runUITask(t, r)
	if r.model.modal == nil || r.model.modal.ask == nil {
		t.Fatal("no question dialog was installed")
	}
	return r.model.modal.ask, answers
}

func askKey(r *managedREPL, id string) {
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id})
}

func TestAskDialogShowsTheQuestionAndAnswers(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tui := &gotuiTurnUI{repl: r, model: r.model}
	d, answers := openAskDialog(t, r, tui, context.Background(), askTestRequest())
	paint := func() string { return plainStyledText(r.model.modal.text(30, 100)) }
	got := paint()
	if !strings.Contains(got, "Question  Database migration") {
		t.Fatalf("title row = %q", got)
	}
	for _, want := range []string{
		"Target PostgreSQL 16, which prod already runs.",
		"One week of porting.",
		"1 PostgreSQL",
		"2 SQLite",
		"3 Other…",
		"the existing prod instance, no schema port",
		"Enter choose",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("dialog = %q, missing %q", got, want)
		}
	}
	if rows := strings.Count(got, "\n") + 1; rows > 30 {
		t.Fatalf("the dialog took %d rows for a 30-row budget", rows)
	}
	// The description pane follows the selection, and the pane keeps its
	// height so the rows stay put.
	askKey(r, "<Down>")
	if got := paint(); !strings.Contains(got, "keep local development only") {
		t.Fatalf("after Down = %q", got)
	}
	askKey(r, "<Enter>")
	select {
	case answer := <-answers:
		if answer.Outcome != askOutcomeAnswered || answer.Index != 1 || answer.Choice != "SQLite" {
			t.Fatalf("answer = %+v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("Enter did not answer")
	}
	if r.model.modal != nil {
		t.Fatal("the dialog stayed open after it was answered")
	}
	if !d.answered {
		t.Fatal("the dialog did not record its answer")
	}
}

func TestAskDialogNumberKeysPickDirectly(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tui := &gotuiTurnUI{repl: r, model: r.model}
	_, answers := openAskDialog(t, r, tui, context.Background(), askTestRequest())
	r.model.modal.text(30, 100)
	askKey(r, "2")
	select {
	case answer := <-answers:
		if answer.Choice != "SQLite" {
			t.Fatalf("answer = %+v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("2 did not answer")
	}
}

func TestAskDialogIgnoresKeysUntilItIsPainted(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tui := &gotuiTurnUI{repl: r, model: r.model}
	_, answers := openAskDialog(t, r, tui, context.Background(), askTestRequest())
	askKey(r, "<Enter>")
	select {
	case answer := <-answers:
		t.Fatalf("a key answered before the question was painted: %+v", answer)
	default:
	}
	r.model.modal.text(30, 100)
	askKey(r, "<Enter>")
	select {
	case answer := <-answers:
		if answer.Outcome != askOutcomeAnswered {
			t.Fatalf("answer = %+v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("Enter after the first paint did not answer")
	}
}

func TestAskDialogIgnoresPastedText(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tui := &gotuiTurnUI{repl: r, model: r.model}
	_, answers := openAskDialog(t, r, tui, context.Background(), askTestRequest())
	r.model.modal.text(30, 100)
	askKey(r, pasteStartID)
	askKey(r, "<Enter>")
	askKey(r, "2")
	askKey(r, pasteEndID)
	select {
	case answer := <-answers:
		t.Fatalf("a paste answered the question: %+v", answer)
	default:
	}
	askKey(r, "<Enter>")
	select {
	case answer := <-answers:
		if answer.Outcome != askOutcomeAnswered {
			t.Fatalf("answer = %+v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("Enter after the paste did not answer")
	}
}

func TestAskDialogEscapeDismisses(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tui := &gotuiTurnUI{repl: r, model: r.model}
	_, answers := openAskDialog(t, r, tui, context.Background(), askTestRequest())
	r.model.modal.text(30, 100)
	askKey(r, "<Escape>")
	select {
	case answer := <-answers:
		if answer.Outcome != askOutcomeDismissed {
			t.Fatalf("answer = %+v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("Escape did not answer")
	}
	if r.model.modal != nil {
		t.Fatal("Escape left the dialog open")
	}
}

func TestAskDialogTakesAFreeTextAnswer(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tui := &gotuiTurnUI{repl: r, model: r.model}
	d, answers := openAskDialog(t, r, tui, context.Background(), askTestRequest())
	paint := func() string { return plainStyledText(r.model.modal.text(30, 100)) }
	paint()
	askKey(r, "3")
	if !d.other {
		t.Fatal("the Other row did not open its editor")
	}
	if got := paint(); !strings.Contains(got, "type an answer") || !strings.Contains(got, "Enter answer") {
		t.Fatalf("editor = %q", got)
	}
	// Empty text answers nothing, and Escape returns to the list.
	askKey(r, "<Enter>")
	select {
	case answer := <-answers:
		t.Fatalf("an empty answer was submitted: %+v", answer)
	default:
	}
	askKey(r, "<Escape>")
	if d.other {
		t.Fatal("Escape did not return to the list")
	}
	askKey(r, "3")
	for _, ch := range "use prod" {
		askKey(r, string(ch))
	}
	if got := paint(); !strings.Contains(got, "use prod") {
		t.Fatalf("typed answer = %q", got)
	}
	askKey(r, "<Enter>")
	select {
	case answer := <-answers:
		if answer.Outcome != askOutcomeAnswered || answer.Choice != askOtherLabel || answer.Text != "use prod" {
			t.Fatalf("answer = %+v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("Enter did not submit the typed answer")
	}
}

func TestAskDialogScrollsALongQuestion(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tui := &gotuiTurnUI{repl: r, model: r.model}
	req := askTestRequest()
	req.Question = strings.Repeat("A line of the question.\n\n", 30)
	d, answers := openAskDialog(t, r, tui, context.Background(), req)
	got := plainStyledText(r.model.modal.text(24, 100))
	if !strings.Contains(got, "PgUp/PgDn body") {
		t.Fatalf("a scrolling body offers no keys: %q", got)
	}
	before := d.bodyTop
	askKey(r, "<PageDown>")
	r.model.modal.text(24, 100)
	if d.bodyTop <= before {
		t.Fatalf("PageDown did not scroll the body: %d -> %d", before, d.bodyTop)
	}
	askKey(r, "<PageUp>")
	r.model.modal.text(24, 100)
	if d.bodyTop != before {
		t.Fatalf("PageUp did not scroll back: %d", d.bodyTop)
	}
	// The list keeps working while the body scrolls.
	askKey(r, "<Down>")
	askKey(r, "<Enter>")
	select {
	case answer := <-answers:
		if answer.Index != 1 {
			t.Fatalf("answer = %+v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("the list stopped answering while the body scrolled")
	}
}

func TestAskDialogStaysPendingBehindAnotherModal(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tui := &gotuiTurnUI{repl: r, model: r.model}
	d, answers := openAskDialog(t, r, tui, context.Background(), askTestRequest())
	if !r.model.modal.awaited() {
		t.Fatal("a live question is not reported as awaited")
	}
	r.model.modal.text(30, 100)
	r.openModal(&replModal{title: "Help", helpLines: []string{"Navigate"}})
	if r.model.pendingModal == nil || r.model.pendingModal.ask != d {
		t.Fatal("opening another dialog did not park the question")
	}
	r.closeModal()
	if r.model.modal == nil || r.model.modal.ask != d {
		t.Fatal("closing the other dialog did not restore the question")
	}
	askKey(r, "<Escape>")
	select {
	case answer := <-answers:
		if answer.Outcome != askOutcomeDismissed {
			t.Fatalf("answer = %+v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("the restored question did not answer")
	}
}

func TestAskUserRefusesASecondQuestionOnTheTab(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tui := &gotuiTurnUI{repl: r, model: r.model}
	_, first := openAskDialog(t, r, tui, context.Background(), askTestRequest())
	r.model.modal.text(30, 100)
	second := make(chan askAnswer, 1)
	go func() { second <- tui.AskUser(context.Background(), askTestRequest()) }()
	runUITask(t, r)
	select {
	case answer := <-second:
		if answer.Outcome != askOutcomeBusy {
			t.Fatalf("second answer = %+v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("the second question was not refused")
	}
	askKey(r, "<Escape>")
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("the first question did not answer")
	}
}

func TestAskUserWithdrawsWhenTheTurnEnds(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tui := &gotuiTurnUI{repl: r, model: r.model}
	ctx, cancel := context.WithCancel(context.Background())
	_, answers := openAskDialog(t, r, tui, ctx, askTestRequest())
	cancel()
	select {
	case answer := <-answers:
		if answer.Outcome != askOutcomeDismissed {
			t.Fatalf("answer = %+v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not end the question")
	}
	runUITask(t, r)
	if r.model.modal != nil {
		t.Fatal("the withdrawn question stayed on screen")
	}
}

func TestAskUserLabelsAMemberQuestion(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.hidden = true
	tui := &gotuiTurnUI{repl: r, model: r.model}
	req := askTestRequest()
	req.Requester = "scout"
	_, answers := openAskDialog(t, r, tui, context.Background(), req)
	if title := plainStyledText(r.model.modal.text(30, 100)); !strings.Contains(title, "agent scout asks") {
		t.Fatalf("title row = %q", title)
	}
	r.model.notificationMu.Lock()
	signals := append([]tabSignal(nil), r.model.signals...)
	r.model.notificationMu.Unlock()
	if len(signals) != 1 || signals[0].kind != signalApprovalNeeded || signals[0].detail != "question from scout" {
		t.Fatalf("signals = %+v", signals)
	}
	askKey(r, "<Escape>")
	select {
	case <-answers:
	case <-time.After(time.Second):
		t.Fatal("the member's question did not answer")
	}
}

func TestAskDialogFitsANarrowWidth(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tui := &gotuiTurnUI{repl: r, model: r.model}
	req := askTestRequest()
	req.Options[0].Label = strings.Repeat("Long label ", 8)
	req.Options[0].Description = strings.Repeat("long description ", 10)
	_, answers := openAskDialog(t, r, tui, context.Background(), req)
	text := r.model.modal.text(30, 32)
	for _, line := range strings.Split(text, "\n") {
		if width := style.TextWidth(line); width > 32 {
			t.Fatalf("line is %d columns wide in a 32-column panel: %q", width, plainStyledText(line))
		}
	}
	if !strings.Contains(plainStyledText(text), "…") {
		t.Fatal("a long label was not truncated")
	}
	askKey(r, "<Enter>")
	select {
	case <-answers:
	case <-time.After(time.Second):
		t.Fatal("the narrow dialog did not answer")
	}
}

func TestAskDialogDropsTheDetailPaneOnATightBudget(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tui := &gotuiTurnUI{repl: r, model: r.model}
	_, answers := openAskDialog(t, r, tui, context.Background(), askTestRequest())
	got := plainStyledText(r.model.modal.text(6, 100))
	if rows := strings.Count(got, "\n") + 1; rows > 6 {
		t.Fatalf("the dialog took %d rows for a 6-row budget: %q", rows, got)
	}
	if !strings.Contains(got, "1 PostgreSQL") {
		t.Fatalf("a tight budget dropped the choices: %q", got)
	}
	askKey(r, "<Enter>")
	select {
	case <-answers:
	case <-time.After(time.Second):
		t.Fatal("the tight dialog did not answer")
	}
}
