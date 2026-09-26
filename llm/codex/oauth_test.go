package codex

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestAuthorizeURLCarriesPKCEAndCodexFlags(t *testing.T) {
	p, err := newPKCE()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(p.verifier))
	if p.challenge != base64.RawURLEncoding.EncodeToString(sum[:]) || len(p.verifier) < 43 {
		t.Fatalf("pkce = %+v", p)
	}
	c := newOAuthClient(Issuer+"/", nil)
	u, err := url.Parse(c.authorizeURL("http://localhost:1455/auth/callback", "state-1", p))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "auth.openai.com" || u.Path != "/oauth/authorize" {
		t.Fatalf("authorize endpoint = %s", u)
	}
	want := map[string]string{
		"response_type":              "code",
		"client_id":                  ClientID,
		"redirect_uri":               "http://localhost:1455/auth/callback",
		"scope":                      "openid profile email offline_access",
		"code_challenge":             p.challenge,
		"code_challenge_method":      "S256",
		"state":                      "state-1",
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
		"originator":                 "polly",
	}
	q := u.Query()
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q) != len(want) {
		t.Errorf("query = %v, want exactly %d parameters", q, len(want))
	}
}

func TestExchangePostsTheCodeAsAForm(t *testing.T) {
	issuer := newFakeIssuer(t)
	tokens, err := issuer.oauth().exchange(context.Background(), "code-1", "http://localhost:1455/auth/callback", "verifier-1")
	if err != nil {
		t.Fatal(err)
	}
	form := issuer.lastForm()
	want := map[string]string{"grant_type": "authorization_code", "client_id": ClientID, "code": "code-1", "redirect_uri": "http://localhost:1455/auth/callback", "code_verifier": "verifier-1"}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, form.Get(k), v)
		}
	}
	if tokens.AccessToken == "" || tokens.RefreshToken != "refresh-1" || tokens.IDToken == "" || tokens.ExpiresIn != 3600 {
		t.Fatalf("tokens = %+v", tokens)
	}
}

func TestRefreshRotatesAndClassifiesAuthorityErrors(t *testing.T) {
	issuer := newFakeIssuer(t)
	tokens, err := issuer.oauth().refresh(context.Background(), "refresh-0")
	if err != nil {
		t.Fatal(err)
	}
	form := issuer.lastForm()
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "refresh-0" || form.Get("client_id") != ClientID {
		t.Fatalf("refresh form = %v", form)
	}
	if tokens.RefreshToken != "refresh-1" {
		t.Fatalf("refresh token not rotated: %+v", tokens)
	}

	for _, tc := range []struct {
		name    string
		status  int
		body    string
		revoked bool
		text    string
	}{
		{"invalid_grant", 400, `{"error":"invalid_grant","error_description":"expired"}`, true, "invalid_grant"},
		{"reused", 400, `{"error":"refresh_token_reused"}`, true, "refresh_token_reused"},
		{"object code", 401, `{"error":{"code":"refresh_token_expired","message":"gone"}}`, true, "refresh_token_expired"},
		{"server error", 500, `{"error":"server_error","error_description":"try later"}`, false, "try later"},
		{"plain text", 502, `bad gateway`, false, "HTTP 502: bad gateway"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer.respond = func(url.Values) (int, string, bool) { return tc.status, tc.body, true }
			_, err := issuer.oauth().refresh(context.Background(), "refresh-x")
			if err == nil {
				t.Fatal("refresh succeeded")
			}
			if errors.Is(err, ErrRefreshRevoked) != tc.revoked {
				t.Fatalf("revoked = %v for %v, want %v", !tc.revoked, err, tc.revoked)
			}
			if !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("error %q does not mention %q", err, tc.text)
			}
		})
	}
}

func TestTokenEndpointMustIssueAnAccessToken(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.respond = func(url.Values) (int, string, bool) { return 200, `{"refresh_token":"only"}`, true }
	if _, err := issuer.oauth().exchange(context.Background(), "c", "r", "v"); err == nil || !strings.Contains(err.Error(), "no access token") {
		t.Fatalf("err = %v", err)
	}
}
