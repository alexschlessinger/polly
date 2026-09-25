package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

func newTestStore(t *testing.T, issuer *fakeIssuer, opts ...StoreOption) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	return NewStore(path, append([]StoreOption{WithIssuer(issuer.URL), WithStoreHTTPClient(issuer.Client())}, opts...)...)
}

// sameAccount compares accounts by instant, not by time.Time internals.
func sameAccount(a, b contract.Account) bool {
	return a.ID == b.ID && a.Email == b.Email && a.Plan == b.Plan && a.ExpiresAt.Equal(b.ExpiresAt)
}

// storeFileOnDisk decodes the store file as written.
func storeFileOnDisk(t *testing.T, path string) storeFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file storeFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("store file %s: %v", raw, err)
	}
	return file
}

func TestStoreSavesTheSignInPrivately(t *testing.T) {
	issuer := newFakeIssuer(t)
	s := newTestStore(t, issuer)
	ctx := context.Background()
	if _, ok := s.Account(); ok {
		t.Fatal("account reported before any sign-in")
	}
	if _, err := s.Credential(ctx); !errors.Is(err, contract.ErrNotSignedIn) {
		t.Fatalf("credential before sign-in: %v", err)
	}
	tokens := issuer.tokens()
	acct, err := s.Save(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if acct.ID != "acct_1" || acct.Email != "user@example.com" || acct.Plan != "plus" || acct.ExpiresAt.IsZero() {
		t.Fatalf("account = %+v", acct)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("store mode = %o, want 0600", info.Mode().Perm())
	}
	file := storeFileOnDisk(t, s.Path())
	if file.Version != 1 || file.Codex == nil || file.Codex.AccessToken != tokens.AccessToken || file.Codex.RefreshToken != "refresh-1" || file.Codex.AccountID != "acct_1" {
		t.Fatalf("store file = %+v", file)
	}
	cred, err := s.Credential(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken != tokens.AccessToken || cred.AccountID != "acct_1" || !cred.ExpiresAt.Equal(acct.ExpiresAt) {
		t.Fatalf("credential = %+v", cred)
	}
	if issuer.tokenCalls() != 0 {
		t.Fatalf("a fresh token was refreshed: %d authority calls", issuer.tokenCalls())
	}
	if got, ok := s.Account(); !ok || !sameAccount(got, acct) {
		t.Fatalf("account = %+v (%v), want %+v", got, ok, acct)
	}
}

func TestStoreRefreshesATokenNearExpiry(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.lifetime = 2 * time.Minute
	s := newTestStore(t, issuer)
	first := issuer.tokens()
	if _, err := s.Save(first); err != nil {
		t.Fatal(err)
	}
	issuer.lifetime = time.Hour
	cred, err := s.Credential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken == first.AccessToken || issuer.tokenCalls() != 1 || issuer.lastForm().Get("refresh_token") != "refresh-1" {
		t.Fatalf("credential = %+v after %d calls, form %v", cred, issuer.tokenCalls(), issuer.lastForm())
	}
	if file := storeFileOnDisk(t, s.Path()); file.Codex.RefreshToken != "refresh-2" || file.Codex.AccessToken != cred.AccessToken {
		t.Fatalf("rotation not persisted: %+v", file.Codex)
	}
	if again, err := s.Credential(context.Background()); err != nil || again.AccessToken != cred.AccessToken || issuer.tokenCalls() != 1 {
		t.Fatalf("second credential = %+v (%v) after %d calls", again, err, issuer.tokenCalls())
	}
}

func TestStoreDefersATransientRefreshFailure(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.lifetime = 2 * time.Minute
	now := time.Now()
	s := newTestStore(t, issuer, withClock(func() time.Time { return now }))
	tokens := issuer.tokens()
	if _, err := s.Save(tokens); err != nil {
		t.Fatal(err)
	}
	issuer.respond = func(url.Values) (int, string, bool) { return 503, `{"error":"server_error"}`, true }
	cred, err := s.Credential(context.Background())
	if err != nil || cred.AccessToken != tokens.AccessToken {
		t.Fatalf("credential = %+v, err = %v; want the still-valid token", cred, err)
	}
	now = now.Add(3 * time.Minute)
	if _, err := s.Credential(context.Background()); err == nil || errors.Is(err, contract.ErrNotSignedIn) {
		t.Fatalf("expired token with a failing authority: %v", err)
	}
}

func TestStoreNoticesAnotherProcessRotatingTheSignIn(t *testing.T) {
	issuer := newFakeIssuer(t)
	s1 := newTestStore(t, issuer)
	s2 := NewStore(s1.Path(), WithIssuer(issuer.URL), WithStoreHTTPClient(issuer.Client()))
	ctx := context.Background()
	first := issuer.tokens()
	if _, err := s1.Save(first); err != nil {
		t.Fatal(err)
	}
	if cred, err := s1.Credential(ctx); err != nil || cred.AccessToken != first.AccessToken {
		t.Fatalf("credential = %+v, %v", cred, err)
	}
	// The second process signs in again with a longer email, so the file
	// changes size as well as mtime.
	issuer.email = "someone.with.a.longer.address@example.com"
	second := issuer.tokens()
	if _, err := s2.Save(second); err != nil {
		t.Fatal(err)
	}
	if cred, err := s1.Credential(ctx); err != nil || cred.AccessToken != second.AccessToken {
		t.Fatalf("s1 did not see the new sign-in: %+v, %v", cred, err)
	}
	if acct, ok := s1.Account(); !ok || acct.Email != issuer.email {
		t.Fatalf("account = %+v (%v)", acct, ok)
	}
	// A refresh of a token that was already replaced returns the
	// replacement without contacting the authority.
	if cred, err := s1.Refresh(ctx, first.AccessToken); err != nil || cred.AccessToken != second.AccessToken || issuer.tokenCalls() != 0 {
		t.Fatalf("refresh of a replaced token: %+v, %v, %d calls", cred, err, issuer.tokenCalls())
	}
	// A refresh of the current token rotates it for both stores.
	rotated, err := s1.Refresh(ctx, second.AccessToken)
	if err != nil || rotated.AccessToken == second.AccessToken || issuer.tokenCalls() != 1 {
		t.Fatalf("refresh: %+v, %v, %d calls", rotated, err, issuer.tokenCalls())
	}
	if cred, err := s2.Credential(ctx); err != nil || cred.AccessToken != rotated.AccessToken {
		t.Fatalf("s2 did not see the rotation: %+v, %v", cred, err)
	}
}

func TestStoreClearsARevokedSignIn(t *testing.T) {
	issuer := newFakeIssuer(t)
	s := newTestStore(t, issuer)
	tokens := issuer.tokens()
	if _, err := s.Save(tokens); err != nil {
		t.Fatal(err)
	}
	issuer.respond = func(url.Values) (int, string, bool) { return 400, `{"error":"invalid_grant"}`, true }
	_, err := s.Refresh(context.Background(), tokens.AccessToken)
	if !errors.Is(err, contract.ErrNotSignedIn) || !strings.Contains(err.Error(), "sign in again") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(s.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("revoked sign-in still on disk: %v", err)
	}
	if _, ok := s.Account(); ok {
		t.Fatal("account after revocation")
	}
	if _, err := s.Credential(context.Background()); !errors.Is(err, contract.ErrNotSignedIn) {
		t.Fatalf("credential after revocation: %v", err)
	}
}

func TestStoreRefreshesOnceForConcurrentCallers(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.lifetime = time.Minute
	s := newTestStore(t, issuer)
	if _, err := s.Save(issuer.tokens()); err != nil {
		t.Fatal(err)
	}
	issuer.lifetime = time.Hour
	var wg sync.WaitGroup
	tokens := make([]string, 10)
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cred, err := s.Credential(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			tokens[i] = cred.AccessToken
		}()
	}
	wg.Wait()
	for _, token := range tokens[1:] {
		if token != tokens[0] {
			t.Fatalf("callers got different tokens: %q vs %q", token, tokens[0])
		}
	}
	if issuer.tokenCalls() != 1 {
		t.Fatalf("authority called %d times, want once", issuer.tokenCalls())
	}
}

func TestStoreClearRemovesTheSignIn(t *testing.T) {
	issuer := newFakeIssuer(t)
	s := newTestStore(t, issuer)
	if err := s.Clear(); err != nil {
		t.Fatalf("clearing an empty store: %v", err)
	}
	if _, err := s.Save(issuer.tokens()); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Account(); ok {
		t.Fatal("account after clear")
	}
	if _, err := os.Stat(s.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file after clear: %v", err)
	}
}

func TestStoreReportsAnUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	for _, tc := range []struct{ body, want string }{
		{"{not json", "not a sign-in store"},
		{`{"version":2,"codex":{"access_token":"x"}}`, "store version 2"},
	} {
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		s := NewStore(path)
		_, err := s.Credential(context.Background())
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path) {
			t.Fatalf("%s: err = %v", tc.body, err)
		}
		if _, ok := s.Account(); ok {
			t.Fatalf("%s: account reported", tc.body)
		}
	}
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path).Credential(context.Background()); !errors.Is(err, contract.ErrNotSignedIn) {
		t.Fatalf("empty store: %v", err)
	}
}

func TestStoreRefreshWaitsForTheFileLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no advisory locks on windows")
	}
	issuer := newFakeIssuer(t)
	s := newTestStore(t, issuer)
	tokens := issuer.tokens()
	if _, err := s.Save(tokens); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFile(context.Background(), s.Path()+".lock")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Refresh(context.Background(), tokens.AccessToken)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("refresh ran under a held lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh never acquired the lock")
	}
	if issuer.tokenCalls() != 1 {
		t.Fatalf("authority calls = %d", issuer.tokenCalls())
	}
}
