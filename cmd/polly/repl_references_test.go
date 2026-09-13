package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/alexschlessinger/pollytool/llm"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
	ui "github.com/metaspartan/gotui/v5"
)

func awaitReferencePreparation(t *testing.T, r *managedREPL) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		r.model.mu.Lock()
		pending := r.model.referencePreparing
		r.model.mu.Unlock()
		if !pending {
			return
		}
		select {
		case fn := <-r.uiTasks:
			fn()
		case <-deadline:
			t.Fatal("attachment preparation timed out")
		}
	}
}
func referenceTestREPL(t *testing.T) *managedREPL {
	t.Helper()
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "references")
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	t.Cleanup(func() { registry.Close() })
	r := newManagedREPL(&Config{}, "references", 0, 0)
	r.state = &conversationState{session: session, artifactStore: session.ArtifactStore(), toolRegistry: registry}
	r.model.artifactStore = session.ArtifactStore()
	r.model.imageBaseDir = t.TempDir()
	t.Cleanup(func() { r.work.close() })
	return r
}
func referenceTestCatalog(t *testing.T) *skills.Catalog {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"review", "help"} {
		dir := filepath.Join(root, name)
		os.MkdirAll(dir, 0700)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: Check the work\n---\nGive a careful review.\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := skills.LoadCatalog([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestReadOnlyWorkspaceReferenceInput(t *testing.T) {
	for _, input := range []string{"typed sigil", "pasted token", "pasted file"} {
		t.Run(input, func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			r := newManagedREPL(&Config{}, "references", 0, 0)
			t.Cleanup(func() { r.work.close() })
			tab := r.addReadOnlyWorkspace(&sessions.SessionView{
				ID: "read-only", Metadata: &sessions.Metadata{Name: "read-only"},
			}, store)
			if r.model != tab.model || r.state != tab.state || r.state.session != nil {
				t.Fatal("expected visible, sessionless read-only workspace")
			}
			text := "@"
			switch input {
			case "typed sigil":
				r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "@"})
			case "pasted token":
				text = "hello"
				sendPasted(r, text)
			case "pasted file":
				text = filepath.Join(t.TempDir(), "note.txt")
				if err := os.WriteFile(text, []byte("read-only contents"), 0600); err != nil {
					t.Fatal(err)
				}
				sendPasted(r, text)
			}
			if r.model.ed.text() != text {
				t.Fatalf("draft = %q, want %q", r.model.ed.text(), text)
			}
			if r.model.referenceFilesLoading || r.model.referencePasting || r.model.referencesPopup != nil {
				t.Fatal("read-only tab started file preparation or completion")
			}
		})
	}
}

func TestComposerEmptyReferencesRemainLiteral(t *testing.T) {
	for _, prompt := range []string{"@", "meet me @ noon", "/skill", "mention /skill", "@\"\"", "@\"unterminated"} {
		t.Run(prompt, func(t *testing.T) {
			if refs := scanComposerReferences(prompt); len(refs) != 0 {
				t.Fatalf("empty references = %+v", refs)
			}
			for _, busy := range []bool{false, true} {
				r := referenceTestREPL(t)
				if busy {
					r.model.beginTurn("running")
				}
				r.model.ed.setText(prompt)
				r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
				var msg messages.ChatMessage
				if busy {
					if len(r.model.queue) != 1 || r.model.queue[0].turn == nil {
						t.Fatal("literal prompt was not queued as a turn")
					}
					msg = r.model.queue[0].turn.userMessage
				} else {
					pending, ok := r.takePending()
					if !ok {
						t.Fatal("literal prompt was not submitted")
					}
					msg = pending.userMessage
				}
				if msg.GetContent() != prompt || r.model.ed.text() != "" || r.model.referencePreparing {
					t.Fatalf("literal prompt changed: message=%q draft=%q", msg.GetContent(), r.model.ed.text())
				}
				if _, ok := readComposerMetadata(msg); ok {
					t.Fatal("literal prompt acquired reference metadata")
				}
			}
		})
	}
}

func TestComposerFileInputRequiresRegistry(t *testing.T) {
	r := referenceTestREPL(t)
	r.state.toolRegistry = nil
	r.model.referenceFiles = []string{"cached.txt"}
	r.model.referenceFilesLoaded = true
	r.model.referenceFilesAt = time.Now()
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "@"})
	if r.model.referencesPopup != nil || r.model.referenceFilesLoading {
		t.Fatal("file completion requires a registry even with cached candidates")
	}
	r.model.ed.clear()
	path := filepath.Join(r.model.imageBaseDir, "note.txt")
	if err := os.WriteFile(path, []byte("contents"), 0600); err != nil {
		t.Fatal(err)
	}
	sendPasted(r, path)
	if r.model.ed.text() != path || r.model.referencePasting {
		t.Fatal("paste without a registry must remain literal")
	}
}

