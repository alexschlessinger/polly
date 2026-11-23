package codex

import (
	"net/http"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// The backend reads four things off a completion besides the bearer
// token: which ChatGPT account it is for, which client sent it, which
// session it belongs to for prompt-cache affinity, and a routing hint
// naming the model and, in fast mode, the tier.
const (
	accountHeader     = "chatgpt-account-id"
	originatorHeader  = "originator"
	sessionHeader     = "session-id"
	routingHintHeader = "x-codex-routing-hint"
)

// routingHint uses the tier from the validated request body, if any.
func routingHint(model, tier string) string {
	if tier != "" {
		return "model=" + model + ";tier=" + tier
	}
	return "model=" + model
}

// setHeaders signs h for cred. sessionID is omitted when empty; accept is
// the response type the caller wants.
func setHeaders(h http.Header, cred contract.Credential, sessionID, accept string) {
	h.Set("Authorization", "Bearer "+cred.AccessToken)
	h.Set(accountHeader, cred.AccountID)
	h.Set(originatorHeader, Originator)
	if sessionID != "" {
		h.Set(sessionHeader, sessionID)
	}
	if accept != "" {
		h.Set("Accept", accept)
	}
}
