package codex

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestParseClaimsReadsTheAuthObject(t *testing.T) {
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	token := unsignedJWT(map[string]any{
		"exp":   exp.Unix(),
		"email": "user@example.com",
		authClaim: map[string]any{
			"chatgpt_account_id": "acct_1",
			"chatgpt_plan_type":  "pro",
			"chatgpt_user_id":    "user_1",
		},
	})
	c, err := parseClaims(token)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccountID != "acct_1" || c.PlanType != "pro" || c.UserID != "user_1" || c.Email != "user@example.com" || !c.Expiry.Equal(exp) {
		t.Fatalf("claims = %+v", c)
	}
	// Padding some issuers add is tolerated.
	parts := strings.Split(token, ".")
	padded := parts[0] + "." + base64.URLEncoding.EncodeToString(mustDecode(parts[1])) + "."
	if c2, err := parseClaims(padded); err != nil || c2.AccountID != "acct_1" {
		t.Fatalf("padded token: %+v %v", c2, err)
	}
}

func mustDecode(s string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestParseClaimsRejectsMalformedTokens(t *testing.T) {
	for _, token := range []string{"", "abc", "a.b", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte("not json")) + ".c"} {
		if _, err := parseClaims(token); err == nil {
			t.Errorf("parseClaims(%q) accepted", token)
		}
	}
	// A JWT without the auth object is a valid token that names no account.
	c, err := parseClaims(unsignedJWT(map[string]any{"sub": "x"}))
	if err != nil || c.AccountID != "" || !c.Expiry.IsZero() {
		t.Fatalf("claims = %+v, err = %v", c, err)
	}
}

func TestTokensClaimsMergeTheIDTokenAndRequireAnAccount(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	auth := map[string]any{"chatgpt_account_id": "acct_1", "chatgpt_plan_type": "plus"}
	tokens := Tokens{
		AccessToken: unsignedJWT(map[string]any{authClaim: auth}),
		IDToken:     unsignedJWT(map[string]any{"email": "user@example.com", authClaim: auth}),
		ExpiresIn:   600,
	}
	c, err := tokens.claims(now)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccountID != "acct_1" || c.PlanType != "plus" || c.Email != "user@example.com" || !c.Expiry.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("claims = %+v", c)
	}
	// The access token's exp wins over expires_in.
	tokens.AccessToken = unsignedJWT(map[string]any{"exp": now.Add(time.Hour).Unix(), authClaim: auth})
	if c, err := tokens.claims(now); err != nil || !c.Expiry.Equal(now.Add(time.Hour)) {
		t.Fatalf("claims = %+v, err = %v", c, err)
	}
	// A bad ID token is ignored; a grant without an account is refused.
	tokens.IDToken = "garbage"
	if c, err := tokens.claims(now); err != nil || c.Email != "" || c.AccountID != "acct_1" {
		t.Fatalf("claims = %+v, err = %v", c, err)
	}
	tokens.AccessToken = unsignedJWT(map[string]any{"exp": now.Unix()})
	if _, err := tokens.claims(now); err == nil || !strings.Contains(err.Error(), "no ChatGPT account") {
		t.Fatalf("err = %v", err)
	}
}
