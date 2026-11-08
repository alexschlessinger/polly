package sessions

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/alexschlessinger/pollytool/messages"
)

// RecordKey identifies a record within one coordination family. IDs are opaque
// and ordered bytewise, except mail/publications (Posted, ID) and workflow steps
// (report ID, numeric step). Unknown kinds remain round-trippable.
type RecordKey struct{ Kind, ID string }

type CoordinationIndex string

type RevisionScope struct {
	Index CoordinationIndex
	Key   string
}

// Revision tokens are opaque equality tokens, not authority or lease fences.
// DurableRevisions must be true before retaining them across operations/processes.
// Native tokens bind the family incarnation and format revision; either changing
// invalidates every token even when an individual scope's facts did not change.
type Revision struct {
	Scope RevisionScope
	Token string
}

type CoordinationIdentity struct {
	ActorID, ParentID string
	Sequence          int64
}

type CoordinationRecord struct {
	Key      RecordKey
	Payload  json.RawMessage
	Revision Revision
}

// RecordQuery selects one declared index partition, optionally restricted to
// Kind. The zero Index means CoordinationIndexRecords. Limit must be positive;
// Offset and MaxBytes must be nonnegative. After is an opaque, selector-bound
// keyset cursor; do not combine it with Offset. Pages in different callbacks
// have live semantics: concurrent changes can move records across the cursor.
// MaxBytes counts payload bytes only; zero means no byte budget.
// CountOnly requires IncludeTotal and zero Limit, Offset, After and MaxBytes;
// it returns Total with no records or continuation. IncludeLast requires CountOnly
// and also returns the final key under the same selector, kind filter and ordering
// as Total, in the callback's consistent view. Native indexed access does not fetch
// or decode payloads for CountOnly, including IncludeLast. Legacy access remains
// slow: it loads the supplied whole-family state and extracts the selected kind(s).
type RecordQuery struct {
	Kind          string
	Index         CoordinationIndex
	Key, After    string
	Offset, Limit int
	IncludeTotal  bool
	CountOnly     bool
	IncludeLast   bool
	MaxBytes      int
}

// RecordPage owns its payload buffers. Total is -1 unless IncludeTotal was
// requested and otherwise counts the partition before After/Offset. Next is
// empty at the end. An oversized first record is returned intact, alone.
// Last is an owned key, nil unless IncludeLast was requested and the partition
// is nonempty. It follows the query's declared ordering, not necessarily ID order.
type RecordPage struct {
	Records   []CoordinationRecord
	Next      string
	Total     int
	Last      *RecordKey
	Oversized bool
}

// CoordinationEffects joins all effects to the record transaction. Repeated
// Effects calls accumulate Append, Pins and Members in call order; nonempty
// PinOwner, ExpectedSequence and Transcript must agree across calls. Input
// buffers are copied. Sequence checks refer to the pre-append sequence;
// Transcript.Sequence is assigned by the backend at commit.
type CoordinationEffects struct {
	Append           []messages.ChatMessage
	ExpectedSequence *int64
	Transcript       *TranscriptIdentity
	Pins             []string
	PinOwner         string
	Members          []string
}

// CoordinationReader is callback-local and not safe for concurrent use. Its
// methods return owned bytes; missing Get returns found=false, not an error.
// Callers must not retain the reader or make nested calls into its store.
// Reads and revisions share one consistent view and honor the operation's
// context. Revisions accepts at most MaxCoordinationRevisionScopes entries.
// Identity.Sequence is the starting sequence, also after staging append effects.
type CoordinationReader interface {
	Identity() (CoordinationIdentity, error)
	Get(RecordKey) (CoordinationRecord, bool, error)
	Query(RecordQuery) (RecordPage, error)
	Revisions([]RevisionScope) ([]Revision, error)
}

// CoordinationTx reads its own staged record writes. Put copies payload bytes;
// Delete of a missing record is harmless. No method performs domain transitions.
type CoordinationTx interface {
	CoordinationReader
	Put(RecordKey, json.RawMessage) error
	Delete(RecordKey) error
	Effects(CoordinationEffects) error
}

// CoordinationCommit is authoritative only when UpdateRecords returns nil.
// Changed is the sorted, deduplicated set of invalidated record/index scopes,
// including both sides of partition moves. Effects alone can change Sequence
// without changing any record scope. Tokens never establish commit success.
type CoordinationCommit struct {
	Sequence int64
	Changed  []RevisionScope
}

// CoordinationRecords is optional; existing Session/CoordinationSession
// requirements are unchanged. Callbacks run at most once and are never retried.
// A callback error vetoes every effect. A backend error may represent an
// uncertain commit: inspect durable receipts rather than replaying callbacks.
type CoordinationRecords interface {
	ReadRecords(context.Context, func(CoordinationReader) error) error
	UpdateRecords(context.Context, func(CoordinationTx) error) (CoordinationCommit, error)
}

// CoordinationRecordsViewStore is trusted-host, root-only, lease-free access.
// It grants no mutation authority, touches no LastUsed and repairs nothing.
// A legacy view reports only the sequence supplied by ReadCoordinationView;
// old view backends may leave it zero. Do not use a display view for fencing.
type CoordinationRecordsViewStore interface {
	ReadRecordsView(context.Context, string, func(CoordinationReader) error) error
}

// CoordinationCapabilities distinguishes functional compatibility from indexed
// work and durable unchanged detection. Legacy access reports both flags false.
type CoordinationCapabilities struct {
	NativeIndexed    bool
	DurableRevisions bool
}

// CoordinationCapabilityReporter is optional. Absence means neither guarantee
// is advertised, even when a backend implements CoordinationRecords.
type CoordinationCapabilityReporter interface {
	CoordinationCapabilities() CoordinationCapabilities
}

type CoordinationAccess interface {
	CoordinationRecords
	Capabilities() CoordinationCapabilities
}

type CoordinationViewAccess interface {
	CoordinationRecordsViewStore
	Capabilities() CoordinationCapabilities
}

var (
	ErrCoordinationUnsupported = errors.New("coordination records are not supported")
	ErrCoordinationRequest     = errors.New("invalid coordination record request")
	ErrCoordinationClosed      = errors.New("coordination reader is outside its callback")
	ErrCoordinationCursor      = errors.New("invalid coordination record cursor")
)
