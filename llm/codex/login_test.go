package codex

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ephemeralPorts makes sign-ins bind an ephemeral loopback port for the
// test's duration.
func ephemeralPorts(t *testing.T, ports ...int) {
	t.Helper()
	old := loginPorts
	loginPorts = ports
	t.Cleanup(func() { loginPorts = old })
}

// verifyingIssuer makes the issuer check the PKCE verifier and redirect it
// is sent against the sign-in's own authorize URL.
func verifyingIssuer(t *testing.T, issuer *fakeIssuer, l *Login) {
	t.Helper()
	u, err := url.Parse(l.URL())
	if err != nil {
		t.Fatal(err)
	}
	challenge, redirect := u.Query().Get("code_challenge"), u.Query().Get("redirect_uri")
	issuer.respond = func(form url.Values) (int, string, bool) {
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			return 400, `{"error":"invalid_grant","error_description":"verifier mismatch"}`, true
		}
		if form.Get("redirect_uri") != redirect {
			return 400, `{"error":"invalid_grant","error_description":"redirect mismatch"}`, true
		}
		return 0, "", false
	}
}

func loginState(t *testing.T, l *Login) string {
	t.Helper()
	u, err := url.Parse(l.URL())
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("state")
}

func TestLoginCompletesFromTheCallback(t *testing.T) {
	ephemeralPorts(t, 0)
	issuer := newFakeIssuer(t)
	s := newTestStore(t, issuer)
	l, err := s.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	verifyingIssuer(t, issuer, l)
	if l.Callback() == "" || !strings.HasPrefix(l.Callback(), "localhost:") {
		t.Fatalf("callback = %q", l.Callback())
	}
	u, _ := url.Parse(l.URL())
	if u.Query().Get("redirect_uri") != "http://"+l.Callback()+callbackPath {
		t.Fatalf("redirect_uri = %q for callback %q", u.Query().Get("redirect_uri"), l.Callback())
	}
	// The browser lands on the callback with the code.
	resp, err := http.Get("http://127.0.0.1" + strings.TrimPrefix(l.Callback(), "localhost") + callbackPath + "?code=abc&state=" + loginState(t, l))
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), "close this tab") {
		t.Fatalf("callback answered %d: %s", resp.StatusCode, page)
	}
	acct, err := l.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.ID != "acct_1" || acct.Email != "user@example.com" {
		t.Fatalf("account = %+v", acct)
	}
	if form := issuer.lastForm(); form.Get("code") != "abc" || form.Get("grant_type") != "authorization_code" {
		t.Fatalf("exchange form = %v", form)
	}
	if got, ok := s.Account(); !ok || !sameAccount(got, acct) {
		t.Fatalf("store account = %+v (%v)", got, ok)
	}
}

func TestLoginCallbackRejectsAWrongStateAndReportsARefusal(t *testing.T) {
	ephemeralPorts(t, 0)
	issuer := newFakeIssuer(t)
	s := newTestStore(t, issuer)
	l, err := s.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	base := "http://127.0.0.1" + strings.TrimPrefix(l.Callback(), "localhost") + callbackPath
	resp, err := http.Get(base + "?code=abc&state=someone-elses")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("wrong state answered %d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := l.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a wrong state ended the sign-in: %v", err)
	}
	resp, err = http.Get(base + "?error=access_denied&error_description=declined&state=" + loginState(t, l))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := l.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "declined") || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("refusal = %v", err)
	}
	if issuer.tokenCalls() != 0 {
		t.Fatalf("authority called %d times", issuer.tokenCalls())
	}
}

func TestLoginCompletesFromAPastedRedirect(t *testing.T) {
	ephemeralPorts(t, 0)
	issuer := newFakeIssuer(t)
	s := newTestStore(t, issuer)
	l, err := s.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	verifyingIssuer(t, issuer, l)
	if err := l.Submit("http://localhost:1455/auth/callback?code=xyz&state=other"); err == nil {
		t.Fatal("a paste with another sign-in's state was accepted")
	}
	if err := l.Submit("http://localhost:1455/auth/callback?code=xyz&state=" + loginState(t, l)); err != nil {
		t.Fatal(err)
	}
	acct, err := l.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.ID != "acct_1" || issuer.lastForm().Get("code") != "xyz" {
		t.Fatalf("account = %+v, form = %v", acct, issuer.lastForm())
	}
}

