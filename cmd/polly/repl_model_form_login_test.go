package main

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
)

// fakeAccountLogin is a sign-in the tests can switch off.
type fakeAccountLogin struct{ signedOut bool }

func (l *fakeAccountLogin) Credential(context.Context) (llm.Credential, error) {
	if l.signedOut {
		return llm.Credential{}, llm.ErrNotSignedIn
	}
	return llm.Credential{AccessToken: "tok", AccountID: "acct_1"}, nil
}

func (l *fakeAccountLogin) Refresh(ctx context.Context, _ string) (llm.Credential, error) {
	return l.Credential(ctx)
}

func (l *fakeAccountLogin) Account() (llm.Account, bool) {
	if l.signedOut {
		return llm.Account{}, false
	}
	return testAccount, true
}

func TestModelFormShowsTheAccountForASignedInProvider(t *testing.T) {
	login := &fakeAccountLogin{}
	mp := llm.NewMultiPass(nil, llm.WithLogin("codex", login))
	a := llm.NewAgent(mp, nil, llm.AgentConfig{})
	t.Cleanup(func() { a.Close() })
	r := newManagedREPL(&Config{}, "form", 0, 0)
	_ = r.work.close()
	r.work = nil
	r.state = &conversationState{agent: a, settings: Settings{Model: "codex/gpt-5.5"}}
	r.openModelPicker()
	f := r.model.modal.modelForm
	if f.provider != "codex" || !f.login || f.hasKey {
		t.Fatalf("form = provider %q, login %v, hasKey %v", f.provider, f.login, f.hasKey)
	}
	text := plainStyledText(f.modal.text(20, 80))
	if !strings.Contains(text, "Account  Signed in as user@example.com (plus plan)") || strings.Contains(text, "Key ") {
		t.Fatalf("form = %q", text)
	}
	// The account row takes no typing.
	r.focusModelForm(f, formFieldKey)
	formKey(r, "x")
	formKey(r, "<Backspace>")
	formKey(r, "<Left>")
	if f.keyChanged || f.key.text() != "" || f.err != "" {
		t.Fatalf("typing on the account row: changed %v, key %q, err %q", f.keyChanged, f.key.text(), f.err)
	}
	// Another provider brings the key row back.
	r.selectFormProvider(f, "openai")
	if f.login || !strings.Contains(plainStyledText(f.modal.text(20, 80)), "Key ") {
		t.Fatal("the key row did not come back for openai")
	}

	// Signed out, the row says so and setup refuses to apply.
	login.signedOut = true
	r.closeModal()
	r.openSetupForm()
	f = r.model.modal.modelForm
	if !strings.Contains(plainStyledText(f.modal.text(30, 80)), "Account  Not signed in · /login codex") {
		t.Fatalf("form = %q", plainStyledText(f.modal.text(30, 80)))
	}
	f.model.setText("gpt-5.5")
	r.focusModelForm(f, f.applyIndex())
	formKey(r, "<Enter>")
	if r.model.modal == nil || f.err != "Sign in first: /login codex" || f.focus != formFieldKey {
		t.Fatalf("apply while signed out: modal %v, err %q, focus %d", r.model.modal != nil, f.err, f.focus)
	}
}
