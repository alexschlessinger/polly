package sessions

// This file is the single cross-record derivation path for native persistence
// and the functional legacy adapter. It never authorizes actors or mutates domain
// records. SQL and memory adapters only implement indexed storage primitives.
import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
)

type derivedItem struct {
	key   RecordKey
	order coordindex.Order
}
type derivedStore interface {
	get(RecordKey) (json.RawMessage, error)
	scan(coordindex.Index, string, string, *coordindex.Order, int) ([]derivedItem, error)
	links(RecordKey, coordindex.Index) ([]coordindex.Relation, error)
	set(derivedItem, coordindex.Relation, bool) error
}
type derivedEngine struct {
	store  derivedStore
	parent string
}

func (d derivedEngine) each(index coordindex.Index, key, kind string, fn func(derivedItem) error) error {
	var after *coordindex.Order
	for {
		rows, err := d.store.scan(index, key, kind, after, 128)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err = fn(row); err != nil {
				return err
			}
		}
		if len(rows) < 128 {
			return nil
		}
		last := rows[len(rows)-1].order
		after = &last
	}
}
func (d derivedEngine) first(index coordindex.Index, key string) (string, error) {
	rows, err := d.store.scan(index, key, "", nil, 1)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[0].key.ID, nil
}
func (d derivedEngine) replace(item derivedItem, index coordindex.Index, keys []string) error {
	old, err := d.store.links(item.key, index)
	if err != nil {
		return err
	}
	for _, rel := range old {
		if !slices.Contains(keys, rel.Key) {
			if err = d.store.set(item, rel, false); err != nil {
				return err
			}
		}
	}
	for _, key := range keys {
		if err = d.store.set(item, coordindex.Relation{Index: index, Key: key}, true); err != nil {
			return err
		}
	}
	return nil
}
func (d derivedEngine) publication(id string) (*coordindex.Publication, error) {
	b, err := d.store.get(RecordKey{"publication", id})
	if err != nil || b == nil {
		return nil, err
	}
	var p coordindex.Publication
	if err = json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	p.ID = id
	return &p, nil
}
func (d derivedEngine) fact(p coordindex.Publication) (string, error) {
	commit := ""
	if p.Snapshot != "" {
		b, err := d.store.get(RecordKey{"snapshot", p.Snapshot})
		if err != nil {
			return "", err
		}
		if b != nil {
			var s struct{ ID, Tree, Commit, Source string }
			if err = json.Unmarshal(b, &s); err != nil {
				return "", err
			}
			if s.ID == p.Snapshot && s.Tree != "" && s.Commit != "" && s.Source != "" {
				commit = s.Commit
			}
		}
	}
	return coordindex.PublicationFactKey(p, commit), nil
}
func (d derivedEngine) active(p *coordindex.Publication) (bool, error) {
	if p == nil {
		return false, nil
	}
	id, err := d.first(coordindex.PublicationSupersedes, p.ID)
	return id == "", err
}

