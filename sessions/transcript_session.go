package sessions

import "context"

// SessionTranscriptReader is optional, read-only paging through the supplied
// session handle. It does not discover or reopen a backend. Like store paging,
// reads neither acquire leases nor update metadata; the stable session identity
// remains the target even after a rename. Wrappers must explicitly forward this
// capability to opt in. Callers without it can use Session.GetHistory instead.
// Query, ownership, epoch and consistency semantics match TranscriptPageStore.
type SessionTranscriptReader interface {
	ReadTranscriptPage(context.Context, TranscriptQuery) (TranscriptPage, error)
}

func (s *sqliteSession) ReadTranscriptPage(ctx context.Context, q TranscriptQuery) (TranscriptPage, error) {
	return s.store.ReadTranscriptPage(ctx, ViewTarget{ID: s.ViewID()}, q)
}
