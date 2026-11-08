package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/coordsem"
)

type legacySemanticReader struct{ CoordinationReader }

func (legacySemanticReader) SemanticCapabilities() CoordinationSemanticCapabilities {
	return CoordinationSemanticCapabilities{}
}

type legacySemanticMark struct {
	Root    string
	Version int
	Entries map[string]json.RawMessage
}

func (r legacySemanticReader) snapshot() (legacySemanticMark, map[string]bool, error) {
	identity, err := r.Identity()
	if err != nil {
		return legacySemanticMark{}, nil, err
	}
	memory := &memoryDerivedStore{ctx: context.Background(), records: map[string]map[string]json.RawMessage{}, relations: map[RecordKey]map[coordindex.Relation]coordindex.Order{}}
	q := RecordQuery{Limit: 128}
	for {
		page, err := r.Query(q)
		if err != nil {
			return legacySemanticMark{}, nil, err
		}
		for _, record := range page.Records {
			if memory.records[record.Key.Kind] == nil {
				memory.records[record.Key.Kind] = map[string]json.RawMessage{}
			}
			memory.records[record.Key.Kind][record.Key.ID] = record.Payload
			rels, order, err := coordindex.Extract(record.Key.Kind, record.Key.ID, record.Payload)
			if err != nil {
				return legacySemanticMark{}, nil, err
			}
			for _, rel := range rels {
				if err := memory.set(derivedItem{record.Key, order}, rel, true); err != nil {
					return legacySemanticMark{}, nil, err
				}
			}
		}
		if page.Next == "" {
			break
		}
		q.After = page.Next
	}
	out := legacySemanticMark{Root: identity.ParentID, Version: coordsem.Version, Entries: map[string]json.RawMessage{}}
	controlled := map[string]bool{}
	source := semanticSource{store: memory, parent: identity.ParentID}
	for _, kind := range []string{"task", "member", "workflow"} {
		for id := range memory.records[kind] {
			if _, err := r.Identity(); err != nil {
				return legacySemanticMark{}, nil, err
			}
			value, mask, err := source.news(RecordKey{kind, id})
			if err != nil {
				return legacySemanticMark{}, nil, err
			}
			key := coordindex.Key(kind, id)
			out.Entries[key] = value
			controlled[key] = mask
		}
	}
	return out, controlled, nil
}
func (r legacySemanticReader) SemanticMark() (SemanticMark, error) {
	mark, _, err := r.snapshot()
	if err != nil {
		return "", err
	}
	return SemanticMark(semanticEncode(mark)), nil
}
func (r legacySemanticReader) SemanticChanges(q SemanticComparison) (SemanticChanges, error) {
	if !validSemanticMode(q.Mode) {
		return SemanticChanges{}, fmt.Errorf("%w: semantic comparison mode", ErrCoordinationRequest)
	}
	var before legacySemanticMark
	if err := semanticDecode(string(q.Baseline), &before); err != nil {
		return SemanticChanges{}, err
	}
	current, controlled, err := r.snapshot()
	if err != nil {
		return SemanticChanges{}, err
	}
	if before.Root != current.Root || before.Version != coordsem.Version || before.Entries == nil {
		return SemanticChanges{}, fmt.Errorf("%w: incompatible legacy semantic mark", ErrCoordinationCursor)
	}
	if q.After != "" {
		var after semanticContinuation
		if err := semanticDecode(q.After, &after); err != nil {
			return SemanticChanges{}, err
		}
		if after.Baseline != q.Baseline || after.Mode != q.Mode {
			return SemanticChanges{}, fmt.Errorf("%w: incompatible semantic continuation", ErrCoordinationCursor)
		}
	}
	out := SemanticChanges{Observed: SemanticMark(semanticEncode(current))}
	for key, value := range current.Entries {
		if q.Mode == SemanticParentNews && controlled[key] {
			continue
		}
		if !bytes.Equal(value, before.Entries[key]) {
			out.Different = true
			return out, nil
		}
	}
	if q.Mode == SemanticCoordination {
		for key := range before.Entries {
			if current.Entries[key] == nil {
				out.Different = true
				return out, nil
			}
		}
	}
	out.Continue = semanticEncode(semanticContinuation{q.Baseline, q.Mode, out.Observed})
	return out, nil
}
