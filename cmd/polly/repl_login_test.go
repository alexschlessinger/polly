package main

import (
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	ui "github.com/metaspartan/gotui/v5"
)

func TestParseLoginArgs(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		provider string
		device   bool
		err      string
	}{
		{nil, "codex", false, ""},
		{[]string{"codex"}, "codex", false, ""},
		{[]string{"--device"}, "codex", true, ""},
		{[]string{"codex", "--device"}, "codex", true, ""},
		{[]string{"device", "codex"}, "codex", true, ""},
		{[]string{"codex", "extra"}, "", false, "usage"},
		{[]string{"openai"}, "", false, "takes an API key"},
	} {
		provider, device, err := parseLoginArgs(tc.args)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("parseLoginArgs(%v) err = %v, want %q", tc.args, err, tc.err)
			}
			continue
		}
		if err != nil || provider != tc.provider || device != tc.device {
			t.Errorf("parseLoginArgs(%v) = %q, %v, %v", tc.args, provider, device, err)
		}
	}
}

func TestLoginCommandsAreListedAndSignOutAsText(t *testing.T) {
	help := strings.Join(defaultReplCommands.helpLines(), "\n")
	if !strings.Contains(help, "/login") || !strings.Contains(help, "/logout") {
		t.Fatalf("help = %q", help)
	}
	flow, signedIn := fakeLoginFlow(nil, nil)
	useFakeLoginFlow(t, flow)
	var out strings.Builder
	ctx := newWriterReplCommandContext(&Config{}, nil, &out)
	if handled, _, err := defaultReplCommands.dispatch("/logout", ctx); err != nil || !handled {
		t.Fatalf("handled = %v, err = %v", handled, err)
	}
	if *signedIn || !strings.Contains(out.String(), "signed out of codex") {
		t.Fatalf("signed in = %v, out = %q", *signedIn, out.String())
	}
	out.Reset()
	defaultReplCommands.dispatch("/logout", ctx)
	if !strings.Contains(out.String(), "not signed in to codex") {
		t.Fatalf("out = %q", out.String())
	}
	out.Reset()
	defaultReplCommands.dispatch("/login openai", ctx)
	if !strings.Contains(out.String(), "takes an API key") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestLoginCommandSignsInAsTextInTheFallbackREPL(t *testing.T) {
	browser := newFakeBrowserLogin("localhost:1455")
	flow, _ := fakeLoginFlow(browser, nil)
	useFakeLoginFlow(t, flow)
	var out strings.Builder
	ctx := newWriterReplCommandContext(&Config{}, nil, &out)
	ctx.readInput = func(string) (string, error) {
		browser.finish(testAccount)
		return "", nil
	}
	if _, _, err := defaultReplCommands.dispatch("/login", ctx); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); !strings.Contains(text, browser.url) || !strings.Contains(text, "signed in to codex as user@example.com (plus plan)") {
		t.Fatalf("out = %q", text)
	}
}

// loginREPL is a managed REPL with the sign-in flow on fakes and a
// recording browser opener.
func loginREPL(t *testing.T, browser *fakeBrowserLogin, device *fakeDeviceLogin) (*managedREPL, *[]string) {
	t.Helper()
	flow, _ := fakeLoginFlow(browser, device)
	useFakeLoginFlow(t, flow)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	t.Cleanup(func() { _ = r.work.close() })
	var opened []string
	r.openURL = func(u string) error { opened = append(opened, u); return nil }
	return r, &opened
}

func loginPaint(r *managedREPL) string {
	return plainStyledText(r.model.modal.text(40, 90))
}

func TestLoginDialogSignsInThroughTheCallback(t *testing.T) {
	browser := newFakeBrowserLogin("localhost:1455")
	r, opened := loginREPL(t, browser, nil)
	r.model.mu.Lock()
	r.openLogin("codex", false)
	r.model.mu.Unlock()
	d := r.model.modal.login
	if d == nil || !d.running || len(*opened) != 1 || (*opened)[0] != browser.url {
		t.Fatalf("dialog = %+v, opened %v", d, *opened)
	}
	if got := loginPaint(r); !strings.Contains(got, browser.url) || !strings.Contains(got, "come back to localhost:1455") || !strings.Contains(got, "› Paste") || !strings.Contains(got, "[ Open browser again ]") {
		t.Fatalf("dialog = %q", got)
	}
	browser.finish(testAccount)
	runUITask(t, r)
	if r.model.modal != nil {
		t.Fatal("dialog still open after the sign-in")
	}
	if text := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(text, "signed in to codex as user@example.com (plus plan)") {
		t.Fatalf("transcript = %q", text)
	}
}

