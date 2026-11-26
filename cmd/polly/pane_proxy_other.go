//go:build !unix

package main

import "errors"

// Panes need a terminal relayed over a Unix socket with resize signals, which
// only the Unix build has; the owner never opens one here.
const paneProxySupported = false

func runPaneProxy(string, string) error {
	return errors.New("polly pane is not supported on this platform")
}

// ownTerminal is never asked here: no host is used without the proxy.
func ownTerminal(string) bool { return true }
