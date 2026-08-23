//go:build linux

package sandbox

// gitHostWritablePath mirrors bubblewrap's private-root precedence. Exact
// /tmp, runtime temp, and /run grants stay private tmpfs mounts; a configured
// descendant or wider ancestor is bound later and therefore re-exposes host
// content that the Git policy must audit.
func gitHostWritablePath(path string) bool {
	tempRoots, runRoots := privateLinuxRoots()
	privateRoots := append(append([]string(nil), tempRoots...), runRoots...)
	return !pathEqualsAny(path, privateRoots)
}
