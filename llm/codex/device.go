package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// The device flow is the authority's own, not RFC 8628: the client asks
// for a user code, the user types it on a web page, and the client polls
// until the authority hands over an authorization code with the PKCE pair
// it generated on the client's behalf.
const (
	deviceUserCodePath = "/api/accounts/deviceauth/usercode"
	deviceTokenPath    = "/api/accounts/deviceauth/token"
	deviceVerifyPath   = "/codex/device"
	deviceRedirectPath = "/deviceauth/callback"
	// defaultDeviceInterval is the poll spacing when the authority names
	// none.
	defaultDeviceInterval = 5 * time.Second
)

// deviceTimeout is how long a user code stays redeemable; tests shorten it.
var deviceTimeout = 15 * time.Minute

// ErrDeviceLoginUnavailable reports that the account or workspace has not
// enabled device sign-in.
var ErrDeviceLoginUnavailable = errors.New("codex: device sign-in is not enabled for this ChatGPT account")

// deviceCode is a pending device sign-in.
type deviceCode struct {
	ID       string `json:"device_auth_id"`
	UserCode string `json:"user_code"`
	Interval int    `json:"interval"`
}

// deviceGrant is what a completed device sign-in redeems: an authorization
// code and the PKCE pair the authority made for it.
type deviceGrant struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
	CodeChallenge     string `json:"code_challenge"`
}

// requestDeviceCode starts a device sign-in.
func (c *oauthClient) requestDeviceCode(ctx context.Context) (deviceCode, error) {
	body, status, err := c.postJSON(ctx, deviceUserCodePath, map[string]string{"client_id": ClientID})
	if err != nil {
		return deviceCode{}, fmt.Errorf("codex: starting device sign-in: %w", err)
	}
	if status == http.StatusForbidden {
		return deviceCode{}, ErrDeviceLoginUnavailable
	}
	if status/100 != 2 {
		return deviceCode{}, fmt.Errorf("codex: starting device sign-in: %w", newAuthError(status, body))
	}
	var dc deviceCode
	if err := json.Unmarshal(body, &dc); err != nil {
		return deviceCode{}, fmt.Errorf("codex: decoding the device code: %w", err)
	}
	if dc.ID == "" || dc.UserCode == "" {
		return deviceCode{}, errors.New("codex: the authority issued no device code")
	}
	return dc, nil
}

// pollDeviceCode waits for the user to approve dc, asking the authority
// every interval through sleep, until the grant arrives, the user code
// expires, or ctx ends. A pending answer is HTTP 403 or 404, or an OAuth
// error that says so; slow_down widens the interval.
func (c *oauthClient) pollDeviceCode(ctx context.Context, dc deviceCode, sleep func(context.Context, time.Duration) error) (deviceGrant, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, deviceTimeout, errDeviceExpired)
	defer cancel()
	interval := time.Duration(dc.Interval) * time.Second
	if interval <= 0 {
		interval = defaultDeviceInterval
	}
	request := map[string]string{"device_auth_id": dc.ID, "user_code": dc.UserCode}
	for {
		if err := sleep(ctx, interval); err != nil {
			return deviceGrant{}, deviceWaitError(ctx, err)
		}
		body, status, err := c.postJSON(ctx, deviceTokenPath, request)
		if err != nil {
			return deviceGrant{}, deviceWaitError(ctx, fmt.Errorf("codex: waiting for device sign-in: %w", err))
		}
		switch {
		case status/100 == 2:
			var grant deviceGrant
			if err := json.Unmarshal(body, &grant); err != nil {
				return deviceGrant{}, fmt.Errorf("codex: decoding the device grant: %w", err)
			}
			if grant.AuthorizationCode == "" {
				return deviceGrant{}, errors.New("codex: the device grant carries no authorization code")
			}
			return grant, nil
		case status == http.StatusForbidden, status == http.StatusNotFound:
			continue
		}
		ae := newAuthError(status, body)
		switch ae.code {
		case "authorization_pending", "deviceauth_authorization_pending":
			continue
		case "slow_down":
			interval += defaultDeviceInterval
			continue
		}
		return deviceGrant{}, fmt.Errorf("codex: device sign-in refused: %w", ae)
	}
}

var errDeviceExpired = errors.New("codex: the device code expired before it was approved")

// deviceWaitError reports why the wait ended: the code expiring, or the
// caller's own cancellation.
func deviceWaitError(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); errors.Is(cause, errDeviceExpired) {
		return errDeviceExpired
	}
	return err
}

// redeemDevice trades an approved device grant for tokens.
func (c *oauthClient) redeemDevice(ctx context.Context, grant deviceGrant) (Tokens, error) {
	return c.exchange(ctx, grant.AuthorizationCode, c.issuer+deviceRedirectPath, grant.CodeVerifier)
}

// postJSON posts a JSON body to an authority path.
func (c *oauthClient) postJSON(ctx context.Context, path string, payload any) ([]byte, int, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.issuer+path, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return c.do(req)
}

// sleepFor is the production sleep: a timer that ctx can cut short.
func sleepFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
