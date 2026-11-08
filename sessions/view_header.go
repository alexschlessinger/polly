package sessions

import (
	"context"

	"github.com/alexschlessinger/pollytool/artifacts"
)

// SessionViewHeader is one atomic, lease-free session metadata snapshot, with no
// transcript payload. Epoch is the same replacement-only token returned by the
// backend's TranscriptPageStore. Use ID, Epoch, Sequence as the target, epoch and
// captured Through boundary of subsequent transcript reads; replacement/deletion
// must fail rather than combine histories. Concurrent append does not change Epoch.
// Metadata and its nested values belong to the caller. Artifacts is read-only,
// stable-ID-bound and usable until the store closes, subject to current ownership.
// Matching knownRevision sets Unchanged but still returns all header fields.
// Revision and Epoch are equality tokens, never mutation authority.
type SessionViewHeader struct {
	ID, Revision, Epoch string
	Sequence            int64
	ParentID            string
	Metadata            *Metadata
	Artifacts           artifacts.Store
	InUse, Unchanged    bool
}

// ViewHeaderStore is optional. Reads acquire no lease, touch no LastUsed and
// repair nothing. Hosts use this bounded metadata path only when the same supplied
// backend also implements TranscriptPageStore with matching epoch semantics.
// A legacy ViewStore remains authoritative; no hidden SQLite access or synthesized
// header/epoch adapter is supplied.
type ViewHeaderStore interface {
	ReadViewHeader(context.Context, ViewTarget, string) (*SessionViewHeader, error)
}
