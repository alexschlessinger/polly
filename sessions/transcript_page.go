package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/messages"
)

const (
	DefaultTranscriptPageBytes = 1 << 20
	MaxTranscriptPageBytes     = 16 << 20
)

var (
	ErrStaleTranscriptEpoch   = errors.New("stale transcript epoch")
	ErrInvalidTranscriptRange = errors.New("invalid transcript range")
)

// TranscriptQuery selects contiguous, zero-based message sequences. After is
// the inclusive next-message boundary; Through is the exclusive end. Zero
// Through captures the current end. Use the returned ID, Epoch, Next (as After)
// and Through for subsequent pages, stopping when Next == Through. Epoch empty
// starts a new read; a supplied epoch must match the target's current epoch.
// An epoch is an equality token, not authority, and is bound to stable identity.
// Limit must be in [1, MaxPageLimit]. MaxBytes zero uses
// DefaultTranscriptPageBytes; otherwise it must be in [1, MaxTranscriptPageBytes].
// The byte budget counts each selected message's JSON encoding, excluding page
// framing. A first message larger than the budget is returned intact, alone,
// with Oversized true; messages and media are never truncated to fit a budget.
type TranscriptQuery struct {
	Epoch           string
	After, Through  int64
	Limit, MaxBytes int
}

// TranscriptPage is an owned read-only snapshot of the requested range. First
// is the requested After boundary, Next is the first sequence not returned, and
// Through is the captured exclusive end. Transcript, when present, is the
// execution identity at the view's current committed boundary, not necessarily
// Next or Through; an unrelated append may leave an older identity.Sequence.
type TranscriptPage struct {
	ID, Epoch            string
	First, Next, Through int64
	Messages             []messages.ChatMessage
	Transcript           *TranscriptIdentity
	Oversized            bool
}

// TranscriptPageStore is optional. Native implementations select by sequence
// before decoding payloads; their durable epochs change on Clear/Reset, but not
// append, and survive reopen. A stale epoch returns ErrStaleTranscriptEpoch.
// An out-of-bounds/reversed range returns ErrInvalidTranscriptRange; a gap or
// corrupt selected payload fails rather than silently omitting a message.
// Reads take no lease, update no timestamps and perform no repairs. Returned
// messages, nested values and identity belong to the caller.
type TranscriptPageStore interface {
	ReadTranscriptPage(context.Context, ViewTarget, TranscriptQuery) (TranscriptPage, error)
}

type TranscriptCapabilities struct {
	NativeIndexed bool
	// NativeDisplay supplies indexed turn windows and compact display prefixes.
	NativeDisplay bool
	// StableEpoch distinguishes replacement-only native epochs from the legacy
	// fallback's conservative, any-view-change epochs.
	StableEpoch bool
}

// TranscriptCapabilityReporter optionally describes the supplied backend's
// implementation. Without a reporter, access claims neither indexed reads nor
// replacement-only epoch stability.
type TranscriptCapabilityReporter interface {
	TranscriptCapabilities() TranscriptCapabilities
}

// TranscriptAccess borrows its supplied store and owns no close operation.
type TranscriptAccess struct {
	TranscriptPageStore
	capabilities TranscriptCapabilities
}

func (a *TranscriptAccess) Capabilities() TranscriptCapabilities { return a.capabilities }

// OpenTranscriptAccess selects only the supplied store's capability. Without
// native paging it requires ViewStore; acquiring a session to read it would
// violate lease-free inspection, so a bare SessionStore fails explicitly.
//
// The functional legacy adapter reads a complete view on EVERY page. Its epoch
// binds stable identity and the view revision: append, settings changes and
// other view changes invalidate continuation too. This is deliberately slower
// and less stable than native replacement-only epochs, not a bounded fast path.
func OpenTranscriptAccess(store SessionStore) (*TranscriptAccess, error) {
	if store == nil {
		return nil, ErrPagingUnavailable
	}
	if native, ok := store.(TranscriptPageStore); ok {
		var capabilities TranscriptCapabilities
		if reporter, ok := store.(TranscriptCapabilityReporter); ok {
			capabilities = reporter.TranscriptCapabilities()
		}
		return &TranscriptAccess{TranscriptPageStore: native, capabilities: capabilities}, nil
	}
	view, ok := store.(ViewStore)
	if !ok {
		return nil, fmt.Errorf("%w: transcript paging requires ViewStore", ErrPagingUnavailable)
	}
	return &TranscriptAccess{TranscriptPageStore: &legacyTranscriptPages{store: view}}, nil
}

