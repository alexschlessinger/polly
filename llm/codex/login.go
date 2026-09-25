package codex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// Login is one browser sign-in in progress: the page to open, the loopback
// callback waiting for the redirect, and the paste fallback for when the
// browser cannot reach that callback. Close it when done.
type Login struct {
	store    *Store
	oauth    *oauthClient
	pkce     pkce
	state    string
	redirect string
	url      string
	server   *callbackServer
	results  chan loginResult
	closed   chan struct{}
	once     sync.Once
}

// ErrLoginClosed reports a sign-in closed before it finished.
var ErrLoginClosed = errors.New("codex: the sign-in was closed")

// StartLogin begins a browser sign-in into s. The callback binds one of
// the authority's loopback ports; when none is free, the sign-in still
// works through Submit and the redirect names the first port anyway,
// since the authority accepts only those.
func (s *Store) StartLogin() (*Login, error) {
	p, err := newPKCE()
	if err != nil {
		return nil, err
	}
	state, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	l := &Login{store: s, oauth: s.oauth, pkce: p, state: state, results: make(chan loginResult, 1), closed: make(chan struct{})}
	server, err := listenCallback(loginPorts, state, l.results)
	if err != nil {
		slog.Debug("codex_login_callback_unbound", "error", err)
		l.redirect = fmt.Sprintf("http://localhost:%d%s", loginPorts[0], callbackPath)
	} else {
		l.server = server
		l.redirect = server.redirectURI()
	}
	l.url = s.oauth.authorizeURL(l.redirect, state, p)
	return l, nil
}

// URL is the page the user signs in on.
func (l *Login) URL() string { return l.url }

// Callback is the loopback address waiting for the redirect, "" when no
// port could be bound and the user must paste the redirect instead.
func (l *Login) Callback() string {
	if l.server == nil {
		return ""
	}
	return fmt.Sprintf("localhost:%d", l.server.port)
}

// Submit takes the redirect the user pasted: the URL from the browser's
// address bar, its query, or the bare code.
func (l *Login) Submit(pasted string) error {
	code, err := parseRedirect(pasted, l.state)
	if err != nil {
		return fmt.Errorf("codex: %w", err)
	}
	select {
	case l.results <- loginResult{code: code}:
	default:
	}
	return nil
}

// Wait blocks until the redirect arrives, from the callback or a paste,
// then redeems it and saves the sign-in.
func (l *Login) Wait(ctx context.Context) (contract.Account, error) {
	select {
	case <-ctx.Done():
		return contract.Account{}, ctx.Err()
	case <-l.closed:
		return contract.Account{}, ErrLoginClosed
	case r := <-l.results:
		if r.err != nil {
			return contract.Account{}, fmt.Errorf("codex: %w", r.err)
		}
		tokens, err := l.oauth.exchange(ctx, r.code, l.redirect, l.pkce.verifier)
		if err != nil {
			return contract.Account{}, err
		}
		return l.store.Save(tokens)
	}
}

// Close releases the callback port and ends a pending Wait.
func (l *Login) Close() {
	l.once.Do(func() {
		close(l.closed)
		if l.server != nil {
			_ = l.server.Close()
		}
	})
}

// DeviceLogin is a device sign-in in progress: the code to type on the
// authority's page, and the poll that waits for it.
type DeviceLogin struct {
	store *Store
	oauth *oauthClient
	code  deviceCode
	sleep func(context.Context, time.Duration) error
}

// StartDeviceLogin asks the authority for a user code. It needs no browser
// on this machine and no free port.
func (s *Store) StartDeviceLogin(ctx context.Context) (*DeviceLogin, error) {
	dc, err := s.oauth.requestDeviceCode(ctx)
	if err != nil {
		return nil, err
	}
	return &DeviceLogin{store: s, oauth: s.oauth, code: dc, sleep: sleepFor}, nil
}

// UserCode is what the user types on the verification page.
func (d *DeviceLogin) UserCode() string { return d.code.UserCode }

// VerifyURL is the page the user types the code on.
func (d *DeviceLogin) VerifyURL() string { return d.oauth.issuer + deviceVerifyPath }

// Wait polls until the user approves the code, then redeems the grant and
// saves the sign-in.
func (d *DeviceLogin) Wait(ctx context.Context) (contract.Account, error) {
	grant, err := d.oauth.pollDeviceCode(ctx, d.code, d.sleep)
	if err != nil {
		return contract.Account{}, err
	}
	tokens, err := d.oauth.redeemDevice(ctx, grant)
	if err != nil {
		return contract.Account{}, err
	}
	return d.store.Save(tokens)
}
