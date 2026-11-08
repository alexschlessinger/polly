package sessions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

func readLegacyCursorPage(t *testing.T, access CoordinationAccess, query RecordQuery) RecordPage {
	t.Helper()
	var page RecordPage
	if err := access.ReadRecords(context.Background(), func(r CoordinationReader) error {
		var err error
		page, err = r.Query(query)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestCoordinationRecordsLegacyCursorBytePreservingPages(t *testing.T) {
	keys := func(kind string, ids ...string) []RecordKey {
		out := make([]RecordKey, 0, len(ids))
		for _, id := range ids {
			out = append(out, RecordKey{kind, id})
		}
		return out
	}
	type testCase struct {
		name     string
		query    RecordQuery
		parent   string
		want     []RecordKey // In canonical order, inserted in reverse below.
		payloads []string
	}
	tests := []testCase{
		{
			name:  "opaque IDs",
			query: RecordQuery{Kind: "custom"},
			want:  keys("custom", "\xff", "\xffz"),
		},
		{
			name:  "Unicode and NUL IDs",
			query: RecordQuery{Kind: "custom"},
			want:  keys("custom", "\x00", "A", "a", "e\u0301", "é", "雪", "\ufffd", "\xff", "\xffz"),
		},
		{
			name: "opaque Unicode and NUL kinds",
			want: []RecordKey{{"\x00", "id"}, {"A", "id"}, {"a", "id"}, {"é", "id"}, {"雪", "id"}, {"\ufffd", "id"}, {"\xff", "id"}, {"\xffz", "id"}},
		},
		{
			name:  "opaque kind filter",
			query: RecordQuery{Kind: "\xff\x00雪"},
			want:  keys("\xff\x00雪", "a", "b"),
		},
		{
			name:  "opaque kind selector",
			query: RecordQuery{Index: CoordinationIndexKind, Key: "\xff\x00雪"},
			want:  keys("\xff\x00雪", "a", "b"),
		},
		{
			name:   "opaque family",
			query:  RecordQuery{Kind: "custom"},
			parent: "\xff\x00雪",
			want:   keys("custom", "a", "b"),
		},
		{
			name:  "workflow byte and numeric order",
			query: RecordQuery{Kind: "workflow_step"},
			want:  keys("workflow_step", "雪\x00/0", "雪\x00/2", "雪\x00/10", "\xff/0", "\xff/2", "\xff/10", "\xffz/0"),
		},
		{
			name:  "opaque workflow selector",
			query: RecordQuery{Index: CoordinationIndexWorkflowSteps, Key: "\xff"},
			want:  keys("workflow_step", "\xff/0", "\xff/2", "\xff/10"),
		},
		{
			name:  "oversized records advance",
			query: RecordQuery{Kind: "custom", Limit: 10, MaxBytes: 1},
			want:  keys("custom", "\xff", "\xffz"),
		},
		{
			name:  "byte budget advances",
			query: RecordQuery{Kind: "custom", Limit: 10, MaxBytes: 3},
			want:  keys("custom", "\xff", "\xffz"),
		},
	}
	for _, kind := range []string{"mail", "publication"} {
		tests = append(tests, testCase{
			name:  kind + " instant order and bytewise ties",
			query: RecordQuery{Kind: kind},
			want:  keys(kind, "z", "\xff", "\xffz"),
			payloads: []string{
				`{"posted":"2025-01-01T00:00:00Z"}`,
				`{"posted":"2026-01-01T01:00:00+01:00"}`,
				`{"posted":"2026-01-01T00:00:00Z"}`,
			},
		})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newRecordLegacyFixture()
			// Unknown kinds need no format marker; omit it from all-record queries.
			if test.query.Kind == "" && test.query.Index == "" {
				delete(f.state.Records, "format")
			}
			if test.parent != "" {
				f.state.ParentID = test.parent
			}
			access := recordAccess(t, f)
			if _, err := access.UpdateRecords(context.Background(), func(tx CoordinationTx) error {
				for i := len(test.want) - 1; i >= 0; i-- {
					payload := `{}`
					if test.payloads != nil {
						payload = test.payloads[i]
					}
					putRecord(t, tx, test.want[i].Kind, test.want[i].ID, payload)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			query := test.query
			query.IncludeTotal = true
			if query.Limit == 0 {
				query.Limit = 1
			}
			seen := make(map[RecordKey]bool)
			cursors := make(map[string]bool)
			// Each page is a new callback. Bound the loop so broken cursors fail
			// rather than hanging, and require exactly one visit per record.
			for i, want := range test.want {
				page := readLegacyCursorPage(t, access, query)
				if len(page.Records) != 1 || page.Total != len(test.want) {
					t.Fatalf("page %d: %+v", i, page)
				}
				got := page.Records[0]
				if got.Key != want || seen[got.Key] {
					t.Fatalf("page %d: key=%q want=%q repeated=%t", i, got.Key, want, seen[got.Key])
				}
				seen[got.Key] = true
				payload := `{}`
				if test.payloads != nil {
					payload = test.payloads[i]
				}
				if string(got.Payload) != payload || page.Oversized != (query.MaxBytes > 0 && len(payload) > query.MaxBytes) {
					t.Fatalf("page %d: payload=%s oversized=%t", i, got.Payload, page.Oversized)
				}
				if (page.Next == "") != (i == len(test.want)-1) || page.Next != "" && cursors[page.Next] {
					t.Fatalf("page %d: unexpected terminal/repeated cursor %q", i, page.Next)
				}
				cursors[page.Next] = true
				query.After = page.Next
			}
			if f.reads != len(test.want) {
				t.Fatalf("reads=%d want one callback per page (%d)", f.reads, len(test.want))
			}
		})
	}
}

func TestCoordinationRecordsLegacyCursorByteBinding(t *testing.T) {
	f := newRecordLegacyFixture()
	f.state.ParentID = "\xff"
	access := recordAccess(t, f)
	if _, err := access.UpdateRecords(context.Background(), func(tx CoordinationTx) error {
		putRecord(t, tx, "\xff", "a", `{}`)
		putRecord(t, tx, "\xff", "b", `{}`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []RecordQuery{
		{Kind: "\xff", Limit: 1},
		{Index: CoordinationIndexKind, Key: "\xff", Limit: 1},
	} {
		page := readLegacyCursorPage(t, access, query)
		if len(page.Records) != 1 || page.Next == "" {
			t.Fatalf("first page: %+v", page)
		}
		query.After = page.Next
		for _, other := range []string{"\xfe", "\ufffd", "other"} {
			for _, field := range []string{"selector", "family"} {
				changed := query
				if field == "family" {
					f.state.ParentID = other
				} else if changed.Kind != "" {
					changed.Kind = other
				} else {
					changed.Key = other
				}
				if err := access.ReadRecords(context.Background(), func(r CoordinationReader) error {
					_, err := r.Query(changed)
					return err
				}); !errors.Is(err, ErrCoordinationCursor) {
					t.Fatalf("changed %s to %q: %v", field, other, err)
				}
				f.state.ParentID = "\xff"
			}
		}
		// Even selectors yielding the same rows stay separately bound.
		changed := query
		if changed.Index == CoordinationIndexKind {
			changed.Index, changed.Key, changed.Kind = "", "", "\xff"
		} else {
			changed.Index, changed.Key = CoordinationIndexKind, "\xff"
		}
		if err := access.ReadRecords(context.Background(), func(r CoordinationReader) error {
			_, err := r.Query(changed)
			return err
		}); !errors.Is(err, ErrCoordinationCursor) {
			t.Fatalf("changed index: %v", err)
		}
		// The default index normalizes to records. Page controls aren't selector
		// identity and may change between callbacks.
		if query.Index == "" {
			query.Index = CoordinationIndexRecords
		}
		query.Limit, query.MaxBytes, query.IncludeTotal = 10, 1, true
		next := readLegacyCursorPage(t, access, query)
		if len(next.Records) != 1 || next.Records[0].Key.ID != "b" || next.Next != "" || !next.Oversized || next.Total != 2 {
			t.Fatalf("same selector: %+v", next)
		}
	}
}

func TestCoordinationRecordsLegacyCursorWireValidation(t *testing.T) {
	f := newRecordLegacyFixture()
	access := recordAccess(t, f)
	if _, err := access.UpdateRecords(context.Background(), func(tx CoordinationTx) error {
		putRecord(t, tx, "custom", "a", `{}`)
		putRecord(t, tx, "custom", "b", `{}`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	query := RecordQuery{Kind: "custom", Limit: 1}
	page := readLegacyCursorPage(t, access, query)
	data, err := base64.RawURLEncoding.DecodeString(page.Next)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"Version", "Parent", "Kind", "Key", "Order.Kind", "Order.ID", "Order.Workflow", "empty ID", "empty kind"} {
		t.Run(field, func(t *testing.T) {
			var wire map[string]any
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "Version":
				wire[field] = 1 // Old lossy string cursors must not be misinterpreted.
			case "Order.Kind", "Order.ID", "Order.Workflow":
				wire["Order"].(map[string]any)[field[len("Order."):]] = "!"
			case "empty ID":
				wire["Order"].(map[string]any)["ID"] = ""
			case "empty kind":
				wire["Order"].(map[string]any)["Kind"] = ""
			default:
				wire[field] = "!"
			}
			malformed, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			changed := query
			changed.After = base64.RawURLEncoding.EncodeToString(malformed)
			if err := access.ReadRecords(context.Background(), func(r CoordinationReader) error {
				_, err := r.Query(changed)
				return err
			}); !errors.Is(err, ErrCoordinationCursor) {
				t.Fatalf("accepted malformed %s: %v", field, err)
			}
		})
	}
}
