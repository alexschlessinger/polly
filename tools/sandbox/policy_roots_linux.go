//go:build linux

package sandbox

// platformPrivatePolicyRoots lists the directories the Linux backend hides
// from wrapped commands and the in-process policy therefore treats as
// private: the home directory and the runtime scratch root, which the temp
// root already hides from wrapped commands but not from in-process reads. It
// is a variable so tests can model a root.
var platformPrivatePolicyRoots = func() []string {
	roots := traversablePrivateRoots()
	if home := resolvedHomeDir(); home != "" {
		roots = append(roots, home)
	}
	return roots
}
