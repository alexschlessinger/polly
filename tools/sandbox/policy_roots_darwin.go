//go:build darwin

package sandbox

// platformPrivatePolicyRoots lists the directories the Darwin backend denies
// to wrapped commands and the in-process policy therefore treats as private:
// the home directory. It is a variable so tests can model a root.
var platformPrivatePolicyRoots = func() []string {
	if home := resolvedHomeDir(); home != "" {
		return []string{home}
	}
	return nil
}