func TestComposerIncompleteReferenceCompletion(t *testing.T) {
	for _, tc := range []struct{ input, choice string }{
		{"@", "@note.txt"},
		{`@"no`, "@note.txt"},
		{"/", "/review"},
		{"/skill ", "/skill review"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			r := referenceTestREPL(t)
			r.state.skillCatalog = referenceTestCatalog(t)
			r.model.referenceFiles = []string{"note.txt"}
			r.model.referenceFilesLoaded = true
			r.model.referenceFilesAt = time.Now()
			r.model.ed.setText(tc.input)
			r.refreshReferenceCompletionLocked()
			popup := r.model.referencesPopup
			if popup == nil {
				t.Fatal("unfinished token did not offer completion")
			}
			found := false
			for i, choice := range popup.choices {
				if choice.text == tc.choice {
					popup.selected, found = i, true
					break
				}
			}
			if !found {
				t.Fatalf("completion omitted %q", tc.choice)
			}
			r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
			if r.model.ed.text() != tc.choice+" " {
				t.Fatalf("completion = %q", r.model.ed.text())
			}
			if _, ok := r.takePending(); ok {
				t.Fatal("accepting completion submitted a turn")
			}
		})
	}
}

func TestConversationSessionContext(t *testing.T) {
	var absent *conversationState
	for _, state := range []*conversationState{absent, {}} {
		if ctx := state.sessionContext(); ctx == nil || ctx.Err() != nil {
			t.Fatal("sessionless state must have a usable context")
		}
	}
	r := referenceTestREPL(t)
	if r.state.sessionContext() != r.state.session.Context() {
		t.Fatal("live state must use its session context")
	}
}

