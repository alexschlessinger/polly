package sessions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
)

// MaxPageLimit is the largest summary or transcript page. Limits are required:
// zero is not an unbounded read and is rejected, as are negative limits.
const MaxPageLimit = 1000

var (
	ErrInvalidPageRequest   = errors.New("invalid session page request")
	ErrInvalidSummaryCursor = errors.New("invalid summary cursor")
	ErrPagingUnavailable    = errors.New("session paging capability unavailable")
)

// SummaryQuery selects a live page ordered by LastUsed descending, then name
// and stable ID ascending (bytewise). ParentID nil selects all visible sessions;
// a pointer to an empty string selects roots, otherwise it selects direct children
// of that stable ID. After is an opaque, filter/order/version-bound continuation.
// Limit must be in [1, MaxPageLimit]; it may change between pages.
//
// Pages do not share a transaction. Renames, last-used updates, deletion and
// parent changes may move entries across the cursor, causing skips or repeats.
// Callers needing a fixed export must use a snapshot/full-result API instead.
// Metadata, including an arbitrarily large selected item, is never truncated.
// Visibility follows the backend's lease/pin-aware session visibility rules.
// Reads neither acquire leases, touch last-used time nor repair stored state.
type SummaryQuery struct {
	ParentID *string
	After    string
	Limit    int
}

type SummaryPage struct {
	Summaries []SessionSummary
	// Next is empty at the end of the current live result set.
	Next string
}

// SummaryPageStore is optional; SessionStore requirements remain unchanged.
// Native indexed implementations select before decoding unrelated metadata.
// All implementations return independently owned metadata. GetSummary uses a
// stable identity, not a name, and returns ErrSessionNotFound for deleted or
// invisible identities.
type SummaryPageStore interface {
	ListSummaryPage(context.Context, SummaryQuery) (SummaryPage, error)
	GetSummary(context.Context, string) (*SessionSummary, error)
}

// SummaryCapabilities distinguishes bounded native selection from a functional
// but slower compatibility adapter that loads and sorts all ListSummaries rows.
type SummaryCapabilities struct{ NativeIndexed bool }

// SummaryCapabilityReporter optionally describes the supplied backend's paging
// implementation. Without a reporter, access makes no indexed-efficiency claim.
type SummaryCapabilityReporter interface {
	SummaryCapabilities() SummaryCapabilities
}

// SummaryAccess borrows its supplied store; it owns no lease or close operation.
type SummaryAccess struct {
	SummaryPageStore
	capabilities SummaryCapabilities
}

func (a *SummaryAccess) Capabilities() SummaryCapabilities { return a.capabilities }

// OpenSummaryAccess selects only the supplied backend. It never discovers a
// database path or substitutes SQLite for a host's store. The fallback retains
// the supplied store's visibility/error semantics and materializes its whole
// ListSummaries result on every call, including stable-ID lookup.
func OpenSummaryAccess(store SessionStore) (*SummaryAccess, error) {
	if store == nil {
		return nil, ErrPagingUnavailable
	}
	if native, ok := store.(SummaryPageStore); ok {
		var capabilities SummaryCapabilities
		if reporter, ok := store.(SummaryCapabilityReporter); ok {
			capabilities = reporter.SummaryCapabilities()
		}
		return &SummaryAccess{SummaryPageStore: native, capabilities: capabilities}, nil
	}
	return &SummaryAccess{SummaryPageStore: &legacySummaryPages{store: store}}, nil
}

type legacySummaryPages struct{ store SessionStore }

const summaryPageOrder = "last-used-desc/name-asc/id-asc"

type summaryCursor struct {
	Version  int       `json:"v"`
	Order    string    `json:"order"`
	ParentID *string   `json:"parent"`
	LastUsed time.Time `json:"used"`
	Name     string    `json:"name"`
	ID       string    `json:"id"`
}