func TestLoginWithoutAFreePortFallsBackToPaste(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port
	ephemeralPorts(t, port)
	issuer := newFakeIssuer(t)
	s := newTestStore(t, issuer)
	l, err := s.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Callback() != "" {
		t.Fatalf("callback bound on a busy port: %q", l.Callback())
	}
	u, _ := url.Parse(l.URL())
	if want := "http://localhost:" + strconv.Itoa(port) + callbackPath; u.Query().Get("redirect_uri") != want {
		t.Fatalf("redirect_uri = %q, want the allow-listed %q", u.Query().Get("redirect_uri"), want)
	}
	verifyingIssuer(t, issuer, l)
	if err := l.Submit("pasted-code"); err != nil {
		t.Fatal(err)
	}
	if acct, err := l.Wait(context.Background()); err != nil || acct.ID != "acct_1" {
		t.Fatalf("account = %+v, err = %v", acct, err)
	}
}

func TestLoginCloseEndsTheWait(t *testing.T) {
	ephemeralPorts(t, 0)
	issuer := newFakeIssuer(t)
	l, err := newTestStore(t, issuer).StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := l.Wait(context.Background())
		done <- err
	}()
	l.Close()
	l.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrLoginClosed) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait outlived Close")
	}
	if _, err := http.Get("http://127.0.0.1" + strings.TrimPrefix(l.Callback(), "localhost") + callbackPath); err == nil {
		t.Fatal("callback still listening after Close")
	}
}

func TestParseRedirect(t *testing.T) {
	for _, tc := range []struct {
		in, code, err string
	}{
		{"http://localhost:1455/auth/callback?code=abc&state=st", "abc", ""},
		{"  http://localhost:1455/auth/callback?state=st&code=abc  ", "abc", ""},
		{"?code=abc&state=st", "abc", ""},
		{"code=abc&state=st", "abc", ""},
		{"abc#st", "abc", ""},
		{"abc", "abc", ""},
		{"http://localhost:1455/auth/callback?code=abc&state=other", "", "state"},
		{"abc#other", "", "state"},
		{"http://localhost:1455/auth/callback?error=access_denied", "", "refused"},
		{"http://localhost:1455/auth/callback", "", "no authorization code"},
		{"", "", "nothing was pasted"},
		{"two words", "", "not an authorization code"},
		{"::not a url::", "", "not an authorization code"},
	} {
		code, err := parseRedirect(tc.in, "st")
		if tc.err == "" {
			if err != nil || code != tc.code {
				t.Errorf("parseRedirect(%q) = %q, %v; want %q", tc.in, code, err, tc.code)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("parseRedirect(%q) = %q, %v; want an error mentioning %q", tc.in, code, err, tc.err)
		}
	}
}

func TestDeviceLoginWaitsForApproval(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.pending = 2
	s := newTestStore(t, issuer)
	d, err := s.StartDeviceLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.UserCode() != "ABCD-EFGH" || d.VerifyURL() != issuer.URL+"/codex/device" {
		t.Fatalf("device login = %q at %q", d.UserCode(), d.VerifyURL())
	}
	d.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	acct, err := d.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.ID != "acct_1" || issuer.lastForm().Get("code") != "dev-code" {
		t.Fatalf("account = %+v, form = %v", acct, issuer.lastForm())
	}
	if got, ok := s.Account(); !ok || !sameAccount(got, acct) {
		t.Fatalf("store account = %+v (%v)", got, ok)
	}
	issuer.userCodeStatus = 403
	if _, err := s.StartDeviceLogin(context.Background()); !errors.Is(err, ErrDeviceLoginUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
