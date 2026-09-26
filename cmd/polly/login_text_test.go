package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
)

// fakeBrowserLogin is a browser sign-in the test finishes by hand, or
// that finishes itself on a submitted redirect when finishOnSubmit is set.
type fakeBrowserLogin struct {
	mu             sync.Mutex
	url            string
	callback       string
	submitted      []string
	submitErr      error
	finishOnSubmit bool
	done           chan llm.Account
	closed         bool
}

func newFakeBrowserLogin(callback string) *fakeBrowserLogin {
	return &fakeBrowserLogin{url: "https://auth.example/authorize?state=s", callback: callback, done: make(chan llm.Account, 1)}
}

func (l *fakeBrowserLogin) URL() string      { return l.url }
func (l *fakeBrowserLogin) Callback() string { return l.callback }
func (l *fakeBrowserLogin) Submit(pasted string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.submitErr != nil {
		return l.submitErr
	}
	l.submitted = append(l.submitted, pasted)
	if l.finishOnSubmit {
		l.done <- testAccount
	}
	return nil
}
func (l *fakeBrowserLogin) Wait(ctx context.Context) (llm.Account, error) {
	select {
	case <-ctx.Done():
		return llm.Account{}, ctx.Err()
	case acct, ok := <-l.done:
		if !ok {
			return llm.Account{}, errors.New("the sign-in was refused")
		}
		return acct, nil
	}
}
func (l *fakeBrowserLogin) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
}
func (l *fakeBrowserLogin) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}
func (l *fakeBrowserLogin) submissions() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.submitted...)
}
func (l *fakeBrowserLogin) finish(acct llm.Account) { l.done <- acct }

type fakeDeviceLogin struct {
	code string
	done chan llm.Account
}

func (d *fakeDeviceLogin) UserCode() string  { return d.code }
func (d *fakeDeviceLogin) VerifyURL() string { return "https://auth.example/codex/device" }
func (d *fakeDeviceLogin) Wait(ctx context.Context) (llm.Account, error) {
	select {
	case <-ctx.Done():
		return llm.Account{}, ctx.Err()
	case acct := <-d.done:
		return acct, nil
	}
}

var testAccount = llm.Account{ID: "acct_1", Email: "user@example.com", Plan: "plus"}

// fakeLoginFlow is a flow on fakes, with a sign-in the test can set.
func fakeLoginFlow(browser *fakeBrowserLogin, device *fakeDeviceLogin) (loginFlow, *bool) {
	signedIn := true
	flow := loginFlow{
		browser: func() (browserLogin, error) {
			if browser == nil {
				return nil, errors.New("no browser sign-in")
			}
			return browser, nil
		},
		device: func(context.Context) (deviceLogin, error) {
			if device == nil {
				return nil, errors.New("no device sign-in")
			}
			return device, nil
		},
		account: func() (llm.Account, bool) {
			if !signedIn {
				return llm.Account{}, false
			}
			return testAccount, true
		},
		signOut: func() error { signedIn = false; return nil },
	}
	return flow, &signedIn
}

// useFakeLoginFlow makes every sign-in command in the test use flow.
func useFakeLoginFlow(t *testing.T, flow loginFlow) {
	t.Helper()
	old := newLoginFlow
	newLoginFlow = func(string) (loginFlow, error) { return flow, nil }
	t.Cleanup(func() { newLoginFlow = old })
}

func TestRunTextLoginWaitsForTheCallback(t *testing.T) {
	browser := newFakeBrowserLogin("localhost:1455")
	flow, _ := fakeLoginFlow(browser, nil)
	var said []string
	var opened string
	answers := []string{""}
	readInput := func(prompt string) (string, error) {
		if !strings.Contains(prompt, "Press Enter") {
			t.Errorf("prompt = %q", prompt)
		}
		answer := answers[0]
		answers = answers[1:]
		return answer, nil
	}
	browser.finish(testAccount)
	acct, err := runTextLogin(context.Background(), flow, false, readInput, func(s string) { said = append(said, s) }, func(u string) error { opened = u; return nil })
	if err != nil || acct != testAccount {
		t.Fatalf("account = %+v, err = %v", acct, err)
	}
	text := strings.Join(said, "\n")
	if opened != browser.url || !strings.Contains(text, browser.url) || !strings.Contains(text, "opening that page") || !strings.Contains(text, "comes back to localhost:1455") {
		t.Fatalf("said %q, opened %q", text, opened)
	}
	if len(browser.submissions()) != 0 || !browser.isClosed() {
		t.Fatalf("submitted %v, closed %v", browser.submissions(), browser.isClosed())
	}
}

