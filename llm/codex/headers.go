package codex

import (
	"net/http"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/openai"
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

// routingHint is the value the Codex client sends the backend for model:
// the model alone, or the model and the priority tier in fast mode. The
// backend's reply echoes its tier as "default" either way; the request is
// what decides, as the Codex client and pi both take it.
func routingHint(model string, fast bool) string {
	if fast {
		return "model=" + model + ";tier=" + openai.ServiceTierPriority
	}
	return "model=" + model
}

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
