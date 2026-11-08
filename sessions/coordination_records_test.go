package sessions

import (
	"context"
	"errors"
	"testing"
)

// The supplied new capability is itself functional but may be slow. Merely
// implementing it must not make OpenCoordinationAccess advertise an index.
type recordCapabilityFixture struct {
	*recordLegacyFixture
	records                                 CoordinationRecords
	views                                   CoordinationRecordsViewStore
	recordReads, recordUpdates, recordViews int
}

func (f *recordCapabilityFixture) ReadRecords(ctx context.Context, fn func(CoordinationReader) error) error {
	f.recordReads++
	return f.records.ReadRecords(ctx, fn)
}
func (f *recordCapabilityFixture) UpdateRecords(ctx context.Context, fn func(CoordinationTx) error) (CoordinationCommit, error) {
	f.recordUpdates++
	return f.records.UpdateRecords(ctx, fn)
}
func (f *recordCapabilityFixture) ReadRecordsView(ctx context.Context, root string, fn func(CoordinationReader) error) error {
	f.recordViews++
	return f.views.ReadRecordsView(ctx, root, fn)
}

type reportingRecordCapabilityFixture struct{ *recordCapabilityFixture }

func (*reportingRecordCapabilityFixture) CoordinationCapabilities() CoordinationCapabilities {
	return CoordinationCapabilities{NativeIndexed: true, DurableRevisions: true}
}

func TestCoordinationRecordsSuppliedCapabilitySelection(t *testing.T) {
	ctx := context.Background()
	authoritative := newRecordLegacyFixture()
	unused := newRecordLegacyFixture()
	views, err := OpenCoordinationViewAccess(authoritative)
	if err != nil {
		t.Fatal(err)
	}
	handle := &recordCapabilityFixture{recordLegacyFixture: unused, records: recordAccess(t, authoritative), views: views}
	access, err := OpenCoordinationAccess(handle)
	if err != nil {
		t.Fatal(err)
	}
	if access.Capabilities() != (CoordinationCapabilities{}) {
		t.Fatal("interface alone advertised fast access")
	}
	if _, err := access.UpdateRecords(ctx, func(tx CoordinationTx) error { putRecord(t, tx, "custom", "x", `{"value":1}`); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := access.ReadRecords(ctx, func(r CoordinationReader) error {
		_, ok, err := r.Get(RecordKey{"custom", "x"})
		if !ok || err != nil {
			t.Fatalf("selected authority lost write: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	view, err := OpenCoordinationViewAccess(handle)
	if err != nil {
		t.Fatal(err)
	}
	if err := view.ReadRecordsView(ctx, "root", func(r CoordinationReader) error {
		_, ok, err := r.Get(RecordKey{"custom", "x"})
		if !ok || err != nil {
			t.Fatalf("selected view lost write: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if handle.recordReads != 1 || handle.recordUpdates != 1 || handle.recordViews != 1 || unused.reads != 0 || unused.updates != 0 {
		t.Fatalf("bypassed capability: %+v", handle)
	}
	reporter := &reportingRecordCapabilityFixture{handle}
	reported, err := OpenCoordinationAccess(reporter)
	if err != nil {
		t.Fatal(err)
	}
	reportedView, err := OpenCoordinationViewAccess(reporter)
	if err != nil {
		t.Fatal(err)
	}
	want := CoordinationCapabilities{NativeIndexed: true, DurableRevisions: true}
	if reported.Capabilities() != want || reportedView.Capabilities() != want {
		t.Fatal("lost explicit capabilities")
	}
	for _, handle := range []any{nil, (*recordCapabilityFixture)(nil), struct{}{}} {
		if _, err := OpenCoordinationAccess(handle); !errors.Is(err, ErrCoordinationUnsupported) {
			t.Fatal(err)
		}
		if _, err := OpenCoordinationViewAccess(handle); !errors.Is(err, ErrCoordinationUnsupported) {
			t.Fatal(err)
		}
	}
}

func TestCoordinationRecordsLegacyRootViewIdentity(t *testing.T) {
	ctx := context.Background()
	f := newRecordLegacyFixture()
	f.state.Sequence = 7
	view, err := OpenCoordinationViewAccess(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := view.ReadRecordsView(ctx, "root", func(r CoordinationReader) error {
		identity, err := r.Identity()
		if err != nil || identity.Sequence != 7 || identity.ActorID != "root" || identity.ParentID != "root" {
			t.Fatalf("identity %+v %v", identity, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if f.reads != 1 || view.Capabilities() != (CoordinationCapabilities{}) {
		t.Fatal("view count/capabilities")
	}
	for _, root := range []string{"child", ""} {
		if err := view.ReadRecordsView(ctx, root, func(CoordinationReader) error { t.Fatal("invalid root callback"); return nil }); err == nil {
			t.Fatal("invalid root accepted")
		}
	}
	f.state.ActorID = "child"
	if err := view.ReadRecordsView(ctx, "root", func(CoordinationReader) error { t.Fatal("non-root callback"); return nil }); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal(err)
	}
}
