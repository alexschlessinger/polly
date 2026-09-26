package llm

import (
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// Login is a sign-in a provider draws its credential from at request time,
// as a provider served on a ChatGPT plan does; Credential and Account
// describe it. ErrNotSignedIn is the refusal when the sign-in is missing.
type (
	Login      = contract.Login
	Credential = contract.Credential
	Account    = contract.Account
)

var ErrNotSignedIn = contract.ErrNotSignedIn

// WithLogin gives the provider named by prefix its sign-in. A provider
// that needs one refuses requests until it is given.
func WithLogin(provider string, login Login) ClientOption {
	return func(c *clientConfig) {
		if c.logins == nil {
			c.logins = map[string]Login{}
		}
		c.logins[strings.ToLower(strings.TrimSpace(provider))] = login
	}
}

// ProviderRequiresLogin reports whether model's provider is served on a
// sign-in rather than an API key.
func ProviderRequiresLogin(model string) bool {
	provider, _, _ := strings.Cut(model, "/")
	return providerFor(provider).signIn
}

// LoginRequired reports whether a request for model would be refused for
// lack of a sign-in: its provider needs one and none is in place.
func (m *MultiPass) LoginRequired(model string) bool {
	provider, _, ok := strings.Cut(model, "/")
	if !ok {
		return false
	}
	spec := m.providers[strings.ToLower(provider)]
	return spec.signIn && !spec.signedIn()
}

// Account describes the sign-in a provider is using, without revealing
// credential material; ok is false when the provider has none or needs
// none.
func (m *MultiPass) Account(provider string) (Account, bool) {
	spec := m.providers[strings.ToLower(strings.TrimSpace(provider))]
	if spec.login == nil {
		return Account{}, false
	}
	return spec.login.Account()
}
