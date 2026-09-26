package codex

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// authClaim is the claim object under which OpenAI's tokens carry the
// ChatGPT account.
const authClaim = "https://api.openai.com/auth"

// claims is what this package reads out of a token: the ChatGPT account it
// belongs to, the plan that account is on, and when the token expires. The
// signature is not checked; the tokens come straight from the authority
// over TLS and are only ever sent back to OpenAI.
type claims struct {
	AccountID string
	PlanType  string
	UserID    string
	Email     string
	Expiry    time.Time
}

// parseClaims decodes the payload of a JWT.
func parseClaims(token string) (claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims{}, errors.New("codex: token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return claims{}, fmt.Errorf("codex: decoding token claims: %w", err)
	}
	var raw struct {
		Exp   int64  `json:"exp"`
		Email string `json:"email"`
		Auth  struct {
			AccountID string `json:"chatgpt_account_id"`
			PlanType  string `json:"chatgpt_plan_type"`
			UserID    string `json:"chatgpt_user_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return claims{}, fmt.Errorf("codex: decoding token claims: %w", err)
	}
	c := claims{AccountID: raw.Auth.AccountID, PlanType: raw.Auth.PlanType, UserID: raw.Auth.UserID, Email: raw.Email}
	if raw.Exp > 0 {
		// UTC, so the expiry compares equal to itself after a trip through
		// the store file whatever zone the process runs in.
		c.Expiry = time.Unix(raw.Exp, 0).UTC()
	}
	return c, nil
}

// claims reads the account out of a grant: the access token is
// authoritative for the account and expiry, the ID token fills in the
// email and whatever the access token left out. A grant that names no
// account cannot reach the backend and is an error.
func (t Tokens) claims(now time.Time) (claims, error) {
	c, err := parseClaims(t.AccessToken)
	if err != nil {
		return claims{}, err
	}
	if t.IDToken != "" {
		if id, err := parseClaims(t.IDToken); err == nil {
			if c.Email == "" {
				c.Email = id.Email
			}
			if c.AccountID == "" {
				c.AccountID = id.AccountID
			}
			if c.PlanType == "" {
				c.PlanType = id.PlanType
			}
		}
	}
	if c.Expiry.IsZero() && t.ExpiresIn > 0 {
		c.Expiry = now.Add(time.Duration(t.ExpiresIn) * time.Second).UTC()
	}
	if c.AccountID == "" {
		return claims{}, errors.New("codex: the sign-in names no ChatGPT account")
	}
	return c, nil
}