func TestRunTextLoginTakesAPastedRedirect(t *testing.T) {
	browser := newFakeBrowserLogin("")
	browser.submitErr = errors.New("codex: the pasted text is not a URL")
	browser.finishOnSubmit = true
	flow, _ := fakeLoginFlow(browser, nil)
	var said []string
	answers := []string{"garbage", "http://localhost:1455/auth/callback?code=abc&state=s"}
	readInput := func(string) (string, error) {
		answer := answers[0]
		answers = answers[1:]
		if len(answers) == 0 {
			browser.submitErr = nil
		}
		return answer, nil
	}
	acct, err := runTextLogin(context.Background(), flow, false, readInput, func(s string) { said = append(said, s) }, func(string) error { return errors.New("no browser") })
	if err != nil || acct != testAccount {
		t.Fatalf("account = %+v, err = %v", acct, err)
	}
	text := strings.Join(said, "\n")
	if strings.Contains(text, "opening that page") || !strings.Contains(text, "cannot come back here") || !strings.Contains(text, "not a URL") {
		t.Fatalf("said %q", text)
	}
	if submitted := browser.submissions(); len(submitted) != 1 || !strings.Contains(submitted[0], "code=abc") {
		t.Fatalf("submitted %v", submitted)
	}
}

func TestRunTextLoginDeviceAndWithoutInput(t *testing.T) {
	device := &fakeDeviceLogin{code: "ABCD-EFGH", done: make(chan llm.Account, 1)}
	flow, _ := fakeLoginFlow(nil, device)
	var said []string
	device.done <- testAccount
	acct, err := runTextLogin(context.Background(), flow, true, nil, func(s string) { said = append(said, s) }, nil)
	if err != nil || acct != testAccount {
		t.Fatalf("account = %+v, err = %v", acct, err)
	}
	if text := strings.Join(said, "\n"); !strings.Contains(text, "ABCD-EFGH") || !strings.Contains(text, "https://auth.example/codex/device") {
		t.Fatalf("said %q", text)
	}

	browser := newFakeBrowserLogin("localhost:1455")
	flow, _ = fakeLoginFlow(browser, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runTextLogin(ctx, flow, false, nil, func(string) {}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, err := runTextLogin(context.Background(), loginFlow{browser: func() (browserLogin, error) { return nil, errors.New("port busy") }}, false, nil, func(string) {}, nil); err == nil || !strings.Contains(err.Error(), "port busy") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginProviderAndNotice(t *testing.T) {
	for _, tc := range []struct{ in, want, err string }{
		{"", "codex", ""},
		{" Codex ", "codex", ""},
		{"openai", "", "takes an API key"},
		{"nope", "", "unknown provider"},
	} {
		got, err := loginProvider(tc.in)
		if tc.err == "" && (err != nil || got != tc.want) {
			t.Errorf("loginProvider(%q) = %q, %v", tc.in, got, err)
		}
		if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("loginProvider(%q) = %q, %v; want %q", tc.in, got, err, tc.err)
		}
	}
	if got := signedInNotice("codex", testAccount); got != "signed in to codex as user@example.com (plus plan)" {
		t.Fatalf("notice = %q", got)
	}
	if got := signedInNotice("codex", llm.Account{ID: "acct_2"}); got != "signed in to codex as acct_2" {
		t.Fatalf("notice = %q", got)
	}
	if got := missingLoginNotice("codex/gpt-5.5"); !strings.Contains(got, "polly --login codex") || !strings.Contains(got, "/login") {
		t.Fatalf("notice = %q", got)
	}
}
