package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/internal/safefile"
	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

const (
	storeVersion = 1
	// refreshMargin is how close to expiry a token is refreshed ahead of a
	// request rather than risked on one.
	refreshMargin = 5 * time.Minute
	// maxStoreBody bounds the store file.
	maxStoreBody = 64 << 10
)

// record is the on-disk sign-in.
type record struct {
	IDToken      string    `json:"id_token,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	AccountID    string    `json:"account_id"`
	Email        string    `json:"email,omitempty"`
	Plan         string    `json:"plan,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	SavedAt      time.Time `json:"saved_at"`
}

func (r *record) credential() contract.Credential {
	return contract.Credential{AccessToken: r.AccessToken, AccountID: r.AccountID, ExpiresAt: r.ExpiresAt}
}

func (r *record) account() contract.Account {
	return contract.Account{ID: r.AccountID, Email: r.Email, Plan: r.Plan, ExpiresAt: r.ExpiresAt}
}

// storeFile is the file: a version and the one sign-in it holds.
type storeFile struct {
	Version int     `json:"version"`
	Codex   *record `json:"codex,omitempty"`
}

// Store keeps a sign-in in one JSON file that every polly process on the
// machine shares, readable only by its owner. The file is authoritative:
// reads notice a write by another process, and a refresh runs under a file
// lock after re-reading, so a token rotated elsewhere is picked up rather
// than refreshed again (the authority invalidates a refresh token on its
// second use). Store implements contract.Login.
type Store struct {
	path  string
	oauth *oauthClient
	now   func() time.Time

	mu sync.Mutex
	// cached is the record last read, with the identity of the file it
	// came from; a read compares identity and reloads when the file moved
	// on. loaded is false until the first read.
	cached     *record
	cachedMod  time.Time
	cachedSize int64
	loaded     bool
}

var _ contract.Login = (*Store)(nil)

// StoreOption configures a Store.
type StoreOption func(*Store, *storeConfig)

type storeConfig struct {
	issuer string
	client *http.Client
}

// WithStoreHTTPClient supplies the client the store refreshes through.
func WithStoreHTTPClient(client *http.Client) StoreOption {
	return func(_ *Store, c *storeConfig) { c.client = client }
}

// WithIssuer points the store at another authority; tests use it.
func WithIssuer(issuer string) StoreOption {
	return func(_ *Store, c *storeConfig) { c.issuer = issuer }
}

func withClock(now func() time.Time) StoreOption {
	return func(s *Store, _ *storeConfig) { s.now = now }
}

// NewStore returns the store at path, which need not exist yet.
func NewStore(path string, opts ...StoreOption) *Store {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	s := &Store{path: path, now: time.Now}
	cfg := storeConfig{issuer: Issuer}
	for _, opt := range opts {
		opt(s, &cfg)
	}
	s.oauth = newOAuthClient(cfg.issuer, cfg.client)
	return s
}

// Path is the store file.
func (s *Store) Path() string { return s.path }

// Account describes the sign-in on file, or reports none, including when
// the file cannot be read.
func (s *Store) Account() (contract.Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.load()
	if err != nil || rec == nil {
		return contract.Account{}, false
	}
	return rec.account(), true
}

// Credential returns the token to send now. One within refreshMargin of
// expiry is refreshed first; when that fails for a passing reason and the
// token is still good, the token is returned and the refresh left to the
// next call.
func (s *Store) Credential(ctx context.Context) (contract.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.load()
	if err != nil {
		return contract.Credential{}, err
	}
	if rec == nil {
		return contract.Credential{}, contract.ErrNotSignedIn
	}
	now := s.now()
	if rec.ExpiresAt.IsZero() || now.Add(refreshMargin).Before(rec.ExpiresAt) {
		return rec.credential(), nil
	}
	fresh, err := s.refresh(ctx, rec.AccessToken)
	if err != nil {
		if errors.Is(err, contract.ErrNotSignedIn) || !now.Before(rec.ExpiresAt) {
			return contract.Credential{}, err
		}
		slog.Debug("codex_refresh_deferred", "error", err, "expires_at", rec.ExpiresAt)
		return rec.credential(), nil
	}
	return fresh, nil
}

// Refresh replaces rejected, the token the backend refused, unless the
// store already holds another.
func (s *Store) Refresh(ctx context.Context, rejected string) (contract.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refresh(ctx, rejected)
}

