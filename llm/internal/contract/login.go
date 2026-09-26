package contract

import (
	"context"
	"errors"
	"time"
)

// Login is a provider credential that outlives any one request: a sign-in
// whose access token rotates. A provider asks for the current credential
// before each request and, when the backend rejects one, asks for a refresh
// naming the token that was refused, so callers sharing one store never
// spend the same refresh token twice.
type Login interface {
	// Credential returns a token expected to be accepted now, refreshing
	// one about to expire. It reports ErrNotSignedIn when there is no
	// sign-in.
	Credential(ctx context.Context) (Credential, error)
	// Refresh replaces the token the backend rejected. When the store
	// already holds a different token, because another caller refreshed
	// first, that one comes back without contacting the authority.
	Refresh(ctx context.Context, rejected string) (Credential, error)
	// Account describes the sign-in without contacting anything; ok is
	// false when there is none.
	Account() (Account, bool)
}

// Credential is what a request carries: the bearer token and the account
// it belongs to.
type Credential struct {
	AccessToken string
	AccountID   string
	ExpiresAt   time.Time
}

// Account describes a sign-in for display: never the tokens.
type Account struct {
	ID    string
	Email string
	// Plan is the subscription the account is on, as the authority names
	// it ("plus", "pro", "team", ...).
	Plan      string
	ExpiresAt time.Time
}

// ErrNotSignedIn reports that a provider needing a sign-in has none.
var ErrNotSignedIn = errors.New("not signed in")
