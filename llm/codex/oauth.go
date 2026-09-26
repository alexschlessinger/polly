// Package codex signs a user in with their ChatGPT account and sends
// completions to OpenAI's Codex backend on the plan that account carries,
// the way the Codex CLI does. Everything on the wire follows that client:
// its public OAuth client, its loopback callback ports, its stateless
// Responses body, and the account header on every request. The one thing
// this package never copies is its name: requests identify as polly.
package codex

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	// Issuer is OpenAI's account authority.
	Issuer = "https://auth.openai.com"
	// ClientID is the Codex CLI's public OAuth client: the one registration
	// the authority lets onto the Codex backend with a ChatGPT plan.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// Originator names this client to the authority and the backend.
	Originator = "polly"

	scopes       = "openid profile email offline_access"
	callbackPath = "/auth/callback"
	// maxTokenBody bounds an authority response.
	maxTokenBody = 1 << 20
)

// loginPorts are the loopback ports the authority's redirect allow-list
// names, tried in order; tests bind an ephemeral port instead.
var loginPorts = []int{1455, 1457}

// ErrRefreshRevoked reports a refresh token the authority no longer honors:
// expired, revoked, or rotated by another refresh. Only a new sign-in
// recovers from it.
var ErrRefreshRevoked = errors.New("codex: the sign-in is no longer valid")

// pkce is one Proof Key for Code Exchange pair: the secret the client keeps
// and the S256 challenge it sends ahead.
type pkce struct{ verifier, challenge string }

func newPKCE() (pkce, error) {
	verifier, err := randomToken(32)
	if err != nil {
		return pkce{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	return pkce{verifier: verifier, challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// randomToken returns n random bytes as unpadded base64url.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("codex: generating a random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Tokens is what the authority issues for a grant. ExpiresIn is the access
// token's lifetime in seconds when the authority states one; the token's
// own exp claim is authoritative.
type Tokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// oauthClient speaks to one authority.
type oauthClient struct {
	issuer string
	client *http.Client
}

func newOAuthClient(issuer string, client *http.Client) *oauthClient {
	if client == nil {
		client = &http.Client{}
	}
	return &oauthClient{issuer: strings.TrimRight(issuer, "/"), client: client}
}

// authorizeURL is the page the user signs in on: the authorization-code
// request with PKCE, plus the Codex flags the authority expects from a
// terminal client.
func (c *oauthClient) authorizeURL(redirectURI, state string, p pkce) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", scopes)
	q.Set("code_challenge", p.challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("originator", Originator)
	return c.issuer + "/oauth/authorize?" + q.Encode()
}

// exchange redeems an authorization code.
func (c *oauthClient) exchange(ctx context.Context, code, redirectURI, verifier string) (Tokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	tokens, err := c.token(ctx, form)
	if err != nil {
		return Tokens{}, fmt.Errorf("codex: redeeming the sign-in: %w", err)
	}
	return tokens, nil
}

// refresh trades a refresh token for fresh tokens. The authority rotates
// the refresh token on every success; a code that says the old one is dead
// for good surfaces as ErrRefreshRevoked, anything else as a transient
// failure the caller may retry later.
func (c *oauthClient) refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {refreshToken},
	}
	tokens, err := c.token(ctx, form)
	if err != nil {
		var ae *authError
		if errors.As(err, &ae) && ae.permanent() {
			return Tokens{}, fmt.Errorf("%w (%s)", ErrRefreshRevoked, ae.code)
		}
		return Tokens{}, fmt.Errorf("codex: refreshing the sign-in: %w", err)
	}
	return tokens, nil
}

// token posts a form to the token endpoint and decodes the grant.
func (c *oauthClient) token(ctx context.Context, form url.Values) (Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.issuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	body, status, err := c.do(req)
	if err != nil {
		return Tokens{}, err
	}
	if status/100 != 2 {
		return Tokens{}, newAuthError(status, body)
	}
	var tokens Tokens
	if err := json.Unmarshal(body, &tokens); err != nil {
		return Tokens{}, fmt.Errorf("decoding the token response: %w", err)
	}
	if tokens.AccessToken == "" {
		return Tokens{}, errors.New("the token response carries no access token")
	}
	return tokens, nil
}

// do sends req and returns the bounded body with its status.
func (c *oauthClient) do(req *http.Request) ([]byte, int, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading the authority response: %w", err)
	}
	return body, resp.StatusCode, nil
}

// authError is a non-2xx answer from the authority: the OAuth error code
// and description when the body carries them, else the raw text.
type authError struct {
	status      int
	code        string
	description string
}

func newAuthError(status int, body []byte) *authError {
	e := &authError{status: status}
	var envelope struct {
		Error       json.RawMessage `json:"error"`
		Description string          `json:"error_description"`
	}
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Error) > 0 {
		e.description = envelope.Description
		var code string
		if json.Unmarshal(envelope.Error, &code) == nil {
			e.code = code
		} else {
			var obj struct {
				Code    string `json:"code"`
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			if json.Unmarshal(envelope.Error, &obj) == nil {
				e.code = obj.Code
				if e.code == "" {
					e.code = obj.Type
				}
				if e.description == "" {
					e.description = obj.Message
				}
			}
		}
	}
	if e.code == "" && e.description == "" {
		e.description = strings.TrimSpace(string(body))
	}
	return e
}

func (e *authError) Error() string {
	switch {
	case e.code != "" && e.description != "":
		return fmt.Sprintf("HTTP %d %s: %s", e.status, e.code, e.description)
	case e.code != "":
		return fmt.Sprintf("HTTP %d %s", e.status, e.code)
	case e.description != "":
		return fmt.Sprintf("HTTP %d: %s", e.status, e.description)
	}
	return fmt.Sprintf("HTTP %d", e.status)
}

// permanent reports a code that says the grant is dead for good.
func (e *authError) permanent() bool {
	switch e.code {
	case "invalid_grant", "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated":
		return true
	}
	return false
}
