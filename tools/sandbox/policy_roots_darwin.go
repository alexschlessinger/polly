//go:build darwin

package sandbox

// platformPrivatePolicyRoots lists the directories the Darwin backend denies
// to wrapped commands and the in-process policy therefore treats as private:
// the home directory and the runtime scratch root. It is a variable so tests
// can model a root.
var platformPrivatePolicyRoots = func() []string {
	roots := traversablePrivateRoots()
	if home := resolvedHomeDir(); home != "" {
		roots = append(roots, home)
	}
	return roots
}
