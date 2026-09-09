package main

import (
	"iter"
	"maps"
)

// transcriptAnchor pins a record to the transcript line that displays it.
// Record types embed it so one registry can renumber them when lines move.
type transcriptAnchor struct {
	id              int64
	transcriptIndex int
}

func (a *transcriptAnchor) anchor() *transcriptAnchor { return a }

type transcriptAnchored interface {
	anchor() *transcriptAnchor
}

// transcriptRegistry owns every record of one kind (reasoning disclosures,
// tool disclosures, turn trailers): ids are unique and ascending for the
// life of the display, and each record is pinned to one transcript line.
// The zero value is empty and ready to use; reset returns to it.
type transcriptRegistry[P transcriptAnchored] struct {
	byID   map[int64]P
	atLine map[int]int64
	seq    int64
}

// add assigns record the next id, pins it to line, and returns it.
func (r *transcriptRegistry[P]) add(record P, line int) P {
	if r.byID == nil {
		r.byID = make(map[int64]P)
		r.atLine = make(map[int]int64)
	}
	r.seq++
	a := record.anchor()
	a.id, a.transcriptIndex = r.seq, line
	r.byID[a.id] = record
	r.atLine[line] = a.id
	return record
}

// get returns the record with id, or nil.
func (r *transcriptRegistry[P]) get(id int64) P { return r.byID[id] }

// latest returns the most recently added record, or nil once it is gone.
func (r *transcriptRegistry[P]) latest() P { return r.byID[r.seq] }

// idAt returns the id of the record pinned to line, or 0.
func (r *transcriptRegistry[P]) idAt(line int) int64 { return r.atLine[line] }

// at returns the record pinned to line, or nil.
func (r *transcriptRegistry[P]) at(line int) P { return r.byID[r.atLine[line]] }

func (r *transcriptRegistry[P]) count() int { return len(r.byID) }

// all ranges over every record in no particular order.
func (r *transcriptRegistry[P]) all() iter.Seq2[int64, P] { return maps.All(r.byID) }

// remove forgets the record with id and unpins its line.
func (r *transcriptRegistry[P]) remove(id int64) {
	record, ok := r.byID[id]
	if !ok {
		return
	}
	delete(r.byID, id)
	if line := record.anchor().transcriptIndex; r.atLine[line] == id {
		delete(r.atLine, line)
	}
}

// deleteLine removes the record pinned to line, if any, and moves every
// record pinned after it up one line: the transcript entry at line is gone.
func (r *transcriptRegistry[P]) deleteLine(line int) (removed P, ok bool) {
	if id, pinned := r.atLine[line]; pinned {
		removed, ok = r.byID[id]
		r.remove(id)
	}
	var moved []P
	for at, id := range r.atLine {
		if at > line {
			moved = append(moved, r.byID[id])
		}
	}
	for _, record := range moved {
		delete(r.atLine, record.anchor().transcriptIndex)
	}
	for _, record := range moved {
		a := record.anchor()
		a.transcriptIndex--
		r.atLine[a.transcriptIndex] = a.id
	}
	return removed, ok
}

// reset forgets every record and restarts ids from 1.
func (r *transcriptRegistry[P]) reset() { *r = transcriptRegistry[P]{} }

// clone copies the registry with copyRecord producing each record's copy;
// ids and pins carry over so the copy projects the same transcript.
func (r *transcriptRegistry[P]) clone(copyRecord func(P) P) transcriptRegistry[P] {
	out := transcriptRegistry[P]{seq: r.seq}
	if r.byID == nil {
		return out
	}
	out.byID = make(map[int64]P, len(r.byID))
	for id, record := range r.byID {
		out.byID[id] = copyRecord(record)
	}
	out.atLine = maps.Clone(r.atLine)
	return out
}
