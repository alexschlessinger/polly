package codex

import (
	"net/http"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// The backend reads three things off every request besides the bearer
// token: which ChatGPT account it is for, which client sent it, and which
// session it belongs to for prompt-cache affinity.
const (
	accountHeader    = "chatgpt-account-id"
	originatorHeader = "originator"
	sessionHeader    = "session-id"
)

// setHeaders signs h for cred. sessionID is omitted when empty; accept is
// the response type the caller wants.
func setHeaders(h http.Header, cred contract.Credential, sessionID, accept string) {
	h.Set("Authorization", "Bearer "+cred.AccessToken)
	h.Set(accountHeader, cred.AccountID)
	h.Set(originatorHeader, Originator)
	h.Set("User-Agent", userAgent())
	if sessionID != "" {
		h.Set(sessionHeader, sessionID)
	}
	if accept != "" {
		h.Set("Accept", accept)
	}
}
