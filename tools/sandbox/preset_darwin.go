//go:build darwin

package sandbox

// Seatbelt writable path rules always refer to the host filesystem, including
// the default temp grants, so every final configured root is relevant to the
// Git persistence audit.
func gitHostWritablePath(string) bool {
	return true
}