// refresh rotates the sign-in under the file lock. Caller holds s.mu.
func (s *Store) refresh(ctx context.Context, rejected string) (contract.Credential, error) {
	rec, err := s.load()
	if err != nil {
		return contract.Credential{}, err
	}
	if rec == nil {
		return contract.Credential{}, contract.ErrNotSignedIn
	}
	unlock, err := lockFile(ctx, s.path+".lock")
	if err != nil {
		return contract.Credential{}, fmt.Errorf("codex: locking the sign-in store: %w", err)
	}
	defer unlock()
	if rec, err = s.read(); err != nil {
		return contract.Credential{}, err
	}
	if rec == nil {
		return contract.Credential{}, contract.ErrNotSignedIn
	}
	if rec.AccessToken != rejected {
		// Another process rotated the sign-in while this one waited.
		return rec.credential(), nil
	}
	tokens, err := s.oauth.refresh(ctx, rec.RefreshToken)
	if err != nil {
		if errors.Is(err, ErrRefreshRevoked) {
			if clearErr := s.clear(); clearErr != nil {
				slog.Debug("codex_clear_failed", "error", clearErr)
			}
			return contract.Credential{}, fmt.Errorf("codex: the sign-in has expired; sign in again: %w", contract.ErrNotSignedIn)
		}
		return contract.Credential{}, err
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = rec.RefreshToken
	}
	if tokens.IDToken == "" {
		tokens.IDToken = rec.IDToken
	}
	saved, err := s.save(tokens)
	if err != nil {
		return contract.Credential{}, err
	}
	return saved.credential(), nil
}

// Save records a fresh grant, replacing any sign-in on file, and describes
// the account it names.
func (s *Store) Save(tokens Tokens) (contract.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.save(tokens)
	if err != nil {
		return contract.Account{}, err
	}
	return rec.account(), nil
}

func (s *Store) save(tokens Tokens) (*record, error) {
	now := s.now()
	c, err := tokens.claims(now)
	if err != nil {
		return nil, err
	}
	rec := &record{
		IDToken:      tokens.IDToken,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		AccountID:    c.AccountID,
		Email:        c.Email,
		Plan:         c.PlanType,
		ExpiresAt:    c.Expiry,
		SavedAt:      now,
	}
	if err := s.write(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// Clear removes the sign-in.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clear()
}

func (s *Store) clear() error {
	s.cached, s.loaded = nil, false
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("codex: removing the sign-in: %w", err)
	}
	return nil
}

// load returns the record on file, re-reading it only when the file's
// identity changed since the last read. Caller holds s.mu.
func (s *Store) load() (*record, error) {
	f, err := safefile.OpenRegularFollow(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.cached, s.loaded = nil, false
			return nil, nil
		}
		return nil, fmt.Errorf("codex: opening the sign-in store: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("codex: reading the sign-in store: %w", err)
	}
	if s.loaded && info.ModTime().Equal(s.cachedMod) && info.Size() == s.cachedSize {
		return s.cached, nil
	}
	rec, err := decodeStore(f, s.path)
	if err != nil {
		return nil, err
	}
	s.cached, s.cachedMod, s.cachedSize, s.loaded = rec, info.ModTime(), info.Size(), true
	return rec, nil
}

// read returns the record on file, always reading it. Caller holds s.mu.
func (s *Store) read() (*record, error) {
	s.loaded = false
	return s.load()
}

// decodeStore reads a store file; a sign-in without an access token counts
// as none.
func decodeStore(r io.Reader, path string) (*record, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxStoreBody+1))
	if err != nil {
		return nil, fmt.Errorf("codex: reading %s: %w", path, err)
	}
	if len(raw) > maxStoreBody {
		return nil, fmt.Errorf("codex: %s is larger than %d bytes", path, maxStoreBody)
	}
	var file storeFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("codex: %s is not a sign-in store: %w", path, err)
	}
	if file.Version != storeVersion {
		return nil, fmt.Errorf("codex: %s has store version %d; this polly reads version %d", path, file.Version, storeVersion)
	}
	if file.Codex == nil || file.Codex.AccessToken == "" {
		return nil, nil
	}
	return file.Codex, nil
}

// write replaces the file atomically: the directory is created private,
// the record lands in a temporary file that only its owner can read, and
// a rename puts it in place. The file's mode, not the directory's, is what
// keeps the tokens private, so a directory that already exists with a
// wider mode is left as it is.
func (s *Store) write(rec *record) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("codex: creating %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(storeFile{Version: storeVersion, Codex: rec}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".auth-*.json")
	if err != nil {
		return fmt.Errorf("codex: writing the sign-in: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("codex: writing the sign-in: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("codex: writing the sign-in: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("codex: writing the sign-in: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("codex: writing the sign-in: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("codex: writing the sign-in: %w", err)
	}
	s.cached, s.loaded = nil, false
	return nil
}
