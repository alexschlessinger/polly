package codex

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

// noSleep records the intervals the poll asked for and returns at once.
func noSleep(intervals *[]time.Duration) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		*intervals = append(*intervals, d)
		return ctx.Err()
	}
}

func TestDeviceFlowPollsUntilGranted(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.pending = 2
	c := issuer.oauth()
	dc, err := c.requestDeviceCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dc.ID != "dev-1" || dc.UserCode != "ABCD-EFGH" {
		t.Fatalf("device code = %+v", dc)
	}
	var intervals []time.Duration
	grant, err := c.pollDeviceCode(context.Background(), dc, noSleep(&intervals))
	if err != nil {
		t.Fatal(err)
	}
	if grant.AuthorizationCode != "dev-code" || grant.CodeVerifier != "dev-verifier" {
		t.Fatalf("grant = %+v", grant)
	}
	if len(intervals) != 3 || intervals[0] != defaultDeviceInterval {
		t.Fatalf("sleeps = %v, want three default intervals", intervals)
	}
	tokens, err := c.redeemDevice(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	form := issuer.lastForm()
	if form.Get("code") != "dev-code" || form.Get("code_verifier") != "dev-verifier" || form.Get("redirect_uri") != issuer.URL+deviceRedirectPath {
		t.Fatalf("redeem form = %v", form)
	}
	if tokens.AccessToken == "" {
		t.Fatal("no tokens")
	}
}

func TestDeviceFlowHonorsIntervalAndSlowDown(t *testing.T) {
	issuer := newFakeIssuer(t)
	c := issuer.oauth()
	calls := 0
	issuer.pollRespond = func() (int, string, bool) {
		calls++
		if calls == 1 {
			return 400, `{"error":"slow_down"}`, true
		}
		return 0, "", false
	}
	var intervals []time.Duration
	grant, err := c.pollDeviceCode(context.Background(), deviceCode{ID: "dev-1", UserCode: "ABCD-EFGH", Interval: 2}, noSleep(&intervals))
	if err != nil {
		t.Fatal(err)
	}
	if grant.AuthorizationCode != "dev-code" {
		t.Fatalf("grant = %+v", grant)
	}
	if len(intervals) != 2 || intervals[0] != 2*time.Second || intervals[1] != 2*time.Second+defaultDeviceInterval {
		t.Fatalf("sleeps = %v, want the stated interval then a widened one", intervals)
	}
}

func TestDeviceFlowStopsWhenTheCodeExpires(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.pending = 1 << 30
	old := deviceTimeout
	deviceTimeout = 40 * time.Millisecond
	t.Cleanup(func() { deviceTimeout = old })
	_, err := issuer.oauth().pollDeviceCode(context.Background(), deviceCode{ID: "dev-1", UserCode: "ABCD-EFGH"}, func(ctx context.Context, _ time.Duration) error {
		return sleepFor(ctx, 5*time.Millisecond)
	})
	if !errors.Is(err, errDeviceExpired) {
		t.Fatalf("err = %v, want the code to expire", err)
	}
}

func TestDeviceFlowReportsCancellationAndRefusals(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.pending = 1 << 30
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := issuer.oauth().pollDeviceCode(ctx, deviceCode{ID: "dev-1", UserCode: "ABCD-EFGH"}, sleepFor)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the caller's cancellation", err)
	}

	issuer.pollRespond = func() (int, string, bool) {
		return 400, `{"error":"access_denied","error_description":"declined"}`, true
	}
	var intervals []time.Duration
	_, err = issuer.oauth().pollDeviceCode(context.Background(), deviceCode{ID: "dev-1", UserCode: "ABCD-EFGH"}, noSleep(&intervals))
	if err == nil || errors.Is(err, errDeviceExpired) || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v, want the refusal", err)
	}
}

func TestDeviceFlowReportsWhenDisabled(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.userCodeStatus = 403
	if _, err := issuer.oauth().requestDeviceCode(context.Background()); !errors.Is(err, ErrDeviceLoginUnavailable) {
		t.Fatalf("err = %v, want ErrDeviceLoginUnavailable", err)
	}
	issuer.userCodeStatus = 500
	if _, err := issuer.oauth().requestDeviceCode(context.Background()); err == nil || errors.Is(err, ErrDeviceLoginUnavailable) {
		t.Fatalf("err = %v, want a plain failure", err)
	}
}

func TestDeviceRedirectURIFollowsTheIssuer(t *testing.T) {
	c := newOAuthClient(Issuer, nil)
	if got := c.issuer + deviceRedirectPath; got != "https://auth.openai.com/deviceauth/callback" {
		t.Fatalf("device redirect = %s", got)
	}
	if _, err := url.Parse(c.issuer + deviceVerifyPath); err != nil {
		t.Fatal(err)
	}
}
