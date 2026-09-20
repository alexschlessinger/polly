//go:build linux

package sandbox

// platformPrivatePolicyRoots protects Polly storage regardless of home policy.
// It is a variable so tests can model a private root.
var platformPrivatePolicyRoots = func() []string { return traversablePrivateRoots() }