// publications visits only touched publications and old/new equivalence heads.
// A superseding record remains a withdrawal even when itself superseded.
func (d derivedEngine) publications(ids map[string]bool, groups map[string]bool) error {
	if groups == nil {
		groups = map[string]bool{}
	}
	affected := map[string]bool{}
	for id := range ids {
		affected[id] = true
		links, err := d.store.links(RecordKey{"publication", id}, coordindex.PublicationEquivalent)
		if err != nil {
			return err
		}
		for _, link := range links {
			groups[link.Key] = true
		}
		p, err := d.publication(id)
		if err != nil {
			return err
		}
		if p != nil && p.Key != "" {
			key, err := d.fact(*p)
			if err != nil {
				return err
			}
			groups[key] = true
		}
	}
	for key := range groups {
		id, err := d.first(coordindex.PublicationEquivalent, key)
		if err != nil {
			return err
		}
		if id != "" {
			affected[id] = true
		}
	}
	for id := range ids {
		p, err := d.publication(id)
		if err != nil {
			return err
		}
		item := derivedItem{key: RecordKey{"publication", id}, order: coordindex.Order{Kind: "publication", ID: id}}
		var audiences, eq []string
		active, err := d.active(p)
		if err != nil {
			return err
		}
		if p != nil {
			item.order = coordindex.PublicationOrder(*p)
		}
		if active {
			audiences = coordindex.PublicationAudiences(*p)
			if p.Key != "" {
				key, err := d.fact(*p)
				if err != nil {
					return err
				}
				eq = []string{key}
			}
		}
		if err = d.replace(item, coordindex.PublicationEquivalent, eq); err != nil {
			return err
		}
		if err = d.replace(item, coordindex.PublicationActive, audiences); err != nil {
			return err
		}
	}
	for key := range groups {
		id, err := d.first(coordindex.PublicationEquivalent, key)
		if err != nil {
			return err
		}
		if id != "" {
			affected[id] = true
		}
	}
	for id := range affected {
		if err := d.publicationDelivery(id); err != nil {
			return err
		}
	}
	return nil
}
func (d derivedEngine) publicationDelivery(id string) error {
	p, err := d.publication(id)
	if err != nil {
		return err
	}
	item := derivedItem{key: RecordKey{"publication", id}, order: coordindex.Order{Kind: "publication", ID: id}}
	active, err := d.active(p)
	if err != nil {
		return err
	}
	survivor := active
	if p != nil {
		item.order = coordindex.PublicationOrder(*p)
	}
	if active && p.Key != "" {
		key, err := d.fact(*p)
		if err != nil {
			return err
		}
		head, err := d.first(coordindex.PublicationEquivalent, key)
		if err != nil {
			return err
		}
		survivor = head == id
	}
	var audiences, events []string
	if survivor {
		audiences = coordindex.PublicationAudiences(*p)
	}
	if p != nil && coordindex.PublicationEvent(*p, active, survivor) {
		events = coordindex.PublicationAudiences(*p)
	}
	if err = d.replace(item, coordindex.PublicationSurvivor, audiences); err != nil {
		return err
	}
	if err = d.replace(item, coordindex.PublicationEvents, events); err != nil {
		return err
	}
	// Actual audience fanout happens at publication/promotion writes, never reads.
	readers := map[string]bool{}
	old, err := d.store.links(item.key, coordindex.PublicationPending)
	if err != nil {
		return err
	}
	for _, rel := range old {
		readers[rel.Key] = true
	}
	for _, audience := range events {
		if audience == coordindex.Key("parent") {
			readers[d.parent] = true
		}
		if err = d.each(coordindex.PublicationReader, audience, "", func(row derivedItem) error { readers[row.key.ID] = true; return nil }); err != nil {
			return err
		}
	}
	for id := range readers {
		reader, err := d.reader(id)
		if err != nil {
			return err
		}
		pending := p != nil && reader != nil && coordindex.PublicationPendingFor(*p, active, survivor, *reader)
		if err = d.store.set(item, coordindex.Relation{Index: coordindex.PublicationPending, Key: id}, pending); err != nil {
			return err
		}
	}
	return nil
}

type derivedMember struct {
	Execution, Control string
	Publications       struct {
		Posted json.RawMessage
		ID     string
	}
}
type derivedExecution struct{ Run, Status string }

