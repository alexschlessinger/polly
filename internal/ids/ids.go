// Package ids generates the random hexadecimal identifiers shared by the
// swarm, workflow, and worktree runtimes.
package ids

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns 32 hexadecimal characters from 16 random bytes. A failing
// system random source is unrecoverable, so it panics rather than returning
// an identifier that could collide.
func New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("random identifier: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