func TestLoginDialogPasteAndButtons(t *testing.T) {
	browser := newFakeBrowserLogin("")
	r, opened := loginREPL(t, browser, nil)
	r.model.mu.Lock()
	r.openLogin("codex", false)
	r.model.mu.Unlock()
	d := r.model.modal.login
	key := func(id string) { r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }
	if got := loginPaint(r); !strings.Contains(got, "cannot come back here") {
		t.Fatalf("dialog = %q", got)
	}
	for _, ch := range "http://localhost:1455/auth/callback?code=abc&state=s" {
		key(string(ch))
	}
	key("<Enter>")
	if submitted := browser.submissions(); len(submitted) != 1 || !strings.Contains(submitted[0], "code=abc") || d.paste.text() != "" {
		t.Fatalf("submitted %v, paste %q", submitted, d.paste.text())
	}
	if got := loginPaint(r); !strings.Contains(got, "redirect received") {
		t.Fatalf("dialog = %q", got)
	}
	// Tab reaches the buttons; Enter on the first opens the browser again.
	key("<Tab>")
	key("<Enter>")
	if len(*opened) != 2 {
		t.Fatalf("opened %v", *opened)
	}
	// Escape stops the sign-in and closes the browser login.
	key("<Escape>")
	if r.model.modal != nil || !browser.isClosed() || d.running {
		t.Fatalf("modal %v, closed %v, running %v", r.model.modal, browser.isClosed(), d.running)
	}
	if text := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(text, "sign-in cancelled") {
		t.Fatalf("transcript = %q", text)
	}
}

func TestLoginDialogSwitchesToADeviceCode(t *testing.T) {
	browser := newFakeBrowserLogin("localhost:1455")
	device := &fakeDeviceLogin{code: "ABCD-EFGH", done: make(chan llm.Account, 1)}
	r, _ := loginREPL(t, browser, device)
	r.model.mu.Lock()
	r.openLogin("codex", false)
	r.model.mu.Unlock()
	d := r.model.modal.login
	key := func(id string) { r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }
	key("<Tab>")
	key("<Tab>")
	key("<Enter>")
	if !d.device || !browser.isClosed() {
		t.Fatalf("device %v, browser closed %v", d.device, browser.isClosed())
	}
	// The code arrives on the loop once the authority answers; the
	// superseded browser sign-in's end may arrive first and is ignored.
	drainUITasksUntil(t, r, func() bool { return d.userCode != "" })
	if got := loginPaint(r); !strings.Contains(got, "ABCD-EFGH") || !strings.Contains(got, "https://auth.example/codex/device") || !strings.Contains(got, "[ Use browser instead ]") {
		t.Fatalf("dialog = %q", got)
	}
	device.done <- testAccount
	drainUITasksUntil(t, r, func() bool { return r.model.modal == nil })
}

// drainUITasksUntil runs the loop's UI tasks until cond holds.
func drainUITasksUntil(t *testing.T, r *managedREPL, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case task := <-r.uiTasks:
			task()
		case <-deadline:
			t.Fatal("the loop never reached the expected state")
		}
	}
}

func TestLoginDialogReportsAFailedSignIn(t *testing.T) {
	browser := newFakeBrowserLogin("localhost:1455")
	r, _ := loginREPL(t, browser, nil)
	r.model.mu.Lock()
	r.openLogin("codex", false)
	r.model.mu.Unlock()
	d := r.model.modal.login
	// The sign-in ends with an error instead of an account.
	close(browser.done)
	drainUITasksUntil(t, r, func() bool { return !d.running })
	if r.model.modal == nil || !strings.Contains(loginPaint(r), "sign-in failed") || !strings.Contains(loginPaint(r), "refused") {
		t.Fatalf("dialog = %v", r.model.modal)
	}
}
