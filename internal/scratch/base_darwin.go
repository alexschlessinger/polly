package scratch

// Darwin's per-user TMPDIR already consumes most of sockaddr_un's 104-byte
// path. Adding a worker slot and an ordinary socket suffix cannot fit there.
// Keep the default under the short system temp directory; EnsureRoot still
// requires exclusive ownership before using this shared parent.
func defaultRoot() string { return RootUnder("/private/tmp") }
