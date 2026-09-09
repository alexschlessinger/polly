package main

import "testing"

type registryProbe struct {
	transcriptAnchor
	name string
}

func TestTranscriptRegistryPinsAndRenumbers(t *testing.T) {
	var reg transcriptRegistry[*registryProbe]
	if reg.count() != 0 || reg.latest() != nil || reg.at(0) != nil || reg.idAt(0) != 0 {
		t.Fatalf("zero registry is not empty: %+v", reg)
	}
	a := reg.add(&registryProbe{name: "a"}, 2)
	b := reg.add(&registryProbe{name: "b"}, 5)
	c := reg.add(&registryProbe{name: "c"}, 6)
	if a.id != 1 || b.id != 2 || c.id != 3 {
		t.Fatalf("ids = %d %d %d, want 1 2 3", a.id, b.id, c.id)
	}
	if reg.latest() != c || reg.get(2) != b || reg.at(5) != b || reg.idAt(6) != 3 || reg.count() != 3 {
		t.Fatalf("lookups disagree: latest=%v get=%v at=%v idAt=%d count=%d", reg.latest(), reg.get(2), reg.at(5), reg.idAt(6), reg.count())
	}

	// Deleting an unpinned line shifts later pins only.
	if removed, ok := reg.deleteLine(3); ok || removed != nil {
		t.Fatalf("deleting an unpinned line removed %v", removed)
	}
	if a.transcriptIndex != 2 || b.transcriptIndex != 4 || c.transcriptIndex != 5 {
		t.Fatalf("indices after unpinned delete = %d %d %d, want 2 4 5", a.transcriptIndex, b.transcriptIndex, c.transcriptIndex)
	}
	if reg.at(4) != b || reg.at(5) != c || reg.at(6) != nil {
		t.Fatalf("pins after unpinned delete: at4=%v at5=%v at6=%v", reg.at(4), reg.at(5), reg.at(6))
	}

	// Deleting a pinned line drops its record and closes the gap. Adjacent
	// pins move through each other's old line without clobbering.
	removed, ok := reg.deleteLine(4)
	if !ok || removed != b {
		t.Fatalf("deleteLine(4) = %v, %v; want b", removed, ok)
	}
	if reg.count() != 2 || reg.get(2) != nil || reg.at(4) != c || c.transcriptIndex != 4 || reg.idAt(5) != 0 {
		t.Fatalf("after pinned delete: count=%d get2=%v at4=%v c.index=%d idAt5=%d", reg.count(), reg.get(2), reg.at(4), c.transcriptIndex, reg.idAt(5))
	}
	if reg.latest() != c {
		t.Fatalf("latest after delete = %v, want c", reg.latest())
	}

	// Ids keep ascending past removed ones; remove unpins.
	d := reg.add(&registryProbe{name: "d"}, 9)
	if d.id != 4 {
		t.Fatalf("id after delete = %d, want 4", d.id)
	}
	reg.remove(d.id)
	if reg.get(4) != nil || reg.at(9) != nil || reg.count() != 2 || reg.latest() != nil {
		t.Fatalf("remove left state: get=%v at=%v count=%d latest=%v", reg.get(4), reg.at(9), reg.count(), reg.latest())
	}
}

func TestTranscriptRegistryCloneAndReset(t *testing.T) {
	var reg transcriptRegistry[*registryProbe]
	a := reg.add(&registryProbe{name: "a"}, 1)
	reg.add(&registryProbe{name: "b"}, 3)

	dup := reg.clone(func(p *registryProbe) *registryProbe { c := *p; return &c })
	if dup.count() != 2 || dup.at(1) == a || dup.at(1).name != "a" || dup.at(3).id != 2 {
		t.Fatalf("clone = %+v", dup)
	}
	dup.deleteLine(1)
	if reg.at(1) != a || reg.count() != 2 || a.transcriptIndex != 1 {
		t.Fatalf("clone shares state with the source: %+v", reg)
	}
	if next := dup.add(&registryProbe{name: "c"}, 0); next.id != 3 {
		t.Fatalf("clone id sequence = %d, want 3", next.id)
	}

	seen := 0
	for id, record := range reg.all() {
		if record.id != id {
			t.Fatalf("all() yielded id %d for record %d", id, record.id)
		}
		seen++
	}
	if seen != 2 {
		t.Fatalf("all() visited %d records, want 2", seen)
	}

	reg.reset()
	if reg.count() != 0 || reg.at(1) != nil {
		t.Fatalf("reset left records: %+v", reg)
	}
	if next := reg.add(&registryProbe{name: "z"}, 0); next.id != 1 {
		t.Fatalf("id after reset = %d, want 1", next.id)
	}

	var empty transcriptRegistry[*registryProbe]
	if c := empty.clone(func(p *registryProbe) *registryProbe { return p }); c.count() != 0 || c.byID != nil {
		t.Fatalf("clone of the zero registry allocated: %+v", c)
	}
}