func (d derivedEngine) reader(id string) (*coordindex.PublicationReaderState, error) {
	b, err := d.store.get(RecordKey{"member", id})
	if err != nil {
		return nil, err
	}
	out := &coordindex.PublicationReaderState{ID: id}
	var source struct {
		Execution    string
		Publications struct {
			Posted json.RawMessage
			ID     string
		}
	}
	if b != nil {
		if err = json.Unmarshal(b, &source); err != nil {
			return nil, err
		}
		if source.Execution != "" {
			e, err := d.store.get(RecordKey{"execution", source.Execution})
			if err != nil {
				return nil, err
			}
			if e != nil {
				var execution derivedExecution
				if err = json.Unmarshal(e, &execution); err != nil {
					return nil, err
				}
				out.Run = execution.Run
			}
		}
	} else {
		b, err = d.store.get(RecordKey{"parent", id})
		if err != nil {
			return nil, err
		}
		if b == nil && id != d.parent {
			return nil, nil
		}
		out.Parent = true
		if b != nil {
			if err = json.Unmarshal(b, &source); err != nil {
				return nil, err
			}
		}
	}
	if source.Publications.Posted != nil {
		if err = json.Unmarshal(source.Publications.Posted, &out.Posted); err != nil {
			return nil, err
		}
	}
	out.MarkID = source.Publications.ID
	return out, nil
}
func readerAudiences(r *coordindex.PublicationReaderState) []string {
	if r == nil {
		return nil
	}
	if r.Parent {
		return []string{coordindex.Key("parent")}
	}
	return []string{coordindex.Key("machine"), coordindex.Key("run", r.Run)}
}
func (d derivedEngine) readerChanged(id string, before *coordindex.PublicationReaderState) error {
	reader, err := d.reader(id)
	if err != nil {
		return err
	}
	// Member and explicit parent records supply reverse audience entries. Root is
	// also implicit, so its first publication needs no synthetic domain record.
	for _, kind := range []string{"member", "parent"} {
		b, err := d.store.get(RecordKey{kind, id})
		if err != nil {
			return err
		}
		var keys []string
		if b != nil && (kind == "member" || reader != nil && reader.Parent) {
			keys = readerAudiences(reader)
		}
		if err = d.replace(derivedItem{RecordKey{kind, id}, coordindex.Order{Kind: kind, ID: id}}, coordindex.PublicationReader, keys); err != nil {
			return err
		}
	}
	// Mark-only advances touch existing pending entries, not accumulated receipts
	// or already-read events. A new/changed run rebuild is explicitly cold work.
	sameAudience := reader != nil && before != nil && reader.Parent == before.Parent && reader.Run == before.Run
	forward := sameAudience && !coordindex.Less(coordindex.Order{Kind: "publication", Posted: reader.Posted, ID: reader.MarkID}, coordindex.Order{Kind: "publication", Posted: before.Posted, ID: before.MarkID})
	if sameAudience && reader.Posted.Equal(before.Posted) && reader.MarkID == before.MarkID {
		return nil
	}
	stop := errors.New("past publication mark")
	if err = d.each(coordindex.PublicationPending, id, "publication", func(item derivedItem) error {
		if forward && coordindex.Less(coordindex.Order{Kind: "publication", Posted: reader.Posted, ID: reader.MarkID}, item.order) {
			return stop
		}
		p, err := d.publication(item.key.ID)
		if err != nil {
			return err
		}
		keep := reader != nil && p != nil && coordindex.PublicationVisible(*p, *reader) && coordindex.PublicationUnread(*p, *reader) && p.Author != id
		return d.store.set(item, coordindex.Relation{Index: coordindex.PublicationPending, Key: id}, keep)
	}); err != nil && !errors.Is(err, stop) {
		return err
	}
	if forward || reader == nil {
		return nil
	}
	for _, audience := range readerAudiences(reader) {
		if err = d.each(coordindex.PublicationEvents, audience, "publication", func(item derivedItem) error {
			p, err := d.publication(item.key.ID)
			if err != nil {
				return err
			}
			if p == nil {
				return fmt.Errorf("publication event without record")
			}
			return d.store.set(item, coordindex.Relation{Index: coordindex.PublicationPending, Key: id}, coordindex.PublicationPendingFor(*p, true, true, *reader))
		}); err != nil {
			return err
		}
	}
	return nil
}
func (d derivedEngine) memberLifecycle(id string) error {
	b, err := d.store.get(RecordKey{"member", id})
	if err != nil {
		return err
	}
	var keys []string
	if b != nil {
		var m derivedMember
		if err = json.Unmarshal(b, &m); err != nil {
			return err
		}
		var e derivedExecution
		var eb json.RawMessage
		if m.Execution != "" {
			eb, err = d.store.get(RecordKey{"execution", m.Execution})
			if err != nil {
				return err
			}
			if eb != nil {
				if err = json.Unmarshal(eb, &e); err != nil {
					return err
				}
			}
		}
		keys = []string{coordindex.Lifecycle(m.Control, e.Status, eb != nil)}
	}
	return d.replace(derivedItem{RecordKey{"member", id}, coordindex.Order{Kind: "member", ID: id}}, coordindex.MemberLifecycle, keys)
}

func derivedNewsChanged(kind string, before, after json.RawMessage) (bool, error) {
	a, err := coordindex.NewsProjection(kind, before)
	if err != nil {
		return false, err
	}
	b, err := coordindex.NewsProjection(kind, after)
	return !bytes.Equal(a, b), err
}
