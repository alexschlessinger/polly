package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
)

func TestRecoveryNativeUnsupportedFormatPrecedesPayloadSelection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, format, kind, want string
		journal                  bool
	}{
		{name: "missing/member", kind: "member", want: "no format record"},
		{name: "missing/member-and-journal", kind: "member", journal: true, want: "no format record"},
		{name: "missing/unknown-kind", kind: "extension", want: "no format record"},
		{name: "version-zero", format: `{"version":0}`, kind: "member", want: "format version 0"},
		{name: "old-version", format: `{"version":1}`, kind: "member", want: "format version 1"},
		{name: "future-version", format: `{"version":3}`, kind: "member", want: "format version 3"},
		{name: "future-version-without-members", format: `{"version":3}`, want: "format version 3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := runtimeTest(t, nilModel(), 1, 1)
			if err := r.parent.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
				if test.format != "" {
					raw.Records[formatKind] = map[string]json.RawMessage{formatID: json.RawMessage(test.format)}
				}
				if test.kind != "" {
					raw.Records[test.kind] = map[string]json.RawMessage{"m": json.RawMessage(`{"id":"m"}`)}
				}
				if test.journal {
					raw.Records["parent_turn"] = map[string]json.RawMessage{r.ID: json.RawMessage(`{"retryPause":{"reason":"retained"}}`)}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			r.Close()
			probe := probeRecovery(r.parent)
			config := r.config
			config.Parent = probe
			fresh, err := New(config)
			if fresh != nil || !errors.Is(err, ErrUnsupportedFormat) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New returned %v, %v; want unsupported format with %q", fresh, err, test.want)
			}
			if probe.writes != 0 || probe.passes != 0 {
				t.Fatalf("unsupported format reached payload scan or recovery: passes=%d writes=%d", probe.passes, probe.writes)
			}
		})
	}
}

func TestRecoveryFormatlessRootStillValidatesOwnJournal(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, journal string
		malformed     bool
	}{
		{name: "empty"},
		{name: "own-journal", journal: `{"retryPause":{"reason":"retained","attempts":2}}`},
		{name: "malformed-own-journal", journal: `{"retryPause":{"attempts":"bad"}}`, malformed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := runtimeTest(t, nilModel(), 1, 1)
			if test.journal != "" {
				if err := r.parent.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
					raw.Records["parent_turn"] = map[string]json.RawMessage{r.ID: json.RawMessage(test.journal)}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			r.Close()
			config := r.config
			config.Promote = func(context.Context) error { return errors.New("unexpected promotion") }
			fresh, err := New(config)
			if test.malformed {
				if fresh != nil || err == nil || errors.Is(err, ErrUnsupportedFormat) {
					t.Fatalf("malformed own journal misclassified: %v, %v", fresh, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				fresh.Close()
			}
			records := rawRecords(t, r)
			if len(records[formatKind]) != 0 || string(records["parent_turn"][r.ID]) != test.journal {
				t.Fatal("format-less constructor mutated records")
			}
		})
	}
}

type recoveryFormatErrorSession struct {
	*recoveryProbe
	countOnly bool
	failure   error
}

func (s *recoveryFormatErrorSession) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	return s.recoveryProbe.ReadRecords(ctx, func(rd sessions.CoordinationReader) error {
		return fn(recoveryFormatErrorReader{CoordinationReader: rd, owner: s})
	})
}

type recoveryFormatErrorReader struct {
	sessions.CoordinationReader
	owner *recoveryFormatErrorSession
}

func (rd recoveryFormatErrorReader) Get(key sessions.RecordKey) (sessions.CoordinationRecord, bool, error) {
	if !rd.owner.countOnly && key == (sessions.RecordKey{Kind: formatKind, ID: formatID}) {
		return sessions.CoordinationRecord{}, false, rd.owner.failure
	}
	return rd.CoordinationReader.Get(key)
}

func (rd recoveryFormatErrorReader) Query(q sessions.RecordQuery) (sessions.RecordPage, error) {
	if rd.owner.countOnly && q.CountOnly {
		return sessions.RecordPage{}, rd.owner.failure
	}
	return rd.CoordinationReader.Query(q)
}

func TestRecoveryFormatPreflightPreservesStorageAndMalformedErrors(t *testing.T) {
	t.Parallel()
	for _, countOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("storage/count=%v", countOnly), func(t *testing.T) {
			r := runtimeTest(t, nilModel(), 1, 1)
			r.Close()
			failure := errors.New("transient storage read failure")
			probe := &recoveryFormatErrorSession{recoveryProbe: probeRecovery(r.parent), countOnly: countOnly, failure: failure}
			config := r.config
			config.Parent = probe
			fresh, err := New(config)
			if fresh != nil || !errors.Is(err, failure) || errors.Is(err, ErrUnsupportedFormat) || probe.writes != 0 {
				t.Fatalf("storage error translated or mutated: %v, %v writes=%d", fresh, err, probe.writes)
			}
		})
	}
	for _, payload := range []string{`{"version":"bad"}`, `null`, `{`} {
		t.Run("malformed/"+payload, func(t *testing.T) {
			r := runtimeTest(t, nilModel(), 1, 1)
			if err := r.prepare(t.Context()); err != nil {
				t.Fatal(err)
			}
			r.Close()
			probe := probeRecovery(r.parent)
			probe.override = map[sessions.RecordKey]json.RawMessage{{Kind: formatKind, ID: formatID}: json.RawMessage(payload)}
			config := r.config
			config.Parent = probe
			fresh, err := New(config)
			if fresh != nil || err == nil || errors.Is(err, ErrUnsupportedFormat) || probe.writes != 0 {
				t.Fatalf("malformed format translated or mutated: %v, %v writes=%d", fresh, err, probe.writes)
			}
		})
	}
}
