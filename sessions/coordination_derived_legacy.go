package sessions

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
)

// This adapter intentionally materializes the legacy family. It never reports
// NativeIndexed or DurableRevisions; the exact same derivation runs over it.
type memoryDerivedStore struct {
	ctx       context.Context
	records   map[string]map[string]json.RawMessage
	relations map[RecordKey]map[coordindex.Relation]coordindex.Order
}

func (s *memoryDerivedStore) get(key RecordKey) (json.RawMessage, error) {
	return s.records[key.Kind][key.ID], s.ctx.Err()
}
func (s *memoryDerivedStore) scan(index coordindex.Index, key, kind string, after *coordindex.Order, limit int) ([]derivedItem, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	var out []derivedItem
	rel := coordindex.Relation{Index: index, Key: key}
	for k, links := range s.relations {
		if kind != "" && k.Kind != kind {
			continue
		}
		if order, ok := links[rel]; ok && (after == nil || coordindex.Less(*after, order)) {
			out = append(out, derivedItem{k, order})
		}
	}
	sort.Slice(out, func(i, j int) bool { return coordindex.Less(out[i].order, out[j].order) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (s *memoryDerivedStore) links(key RecordKey, index coordindex.Index) ([]coordindex.Relation, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	var out []coordindex.Relation
	for rel := range s.relations[key] {
		if rel.Index == index {
			out = append(out, rel)
		}
	}
	return out, nil
}
func (s *memoryDerivedStore) set(item derivedItem, rel coordindex.Relation, present bool) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if !present {
		delete(s.relations[item.key], rel)
		return nil
	}
	if s.relations[item.key] == nil {
		s.relations[item.key] = map[coordindex.Relation]coordindex.Order{}
	}
	s.relations[item.key][rel] = item.order
	return nil
}
func buildDerivedMemory(ctx context.Context, records map[string]map[string]json.RawMessage, parent string) (*memoryDerivedStore, error) {
	s := &memoryDerivedStore{ctx: ctx, records: records, relations: map[RecordKey]map[coordindex.Relation]coordindex.Order{}}
	pubs := map[string]bool{}
	for _, kind := range []string{"publication", "snapshot", "member", "parent", "execution"} {
		for id, payload := range records[kind] {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			rels, order, err := coordindex.Extract(kind, id, payload)
			if err != nil {
				return nil, err
			}
			for _, rel := range rels {
				if err = s.set(derivedItem{RecordKey{kind, id}, order}, rel, true); err != nil {
					return nil, err
				}
			}
			if kind == "publication" {
				pubs[id] = true
			}
		}
	}
	d := derivedEngine{s, parent}
	for id := range records["member"] {
		if err := d.readerChanged(id, nil); err != nil {
			return nil, err
		}
		if err := d.memberLifecycle(id); err != nil {
			return nil, err
		}
	}
	for id := range records["parent"] {
		if err := d.readerChanged(id, nil); err != nil {
			return nil, err
		}
	}
	if err := d.publications(pubs, nil); err != nil {
		return nil, err
	}
	return s, nil
}
func isDerivedSelector(index CoordinationIndex) bool {
	switch index {
	case CoordinationIndexPublicationActive, CoordinationIndexPublicationSurvivor, CoordinationIndexPublicationPending, CoordinationIndexMemberLifecycle:
		return true
	}
	return false
}