type legacyTranscriptPages struct{ store ViewStore }

func (s *legacyTranscriptPages) ReadTranscriptPage(ctx context.Context, target ViewTarget, q TranscriptQuery) (TranscriptPage, error) {
	if err := ctx.Err(); err != nil {
		return TranscriptPage{}, err
	}
	if err := validatePageLimit(q.Limit); err != nil {
		return TranscriptPage{}, err
	}
	if q.MaxBytes < 0 || q.MaxBytes > MaxTranscriptPageBytes {
		return TranscriptPage{}, fmt.Errorf("%w: invalid transcript byte limit", ErrInvalidPageRequest)
	}
	if q.MaxBytes == 0 {
		q.MaxBytes = DefaultTranscriptPageBytes
	}
	if q.After < 0 || q.Through < 0 || (q.Through != 0 && q.After > q.Through) {
		return TranscriptPage{}, ErrInvalidTranscriptRange
	}
	view, err := s.store.ReadView(ctx, target, "")
	if err != nil {
		return TranscriptPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return TranscriptPage{}, err
	}
	if view == nil || view.ID == "" || view.Revision == "" || view.Unchanged {
		return TranscriptPage{}, fmt.Errorf("%w: transcript requires a full identified view", ErrPagingUnavailable)
	}
	if view.Sequence != int64(len(view.History)) {
		return TranscriptPage{}, fmt.Errorf("session message sequence is corrupt: read %d messages, expected %d", len(view.History), view.Sequence)
	}
	// JSON tuple encoding avoids ambiguity even for backend-defined identities.
	encoded, err := json.Marshal([]string{"legacy-transcript-v1", view.ID, view.Revision})
	if err != nil {
		return TranscriptPage{}, fmt.Errorf("encode transcript epoch: %w", err)
	}
	hash := sha256.Sum256(encoded)
	epoch := "legacy-v1:" + hex.EncodeToString(hash[:])
	if q.Epoch != "" && q.Epoch != epoch {
		return TranscriptPage{}, ErrStaleTranscriptEpoch
	}
	through := q.Through
	if through == 0 {
		through = view.Sequence
	}
	if q.After > through || through > view.Sequence {
		return TranscriptPage{}, ErrInvalidTranscriptRange
	}
	page := TranscriptPage{ID: view.ID, Epoch: epoch, First: q.After, Next: q.After, Through: through}
	if view.Metadata != nil && view.Metadata.Transcript != nil {
		identity := *view.Metadata.Transcript
		page.Transcript = &identity
	}
	used := 0
	for sequence := q.After; sequence < through && len(page.Messages) < q.Limit; sequence++ {
		if err := ctx.Err(); err != nil {
			return TranscriptPage{}, err
		}
		data, err := json.Marshal(view.History[sequence])
		if err != nil {
			return TranscriptPage{}, fmt.Errorf("encode transcript message %d: %w", sequence, err)
		}
		if len(data) > q.MaxBytes-used && len(page.Messages) > 0 {
			break
		}
		// Clone the persisted representation, including nested metadata, tool
		// calls and media references, even if a backend retains its view buffers.
		var message messages.ChatMessage
		if err := json.Unmarshal(data, &message); err != nil {
			return TranscriptPage{}, fmt.Errorf("decode transcript message %d: %w", sequence, err)
		}
		page.Messages = append(page.Messages, message)
		page.Next = sequence + 1
		if len(data) > q.MaxBytes {
			page.Oversized = true
			break
		}
		used += len(data)
	}
	return page, nil
}
