package sessions

import (
	"context"

	"github.com/alexschlessinger/pollytool/messages"
)

// TranscriptDisplayStore is an optional native display index, not a replacement
// for transcript paging. It selects real-user turn boundaries before reading
// payloads. Prefix facts contain only display dependencies: attachment references,
// tool identities/outcomes/change summaries and reasoning section boundaries.
// They are NOT messages suitable for replay, model input or transcript export.
// Every read is lease-free, replacement-epoch fenced and performs no repair.
// Epoch is required. Through (and PreviousUser's before) is always an exact
// exclusive boundary, including zero; unlike ReadTranscriptPage, zero never
// captures a newer end. Display pages are sparse: Next advances across omitted
// non-display messages too. Limit/MaxBytes and intact oversized facts follow
// TranscriptQuery's budgets. Returned nested values belong to the caller.
// Callers must also require NativeDisplay from TranscriptCapabilities.
type TranscriptDisplayStore interface {
	ReadTranscriptWindow(context.Context, ViewTarget, TranscriptWindowQuery) (TranscriptWindow, error)
	ReadTranscriptDisplayPage(context.Context, ViewTarget, TranscriptQuery) (TranscriptDisplayPage, error)
	ReadTranscriptPreviousUser(context.Context, ViewTarget, string, int64) (*TranscriptDisplayUser, error)
}

type TranscriptWindowQuery struct {
	Epoch   string
	Through int64
	Turns   int
}

type TranscriptWindow struct {
	ID, Epoch      string
	Start, Through int64
	// PreviousTurn is -1 when no real user precedes Start.
	PreviousTurn int64
}

// TranscriptDisplayFact retains exact identities and structural metadata, but
// not hidden assistant text, reasoning bodies, tool output or file diffs. Calls
// retain arguments. Image references (including legacy inline image bytes) stay
// intact: an attachment token must never silently change meaning.
type TranscriptDisplayFact struct {
	Sequence, Turn, Barrier, AnswerBarrier int64
	Message                                messages.ChatMessage
	// ContentHash identifies a tool's exact sentinel without retaining output.
	ContentHash string
}

type TranscriptDisplayPage struct {
	ID, Epoch            string
	First, Next, Through int64
	Facts                []TranscriptDisplayFact
	Oversized            bool
}

// TranscriptDisplayUser is a preceding real user's display input. Imported file
// bodies are omitted, but draft text, labels and composer metadata remain exact.
// Consumers walk backwards only when context imports/report notices require an
// earlier assignment; no fixed cap may silently drop that assignment.
type TranscriptDisplayUser struct {
	Sequence int64
	Message  messages.ChatMessage
}

// TranscriptDisplay is an explicitly partial display snapshot. History in a
// SessionView carrying this field is only [Start, Sequence); Prefix must never
// be replayed as visible transcript messages.
type TranscriptDisplay struct {
	Start           int64
	Previous        *TranscriptDisplayUser
	HasPreviousTurn bool
	Prefix          []TranscriptDisplayFact
}
