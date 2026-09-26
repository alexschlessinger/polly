package codex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// fakeIssuer stands in for auth.openai.com: it answers the token endpoint
// and the device endpoints, records every form it was sent, and mints
// unsigned JWTs for the account the test names.
type fakeIssuer struct {
	*httptest.Server
	mu    sync.Mutex
	forms []url.Values
	// respond, when set, answers a token request itself; ok false falls
	// through to the default grant.
	respond func(form url.Values) (status int, body string, ok bool)
	// pending is how many device polls answer 404 before the grant; polls
	// counts them all. pollRespond, when set, answers a poll itself first;
	// userCodeStatus, when set, is the status the user-code request gets.
	pending, polls int
	pollRespond    func() (status int, body string, ok bool)
	userCodeStatus int
	// issued numbers the grants so refresh tokens are distinct.
	issued int

	account, email, plan string
	lifetime             time.Duration
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	f := &fakeIssuer{account: "acct_1", email: "user@example.com", plan: "plus", lifetime: time.Hour}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", f.handleToken)
	mux.HandleFunc("POST "+deviceUserCodePath, f.handleUserCode)
	mux.HandleFunc("POST "+deviceTokenPath, f.handleDevicePoll)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeIssuer) oauth() *oauthClient { return newOAuthClient(f.URL, f.Client()) }

// tokens mints a fresh grant for the configured account.
func (f *fakeIssuer) tokens() Tokens {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokensLocked()
}

func (f *fakeIssuer) tokensLocked() Tokens {
	f.issued++
	auth := map[string]any{"chatgpt_account_id": f.account, "chatgpt_plan_type": f.plan, "chatgpt_user_id": "user_1"}
	return Tokens{
		IDToken:      unsignedJWT(map[string]any{"email": f.email, authClaim: auth}),
		AccessToken:  unsignedJWT(map[string]any{"exp": time.Now().Add(f.lifetime).Unix(), "jti": fmt.Sprintf("grant-%d", f.issued), authClaim: auth}),
		RefreshToken: fmt.Sprintf("refresh-%d", f.issued),
		ExpiresIn:    int(f.lifetime / time.Second),
	}
}

// lastForm is the most recent token request.
func (f *fakeIssuer) lastForm() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.forms) == 0 {
		return nil
	}
	return f.forms[len(f.forms)-1]
}

func (f *fakeIssuer) tokenCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.forms)
}

func (f *fakeIssuer) handleToken(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		http.Error(w, `{"error":"invalid_request","error_description":"form expected"}`, http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forms = append(f.forms, r.PostForm)
	if f.respond != nil {
		if status, body, ok := f.respond(r.PostForm); ok {
			w.WriteHeader(status)
			fmt.Fprint(w, body)
			return
		}
	}
	form := r.PostForm
	if form.Get("client_id") != ClientID {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	switch form.Get("grant_type") {
	case "authorization_code":
		if form.Get("code") == "" || form.Get("redirect_uri") == "" || form.Get("code_verifier") == "" {
			http.Error(w, `{"error":"invalid_request","error_description":"code, redirect_uri and code_verifier are required"}`, http.StatusBadRequest)
			return
		}
	case "refresh_token":
		if form.Get("refresh_token") == "" {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(f.tokensLocked())
}

func (f *fakeIssuer) handleUserCode(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if json.NewDecoder(r.Body).Decode(&body) != nil || body["client_id"] != ClientID {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	status := f.userCodeStatus
	f.mu.Unlock()
	if status != 0 {
		http.Error(w, `{"error":"device_auth_unavailable"}`, status)
		return
	}
	fmt.Fprint(w, `{"device_auth_id":"dev-1","user_code":"ABCD-EFGH","interval":0}`)
}

func (f *fakeIssuer) handleDevicePoll(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if json.NewDecoder(r.Body).Decode(&body) != nil || body["device_auth_id"] != "dev-1" || body["user_code"] != "ABCD-EFGH" {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if f.pollRespond != nil {
		if status, body, ok := f.pollRespond(); ok {
			w.WriteHeader(status)
			fmt.Fprint(w, body)
			return
		}
	}
	if f.polls <= f.pending {
		http.Error(w, "not yet", http.StatusNotFound)
		return
	}
	fmt.Fprint(w, `{"authorization_code":"dev-code","code_verifier":"dev-verifier","code_challenge":"dev-challenge"}`)
}

// unsignedJWT encodes payload as a JWT with an empty signature.
func unsignedJWT(payload map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	body, _ := json.Marshal(payload)
	enc := base64.RawURLEncoding.EncodeToString
	return enc(header) + "." + enc(body) + "."
}