func validatePageLimit(limit int) error {
	if limit < 1 || limit > MaxPageLimit {
		return fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidPageRequest, MaxPageLimit)
	}
	return nil
}

func decodeSummaryCursor(q SummaryQuery) (*summaryCursor, error) {
	if q.After == "" {
		return nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(q.After)
	if err != nil {
		return nil, fmt.Errorf("%w: encoding", ErrInvalidSummaryCursor)
	}
	var cursor summaryCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Version != 1 || cursor.Order != summaryPageOrder || cursor.ID == "" {
		return nil, fmt.Errorf("%w: version, order or key", ErrInvalidSummaryCursor)
	}
	if (cursor.ParentID == nil) != (q.ParentID == nil) || (q.ParentID != nil && *cursor.ParentID != *q.ParentID) {
		return nil, fmt.Errorf("%w: parent filter changed", ErrInvalidSummaryCursor)
	}
	return &cursor, nil
}

func summaryAfter(row SessionSummary, cursor *summaryCursor) bool {
	used := row.Metadata.LastUsed
	if !used.Equal(cursor.LastUsed) {
		return used.Before(cursor.LastUsed)
	}
	if row.Metadata.Name != cursor.Name {
		return row.Metadata.Name > cursor.Name
	}
	return row.ID > cursor.ID
}

func (s *legacySummaryPages) ListSummaryPage(ctx context.Context, q SummaryQuery) (SummaryPage, error) {
	if err := ctx.Err(); err != nil {
		return SummaryPage{}, err
	}
	if err := validatePageLimit(q.Limit); err != nil {
		return SummaryPage{}, err
	}
	cursor, err := decodeSummaryCursor(q)
	if err != nil {
		return SummaryPage{}, err
	}
	rows, err := s.store.ListSummaries(ctx)
	if err != nil {
		return SummaryPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return SummaryPage{}, err
	}
	// Do not reorder a slice a supplied backend may retain.
	rows = slices.Clone(rows)
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return SummaryPage{}, err
		}
		if row.ID == "" || row.Metadata == nil {
			return SummaryPage{}, fmt.Errorf("%w: summaries require stable IDs and metadata", ErrPagingUnavailable)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if !a.Metadata.LastUsed.Equal(b.Metadata.LastUsed) {
			return a.Metadata.LastUsed.After(b.Metadata.LastUsed)
		}
		if a.Metadata.Name != b.Metadata.Name {
			return a.Metadata.Name < b.Metadata.Name
		}
		return a.ID < b.ID
	})
	var page SummaryPage
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return SummaryPage{}, err
		}
		if q.ParentID != nil && row.ParentID != *q.ParentID {
			continue
		}
		if cursor != nil && !summaryAfter(row, cursor) {
			continue
		}
		if len(page.Summaries) == q.Limit {
			last := page.Summaries[len(page.Summaries)-1]
			data, err := json.Marshal(summaryCursor{Version: 1, Order: summaryPageOrder, ParentID: q.ParentID, LastUsed: last.Metadata.LastUsed, Name: last.Metadata.Name, ID: last.ID})
			if err != nil {
				return SummaryPage{}, fmt.Errorf("encode summary cursor: %w", err)
			}
			page.Next = base64.RawURLEncoding.EncodeToString(data)
			break
		}
		row.Metadata = cloneMetadata(row.Metadata)
		page.Summaries = append(page.Summaries, row)
	}
	return page, nil
}

func (s *legacySummaryPages) GetSummary(ctx context.Context, id string) (*SessionSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, ErrSessionNotFound
	}
	rows, err := s.store.ListSummaries(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if row.ID == "" || row.Metadata == nil {
			return nil, fmt.Errorf("%w: summaries require stable IDs and metadata", ErrPagingUnavailable)
		}
		if row.ID == id {
			row.Metadata = cloneMetadata(row.Metadata)
			return &row, nil
		}
	}
	return nil, ErrSessionNotFound
}