func TestComposerReferenceGrammar(t *testing.T) {
	input := "use /review @one.go @\"two words.go\" \\@literal `@code /review`\n```go\n@fence\n```\nmail@example.com src/path (/skill help)"
	refs := scanComposerReferences(input)
	var got []string
	for _, ref := range refs {
		got = append(got, ref.kind+":"+ref.name)
	}
	want := []string{"/:review", "@:one.go", "@:two words.go", "skill:help"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("references = %q", got)
	}
	cat := referenceTestCatalog(t)
	names, err := referenceSkills(scanComposerReferences("/review /review /help /skill help /unknown"), cat)
	if err != nil || !reflect.DeepEqual(names, []string{"review", "help"}) {
		t.Fatalf("skills = %v, %v", names, err)
	}
	if leadingSkillReference("/help", cat) || !leadingSkillReference("/skill help", cat) {
		t.Fatal("command collision routing")
	}
}
func TestComposerTextSnapshotSurvivesQueueAndRestore(t *testing.T) {
	r := referenceTestREPL(t)
	path := filepath.Join(r.model.imageBaseDir, "note.txt")
	os.WriteFile(path, []byte("original contents"), 0600)
	r.model.beginTurn("busy")
	r.model.ed.setText("read @note.txt")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	awaitReferencePreparation(t, r)
	if len(r.model.queue) != 1 {
		t.Fatal("missing queued turn")
	}
	turn := *r.model.queue[0].turn
	os.Remove(path)
	if !strings.Contains(turn.userMessage.GetContent(), "original contents") {
		t.Fatal("missing snapshot")
	}
	data, _ := json.Marshal(turn.userMessage)
	var persisted messages.ChatMessage
	json.Unmarshal(data, &persisted)
	restored, ok := restorableHistoryTurn(persisted, "", false, r.model.artifactStore)
	if !ok {
		t.Fatal("not restorable")
	}
	r.model.ed.clear()
	r.model.restoreTurnDraft(restored, newTurnPersistenceAck(true))
	r.model.ed.setText("summarize @note.txt")
	edited, err := prepareReferenceTurn(context.Background(), r.state, r.model.imageBaseDir, r.model.ed.text(), nil, r.model.referenceSnapshotCopy())
	if err != nil || !strings.Contains(edited.userMessage.GetContent(), "original contents") {
		t.Fatalf("restore: %v", err)
	}
	r.model.ed.setText("summarize")
	r.model.pruneReferenceSnapshots()
	if len(r.model.referenceSnapshots) != 0 {
		t.Fatal("deleted reference kept snapshot")
	}
	if _, err := prepareReferenceTurn(context.Background(), r.state, r.model.imageBaseDir, "@note.txt", nil, r.model.referenceSnapshotCopy()); err == nil {
		t.Fatal("reattach reread should fail after deletion")
	}
}
func TestComposerFileValidation(t *testing.T) {
	r := referenceTestREPL(t)
	for _, tc := range []struct {
		name string
		data []byte
		ok   bool
	}{
		{"empty", nil, true}, {"utf8", []byte("hello 世界"), true}, {"nul", []byte("a\x00b"), false}, {"invalid", []byte{255}, false}, {"big", []byte(strings.Repeat("x", maxContextTextBytes+1)), false}, {"pdf", []byte("%PDF-1.7"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(r.model.imageBaseDir, tc.name)
			os.WriteFile(path, tc.data, 0600)
			_, err := prepareReferenceTurn(context.Background(), r.state, r.model.imageBaseDir, "@"+tc.name, nil, nil)
			if (err == nil) != tc.ok {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if _, err := contextFilePart(context.Background(), r.state.toolRegistry, r.model.imageBaseDir, "@dir"); err == nil {
		t.Fatal("directory accepted")
	}
}
func TestComposerMixedPasteAndStaleResults(t *testing.T) {
	r := referenceTestREPL(t)
	a := filepath.Join(r.model.imageBaseDir, "a.txt")
	b := filepath.Join(r.model.imageBaseDir, "b.png")
	os.WriteFile(a, []byte("text"), 0600)
	writeImageFixture(t, b, 8, 8)
	sendPasted(r, a+" "+b)
	select {
	case fn := <-r.uiTasks:
		fn()
	case <-time.After(5 * time.Second):
		t.Fatal("paste timed out")
	}
	if r.model.ed.text() != fileReference(a)+" "+fileReference(b)+" " {
		t.Fatalf("paste = %q", r.model.ed.text())
	}
	r.model.ed.setText("@" + a)
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	r.model.ed.setText("newer draft")
	awaitReferencePreparation(t, r)
	if r.model.ed.text() != "newer draft" {
		t.Fatal("stale read overwrote draft")
	}
	if _, ok := r.takePending(); ok {
		t.Fatal("stale read submitted turn")
	}
}
func TestComposerCompletionReplacesOnlyReference(t *testing.T) {
	r := referenceTestREPL(t)
	r.model.referenceFilesLoaded = true
	r.model.referenceFiles = []string{"one.go", "other.go"}
	r.model.ed.setText("inspect @on please")
	r.model.ed.cursor = len([]rune("inspect @on"))
	r.refreshReferenceCompletionLocked()
	if r.model.referencesPopup == nil {
		t.Fatal("no popup")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if r.model.ed.text() != "inspect @one.go please" {
		t.Fatalf("completion = %q", r.model.ed.text())
	}
	if _, ok := r.takePending(); ok {
		t.Fatal("selection submitted")
	}
	r.model.ed.setText("@o")
	r.refreshReferenceCompletionLocked()
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Escape>"})
	if r.model.referencesPopup != nil {
		t.Fatal("escape did not dismiss")
	}
	for _, width := range []int{20, 80} {
		r.model.referenceDismissed = ""
		r.refreshReferenceCompletionLocked()
		w := r.model.referencePopupWidget(width, width-1, 4)
		if w == nil || w.Max.X > width || w.Min.Y < 0 {
			t.Fatalf("popup overflow at %d", width)
		}
	}
}
func TestComposerSkillActivatesOnlyAtExecution(t *testing.T) {
	r := referenceTestREPL(t)
	r.state.skillCatalog = referenceTestCatalog(t)
	runtime, err := tools.NewSkillRuntime(r.state.skillCatalog, r.state.toolRegistry)
	if err != nil {
		t.Fatal(err)
	}
	r.state.skillRuntime = runtime
	turn, err := prepareReferenceTurn(context.Background(), r.state, r.model.imageBaseDir, "/review check this", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.ActivatedSkills()) != 0 {
		t.Fatal("activated during preparation")
	}
	msg, err := activateComposerSkills(context.Background(), r.state, turn.userMessage)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.GetContent(), "Give a careful review.") || !reflect.DeepEqual(runtime.ActivatedSkills(), []string{"review"}) {
		t.Fatal("activation missing")
	}
	md, _ := r.state.session.GetMetadata(context.Background())
	if !reflect.DeepEqual(md.ActiveSkills, []string{"review"}) {
		t.Fatal("activation not persisted")
	}
	again, err := activateComposerSkills(context.Background(), r.state, msg.Clone())
	if err != nil || !reflect.DeepEqual(msg, again) {
		t.Fatalf("repeat changed message: %v", err)
	}
}

func TestComposerFileReferenceQuotesRoundTrip(t *testing.T) {
	for _, path := range []string{"plain.go", "two words.go", `a"b.go`, "a'b.go", "a\nb.go"} {
		refs := scanComposerReferences(fileReference(path))
		if len(refs) != 1 || refs[0].name != path {
			t.Fatalf("%q -> %+v", path, refs)
		}
	}
	refs := scanComposerReferences(`@"unterminated`)
	if len(refs) != 0 {
		t.Fatal("unterminated quote accepted")
	}
}
func TestComposerFileLimitsAndDeduplication(t *testing.T) {
	r := referenceTestREPL(t)
	var refs []string
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("file%d", i)
		os.WriteFile(filepath.Join(r.model.imageBaseDir, name), []byte(strings.Repeat("x", maxContextTextBytes)), 0600)
		refs = append(refs, "@"+name)
	}
	turn, err := prepareReferenceTurn(context.Background(), r.state, r.model.imageBaseDir, strings.Join(refs[:4], " "), nil, nil)
	if err != nil {
		t.Fatalf("exact 1 MiB rejected: %v", err)
	}
	if len(turn.userMessage.Parts) != 5 {
		t.Fatal("missing file parts")
	}
	if _, err := prepareReferenceTurn(context.Background(), r.state, r.model.imageBaseDir, strings.Join(refs, " "), nil, nil); err == nil {
		t.Fatal("aggregate limit ignored")
	}
	turn, err = prepareReferenceTurn(context.Background(), r.state, r.model.imageBaseDir, "@file0 @./file0", nil, nil)
	if err != nil || len(turn.userMessage.Parts) != 2 {
		t.Fatalf("duplicate reference = %d %v", len(turn.userMessage.Parts), err)
	}
}
func TestComposerUnsupportedPasteIsAtomic(t *testing.T) {
	r := referenceTestREPL(t)
	a := filepath.Join(r.model.imageBaseDir, "a.txt")
	b := filepath.Join(r.model.imageBaseDir, "b.bin")
	os.WriteFile(a, []byte("hello"), 0600)
	os.WriteFile(b, []byte{0, 255}, 0600)
	_, recognized, err := preparePathPaste(context.Background(), r.state, r.model.imageBaseDir, []string{a, b})
	if !recognized || err == nil {
		t.Fatal("unsupported batch not rejected")
	}
	_, recognized, err = preparePathPaste(context.Background(), r.state, r.model.imageBaseDir, []string{"read", a})
	if recognized || err != nil {
		t.Fatal("prose classified as files")
	}
}
func TestComposerQueuedMixedFilesSurviveReset(t *testing.T) {
	r := referenceTestREPL(t)
	os.WriteFile(filepath.Join(r.model.imageBaseDir, "a.txt"), []byte("before reset"), 0600)
	writeImageFixture(t, filepath.Join(r.model.imageBaseDir, "b.png"), 8, 8)
	turn, err := prepareReferenceTurn(context.Background(), r.state, r.model.imageBaseDir, "@a.txt @b.png", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.model.queue = []queuedREPLInput{{text: turn.displayText, turn: &turn}}
	queue, err := r.model.materializeQueuedImagesForReset(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.model.restoreQueuedImagesAfterReset(context.Background(), queue); err != nil {
		t.Fatal(err)
	}
	got := r.model.queue[0].turn.userMessage
	if !strings.Contains(got.GetContent(), "before reset") || !got.HasImages() {
		t.Fatal("reset lost attachments")
	}
}
func TestComposerPartialSkillFailureKeepsEarlierActivation(t *testing.T) {
	r := referenceTestREPL(t)
	r.state.skillCatalog = referenceTestCatalog(t)
	runtime, err := tools.NewSkillRuntime(r.state.skillCatalog, r.state.toolRegistry)
	if err != nil {
		t.Fatal(err)
	}
	r.state.skillRuntime = runtime
	msg := messages.ChatMessage{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "/review"}}}
	writeComposerMetadata(&msg, composerMetadata{Version: 1, Draft: "/review", Skills: []string{"review", "missing"}})
	_, err = activateComposerSkills(context.Background(), r.state, msg)
	if err == nil || !reflect.DeepEqual(runtime.ActivatedSkills(), []string{"review"}) {
		t.Fatalf("partial activation: %v %v", runtime.ActivatedSkills(), err)
	}
	md, _ := r.state.session.GetMetadata(context.Background())
	if !reflect.DeepEqual(md.ActiveSkills, []string{"review"}) {
		t.Fatal("partial activation not persisted")
	}
}

type referenceCaptureLLM struct{ content string }

func (c *referenceCaptureLLM) ChatCompletionStream(_ context.Context, req *llm.CompletionRequest, processor llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	for _, msg := range req.ResolvedMessages() {
		c.content += msg.GetContent() + "\n"
	}
	input := make(chan messages.ChatMessage, 1)
	input <- messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}
	close(input)
	return processor.ProcessMessagesToEvents(input)
}
func TestComposerProviderReceivesFilesAndActivatedInstructions(t *testing.T) {
	r := referenceTestREPL(t)
	r.state.skillCatalog = referenceTestCatalog(t)
	runtime, err := tools.NewSkillRuntime(r.state.skillCatalog, r.state.toolRegistry)
	if err != nil {
		t.Fatal(err)
	}
	r.state.skillRuntime = runtime
	model := &referenceCaptureLLM{}
	r.state.agent = llm.NewAgent(model, r.state.toolRegistry, llm.AgentConfig{ArtifactStore: r.state.artifactStore})
	r.state.settings = Settings{Model: "test/model", MaxTokens: 128}
	os.WriteFile(filepath.Join(r.model.imageBaseDir, "a.txt"), []byte("unique attached contents"), 0600)
	turn, err := prepareReferenceTurn(context.Background(), r.state, r.model.imageBaseDir, "/review @a.txt", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := activateComposerSkills(context.Background(), r.state, turn.userMessage)
	if err != nil {
		t.Fatal(err)
	}
	turnUI := newLineTurnUI(r.config, nil)
	turnUI.writer = io.Discard
	turnUI.errWriter = io.Discard
	if _, err := executeTurnWithUserMessage(context.Background(), r.config, r.state, msg, nil, nil, turnUI, false); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Attached file", "a.txt", "unique attached contents", "Explicitly requested skill instructions", "Give a careful review."} {
		if !strings.Contains(model.content, text) {
			t.Fatalf("provider missing %q", text)
		}
	}
	history := testSessionHistory(t, r.state.session)
	if len(history) < 1 || !strings.Contains(history[0].GetContent(), "unique attached contents") {
		t.Fatal("submitted context was not persisted")
	}
}
func TestComposerPreparationStaysWithOriginatingTab(t *testing.T) {
	r := referenceTestREPL(t)
	origin := r.model
	os.WriteFile(filepath.Join(origin.imageBaseDir, "a.txt"), []byte("hello"), 0600)
	origin.ed.setText("@a.txt")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	other := newReplModel()
	other.ed.setText("other draft")
	r.model = other
	select {
	case fn := <-r.uiTasks:
		fn()
	case <-time.After(5 * time.Second):
		t.Fatal("preparation timed out")
	}
	pending, ok := r.takePendingTurn()
	if !ok || pending.model != origin || other.ed.text() != "other draft" {
		t.Fatal("preparation crossed tabs")
	}
}
func TestComposerDuplicateAliasRestoresSavedBytes(t *testing.T) {
	r := referenceTestREPL(t)
	path := filepath.Join(r.model.imageBaseDir, "a.txt")
	os.WriteFile(path, []byte("snapshot"), 0600)
	turn, err := prepareReferenceTurn(context.Background(), r.state, r.model.imageBaseDir, "@a.txt @./a.txt", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.model.rememberReferenceSnapshots(turn.userMessage)
	os.Remove(path)
	_, err = prepareReferenceTurn(context.Background(), r.state, r.model.imageBaseDir, "@./a.txt", nil, r.model.referenceSnapshotCopy())
	if err != nil {
		t.Fatalf("alias lost snapshot: %v", err)
	}
}

func TestComposerFailedTurnDoesNotRebindNewerDraft(t *testing.T) {
	r := referenceTestREPL(t)
	path := filepath.Join(r.model.imageBaseDir, "a.txt")
	os.WriteFile(path, []byte("old bytes"), 0600)
	turn, err := prepareReferenceTurn(context.Background(), r.state, r.model.imageBaseDir, "@a.txt", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.model.ed.setText("@a.txt")
	if r.model.restoreTurnDraft(turn, nil) {
		t.Fatal("overwrote newer draft")
	}
	if _, _, reuse := r.model.acceptedRestoredTurn("@a.txt"); reuse {
		t.Fatal("new draft silently reused old attachment")
	}
	os.WriteFile(path, []byte("new bytes"), 0600)
	fresh, err := prepareReferenceTurn(context.Background(), r.state, r.model.imageBaseDir, "@a.txt", nil, r.model.referenceSnapshotCopy())
	if err != nil || !strings.Contains(fresh.userMessage.GetContent(), "new bytes") {
		t.Fatalf("new draft rebound: %v", err)
	}
}
