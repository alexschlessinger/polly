package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	ui "github.com/metaspartan/gotui/v5"
)

func TestCopyAsyncCommandCallback(t *testing.T) {
	starts, syncCalls := 0, 0
	ctx := &replCommandContext{
		copyResponseAsync: func() error { starts++; return nil },
		copyResponse:      func() error { syncCalls++; return nil },
	}
	if replies := dispatchDefaultCommandForTest(t, "/copy", ctx); len(replies) != 0 || starts != 1 || syncCalls != 0 {
		t.Fatalf("async start replied before writing: %v, starts=%d synchronous=%d", replies, starts, syncCalls)
	}
	if replies := dispatchDefaultCommandForTest(t, "/copy extra", ctx); strings.Join(replies, "\n") != "usage: /copy" || starts != 1 {
		t.Fatalf("usage started copy: %v, starts=%d", replies, starts)
	}
	ctx.copyResponseAsync = func() error { return errors.New("workspace is closing") }
	if replies := dispatchDefaultCommandForTest(t, "/copy", ctx); strings.Join(replies, "\n") != "copy failed: workspace is closing" {
		t.Fatalf("async start failure = %v", replies)
	}
	ctx.copyResponseAsync = nil
	if replies := dispatchDefaultCommandForTest(t, "/copy", ctx); strings.Join(replies, "\n") != "copied last response to clipboard" || syncCalls != 1 {
		t.Fatalf("synchronous callback semantics = %v, calls=%d", replies, syncCalls)
	}
}

func TestCopyAsyncOwnsCapturedTargetAndUI(t *testing.T) {
	for _, scenario := range []string{"switch tab", "replace state", "replace model", "change identity", "close tab", "shutdown", "read error"} {
		t.Run(scenario, func(t *testing.T) {
			screen := installCopyTrackingScreen(t)
			store := testOpenMemoryStore(t, nil)
			session := testAcquireSession(t, store, "origin")
			testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "origin answer"})
			entered, release := make(chan sessions.ViewTarget, 1), make(chan struct{})
			releaseRead := sync.OnceFunc(func() { close(release) })
			reader := &copyPagingStore{SessionStore: store}
			reader.header = func(ctx context.Context, target sessions.ViewTarget, revision string) (*sessions.SessionViewHeader, error) {
				entered <- target
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				if scenario == "read error" {
					return nil, errors.New("storage offline")
				}
				return store.(sessions.ViewHeaderStore).ReadViewHeader(ctx, target, revision)
			}
			r := newManagedREPL(&Config{}, "origin", 0, 0)
			t.Cleanup(func() { releaseRead(); _ = r.work.close() })
			origin := r.visibleTab()
			r.state = &conversationState{session: session, sessionStore: reader}
			origin.state = r.state
			originalModel := origin.model
			other := &replTab{name: "other", state: &conversationState{}, model: newReplModel()}
			r.tabs = append(r.tabs, other)

			// runCommand holds the model lock. A synchronous header read would
			// deadlock here instead of returning with the read still blocked.
			r.runCommand("/copy")
			select {
			case target := <-entered:
				if target != (sessions.ViewTarget{ID: session.(sessions.ViewIdentity).ViewID()}) {
					t.Fatalf("captured target = %+v", target)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("copy did not start off-screen")
			}
			if screen.calls != 0 || strings.Contains(plainStyledText(originalModel.fullTranscript()), "copied") {
				t.Fatal("copy wrote/confirmed while read was blocked")
			}
			switch scenario {
			case "replace state":
				origin.state = other.state
			case "replace model":
				origin.model = newReplModel()
			case "change identity":
				origin.viewTarget = sessions.ViewTarget{ID: "different"}
			case "close tab":
				r.tabs = []*replTab{other}
			case "shutdown":
				if err := r.work.close(); err != nil {
					t.Fatal(err)
				}
			}
			// UI state is mutable after dispatch. The worker must use the
			// captured store, session, and target, not r.state/r.model/tab.
			origin.state.sessionStore = nil
			r.state, r.model = other.state, other.model
			releaseRead()
			r.work.wg.Wait()
			if screen.calls != 0 || strings.Contains(plainStyledText(originalModel.fullTranscript()), "copied") {
				t.Fatal("worker wrote clipboard/model before its UI task")
			}
			screen.onWrite = func() {
				if strings.Contains(plainStyledText(originalModel.fullTranscript()), "copied") {
					t.Fatal("success was reported before clipboard write")
				}
			}
			drainUITasks(r)
			originText := plainStyledText(originalModel.fullTranscript())
			if scenario == "switch tab" {
				if screen.calls != 1 || screen.text != "origin answer" || !strings.Contains(originText, "copied last response") {
					t.Fatalf("copy lost origin ownership: clipboard=%q calls=%d transcript=%q", screen.text, screen.calls, originText)
				}
			} else if screen.calls != 0 || strings.Contains(originText, "copied last response") {
				t.Fatalf("retired/failed request copied: calls=%d transcript=%q", screen.calls, originText)
			}
			if scenario == "read error" && !strings.Contains(originText, "copy failed: read conversation: storage offline") {
				t.Fatalf("missing read failure on originating tab: %q", originText)
			}
			if got := plainStyledText(other.model.fullTranscript()); strings.Contains(got, "copied") || strings.Contains(got, "copy failed") {
				t.Fatalf("copy notice leaked to current tab: %q", got)
			}
		})
	}
}

func TestCopyAsyncDisplayTabUsesStableIDWithoutLease(t *testing.T) {
	screen := installCopyTrackingScreen(t)
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "display")
	testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "display answer"})
	view, err := store.(sessions.ViewStore).ReadView(t.Context(), sessions.ViewTarget{Name: "display"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Rename(t.Context(), "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reused := testAcquireSession(t, store, "display")
	testAddMessage(t, reused, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "wrong answer"})
	reader := &copyPagingStore{SessionStore: store}
	r := newManagedREPL(&Config{}, "display", 0, 0)
	t.Cleanup(func() { _ = r.work.close() })
	tab := r.visibleTab()
	tab.state = &conversationState{sessionStore: reader}
	tab.childView = view
	// Even a legacy display target by name must prefer the known view ID.
	tab.viewTarget = sessions.ViewTarget{Name: "display"}
	r.state = tab.state
	r.runCommand("/copy")
	settleCopyWork(r)
	if screen.text != "display answer" || screen.calls != 1 || tab.state.session != nil || tab.viewOpening || reader.acquires != 0 || reader.views != 0 || reader.targets[0] != (sessions.ViewTarget{ID: view.ID}) {
		t.Fatalf("display copy = %q (%d calls), reader=%+v", screen.text, screen.calls, reader)
	}
}

func TestCopyAsyncRejectsMissingScreenAndClosedWork(t *testing.T) {
	screen := installCopyTrackingScreen(t)
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "closed")
	r := newManagedREPL(&Config{}, "closed", 0, 0)
	r.state = &conversationState{sessionStore: store, session: session}
	r.visibleTab().state = r.state
	ui.DefaultBackend.Screen = nil
	r.runCommand("/copy")
	if !strings.Contains(plainStyledText(r.model.fullTranscript()), "copy failed: no terminal screen") {
		t.Fatal("missing screen should report failure")
	}
	ui.DefaultBackend.Screen = screen
	if err := r.work.close(); err != nil {
		t.Fatal(err)
	}
	r.runCommand("/copy")
	if screen.calls != 0 || !strings.Contains(plainStyledText(r.model.fullTranscript()), "copy failed: workspace is closing") {
		t.Fatal("closed work should not copy")
	}
}
