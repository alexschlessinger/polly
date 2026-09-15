//go:build !linux && !darwin

package sandbox

// platformPrivatePolicyRoots lists the directories the platform backend hides
// from wrapped commands and the in-process policy therefore treats as
// private. It is a variable so tests can model a private root.
var platformPrivatePolicyRoots = func() []string { return nil }
