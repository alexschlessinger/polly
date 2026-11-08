package sessions

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCoordinationCandidateQueryPlans(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	for _, test := range candidateCases() {
		t.Run(string(test.selector), func(t *testing.T) {
			q := CoordinationCandidateQuery{Selector: test.selector, Key: test.key, Limit: 2}
			spec, err := candidateQuerySpec(q)
			nativeMust(t, err)
			query, args := candidateSelectionSQL(s.id, q, spec, "after")
			var details []string
			nativeMust(t, queryEach(t.Context(), store.db, "EXPLAIN QUERY PLAN "+query, args, func(rows *sql.Rows) error {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					return err
				}
				details = append(details, detail)
				return nil
			}))
			plan := strings.Join(details, "; ")
			if !strings.Contains(plan, "SEARCH ") || !strings.Contains(plan, "USING INDEX swarm_candidates_") || strings.Contains(plan, "SCAN swarm_records") || strings.Contains(plan, "SCAN a ") || strings.Contains(plan, "SCAN t ") || strings.Contains(plan, "SCAN s ") {
				t.Fatalf("candidate partition not sought: %s", plan)
			}
			if test.selector == CandidateDoneTaskTree && (!strings.Contains(plan, "swarm_candidates_snapshot_tree_idx (parent_id=? AND <expr>=? AND <expr>=?)") || !strings.Contains(plan, "swarm_candidates_done_snapshot_idx (parent_id=? AND <expr>=? AND id>?)")) {
				t.Fatalf("done-tree scanned history instead of exact snapshots: %s", plan)
			}
			if test.selector == CandidateApplyUnresolvedOwner && !strings.Contains(plan, "(parent_id=? AND kind=? AND id=?)") {
				t.Fatalf("unresolved apply scanned task history instead of reference point reads: %s", plan)
			}
			if test.selector != CandidateDoneTaskTree && test.selector != CandidateTaskWorkflowDelivery && strings.Contains(plan, "TEMP B-TREE") {
				t.Fatalf("ordinary candidate page sorted partition: %s", plan)
			}
			t.Log(plan)
		})
	}
}

func TestCoordinationCandidateIndexesInSchemaV10(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v10.db")
	store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	nativeMust(t, err)
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	_, err = s.UpdateRecords(t.Context(), func(tx CoordinationTx) error { return candidatePutAll(tx, candidateTestRecords()) })
	nativeMust(t, err)
	id := s.ViewID()
	var stamp, version, relations int64
	nativeMust(t, store.db.QueryRow(`SELECT coordination_stamp FROM sessions WHERE id=?`, s.id).Scan(&stamp))
	nativeMust(t, store.db.QueryRow(`SELECT count(*) FROM swarm_relations`).Scan(&relations))
	nativeMust(t, s.Close())
	nativeMust(t, store.Close())
	reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	nativeMust(t, err)
	defer reopened.Close()
	nativeMust(t, reopened.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	if version != 10 {
		t.Fatalf("candidate indexes bumped schema: %d", version)
	}
	for _, index := range candidateSourceIndexes {
		var statement string
		if err := reopened.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, index.indexName()).Scan(&statement); err != nil {
			t.Fatalf("index %s missing from schema 10: %v", index.indexName(), err)
		}
	}
	var afterStamp, afterRelations int64
	nativeMust(t, reopened.db.QueryRow(`SELECT coordination_stamp FROM sessions WHERE id=?`, s.id).Scan(&afterStamp))
	nativeMust(t, reopened.db.QueryRow(`SELECT count(*) FROM swarm_relations`).Scan(&afterRelations))
	if afterStamp != stamp || afterRelations != relations {
		t.Fatalf("complete v10 rebuilt relation state: stamps %d/%d relations %d/%d", stamp, afterStamp, relations, afterRelations)
	}
	nativeMust(t, reopened.ReadRecordsView(t.Context(), id, func(r CoordinationReader) error {
		for _, test := range candidateCases() {
			page, err := QueryCoordinationCandidates(r, CoordinationCandidateQuery{Selector: test.selector, Key: test.key, Limit: 100})
			nativeMust(t, err)
			if !reflect.DeepEqual(lifecycleKeys(page), test.want) {
				t.Fatalf("reopen %s: %+v", test.selector, page)
			}
		}
		return nil
	}))
}

func TestCoordinationCandidateIndexesSchema9Migration(t *testing.T) {
	path, id := nativeSchema9Fixture(t, false)
	raw, err := sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
	nativeMust(t, err)
	parent, err := hex.DecodeString(id)
	nativeMust(t, err)
	_, err = raw.Exec(`INSERT INTO swarm_records(parent_id,kind,id,payload_json) VALUES(?,'workflow','running',?)`, parent, []byte(`{"status":"running"}`))
	nativeMust(t, err)
	nativeMust(t, raw.Close())
	store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	nativeMust(t, err)
	defer store.Close()
	for _, index := range candidateSourceIndexes {
		var statement string
		if err := store.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, index.indexName()).Scan(&statement); err != nil {
			t.Fatalf("migration did not create %s: %v", index.indexName(), err)
		}
	}
	nativeMust(t, store.ReadRecordsView(t.Context(), id, func(r CoordinationReader) error {
		page, err := QueryCoordinationCandidates(r, CoordinationCandidateQuery{Selector: CandidateWorkflowRunning, Limit: 1})
		nativeMust(t, err)
		if !reflect.DeepEqual(lifecycleKeys(page), []string{"running"}) {
			t.Fatalf("migration failed to build candidate index: %+v", page)
		}
		return nil
	}))
}

func TestCoordinationCandidateIndexesRejectWrongDefinitions(t *testing.T) {
	for _, alter := range []string{"wrong predicate", "wrong collation", "wrong expression", "wrong unique", "case only"} {
		t.Run(alter, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wrong.db")
			store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
			nativeMust(t, err)
			nativeMust(t, store.Close())
			raw, err := sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
			nativeMust(t, err)
			index := candidateIndex("task_notice")
			_, err = raw.Exec(`DROP INDEX ` + index.indexName())
			nativeMust(t, err)
			definition := index.definition()
			switch alter {
			case "wrong predicate":
				definition = strings.Replace(definition, "kind='mail'", "kind='task'", 1)
			case "wrong collation":
				definition = strings.Replace(definition, ",id,", ",id COLLATE NOCASE,", 1)
			case "wrong expression":
				definition = strings.Replace(definition, "$.execution", "$.Execution", 1)
			case "case only":
				definition = strings.ReplaceAll(strings.ReplaceAll(definition, "ON ", "on "), " WHERE ", " where ")
			}
			create := `CREATE INDEX `
			if alter == "wrong unique" {
				create = `CREATE UNIQUE INDEX `
			}
			_, err = raw.Exec(create + index.indexName() + ` ` + definition)
			nativeMust(t, err)
			nativeMust(t, raw.Close())
			store, err = OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
			if alter == "case only" {
				nativeMust(t, err)
				nativeMust(t, store.Close())
				return
			}
			if err == nil {
				store.Close()
				t.Fatal("wrong physical index accepted")
			}
			if !errors.Is(err, ErrStoreCorrupt) {
				t.Fatalf("deterministic physical mismatch was not typed corruption: %v", err)
			}
			if !strings.Contains(err.Error(), index.indexName()) {
				t.Fatalf("lost exact index diagnosis: %v", err)
			}
		})
	}
}

// Hides only optional capability: fallback must use this callback's reader, not
// reopen a backend or recover indexing from the store that created it.
type candidateFallbackReader struct{ CoordinationReader }

func TestCoordinationCandidateSelectedMalformedAndFallback(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	nativePut(t, s, "workflow", "running", `{"status":"running"}`)
	// Source corruption retains valid JSON and a candidate predicate, but the
	// selected typed boolean is invalid. Both readers must reject, not omit it.
	_, err := store.db.Exec(`UPDATE swarm_records SET payload_json=? WHERE parent_id=? AND kind='workflow'`, []byte(`{"status":"running","acknowledged":"yes"}`), s.id)
	nativeMust(t, err)
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		for _, reader := range []CoordinationReader{r, candidateFallbackReader{r}} {
			_, err := QueryCoordinationCandidates(reader, CoordinationCandidateQuery{Selector: CandidateWorkflowUnsettled, Limit: 1})
			if !errors.Is(err, ErrCoordinationFormat) {
				t.Fatalf("malformed candidate disappeared: %v", err)
			}
		}
		return nil
	}))
}

func TestCoordinationCandidateEmptyAndInvalidRequests(t *testing.T) {
	for _, native := range []bool{false, true} {
		access := lifecycleAccess(t, native)
		nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
			for _, test := range candidateCases() {
				q := CoordinationCandidateQuery{Selector: test.selector, Key: test.key, Limit: 1}
				page, err := QueryCoordinationCandidates(r, q)
				nativeMust(t, err)
				if len(page.Records) != 0 || page.Total != -1 || page.Next != "" {
					t.Fatalf("empty %+v", page)
				}
				q.Limit = 0
				if _, err := QueryCoordinationCandidates(r, q); !errors.Is(err, ErrCoordinationRequest) {
					t.Fatalf("zero limit accepted: %v", err)
				}
			}
			for _, q := range []CoordinationCandidateQuery{
				{Selector: CandidateTaskNotice, Key: CoordinationIndexKey("t", "e", "01"), Limit: 1},
				{Selector: CandidateDoneTaskTree, Key: "not a tuple", Limit: 1},
				{Selector: CandidateWorkflowRunning, Key: "extra", Limit: 1},
				{Selector: CandidateWorkflowRunning, Limit: 1, MaxBytes: -1},
			} {
				if _, err := QueryCoordinationCandidates(r, q); !errors.Is(err, ErrCoordinationRequest) {
					t.Fatalf("invalid query accepted: %+v %v", q, err)
				}
			}
			if _, err := QueryCoordinationCandidates(r, CoordinationCandidateQuery{Selector: "unknown", Limit: 1}); !errors.Is(err, ErrCoordinationSelector) {
				t.Fatalf("unknown selector: %v", err)
			}
			return nil
		}))
	}
}

func TestCoordinationCandidateSemanticSelectorKinds(t *testing.T) {
	for _, native := range []bool{false, true} {
		access := lifecycleAccess(t, native)
		_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
			return candidatePutAll(tx, map[RecordKey]string{{"task", "task"}: `{"status":"running"}`, {"apply", "apply"}: `{"status":"applying"}`})
		})
		nativeMust(t, err)
		nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
			for _, test := range []struct {
				index CoordinationIndex
				kind  string
			}{{CoordinationIndexTaskOpen, "task"}, {CoordinationIndexApplyUncertain, "apply"}} {
				if selectorKind(RevisionScope{Index: test.index}) != test.kind {
					t.Fatalf("wrong selector kind %s", test.index)
				}
				page, err := r.Query(RecordQuery{Index: test.index, Kind: test.kind, Limit: 1})
				nativeMust(t, err)
				if len(page.Records) != 1 || page.Records[0].Key.Kind != test.kind {
					t.Fatalf("selector %+v", page)
				}
			}
			return nil
		}))
	}
}

func TestCoordinationCandidatesOpaquePartitionAndLiveCursor(t *testing.T) {
	for _, native := range []bool{false, true} {
		access := lifecycleAccess(t, native)
		key := "report\x00☃"
		_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
			for _, id := range []string{key + "/0", key + "/1"} {
				if err := tx.Put(RecordKey{"workflow_step", id}, json.RawMessage(`{"status":"running","kind":"context"}`)); err != nil {
					return err
				}
			}
			return nil
		})
		nativeMust(t, err)
		q := CoordinationCandidateQuery{Selector: CandidateWorkflowHostRunningStep, Key: key, Limit: 1}
		nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
			page, err := QueryCoordinationCandidates(r, q)
			q.After = page.Next
			return err
		}))
		if q.After == "" {
			t.Fatal("missing cursor")
		}
		_, err = access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
			if err := tx.Delete(RecordKey{"workflow_step", key + "/0"}); err != nil {
				return err
			}
			return tx.Put(RecordKey{"workflow_step", key + "/2"}, json.RawMessage(`{"status":"running","kind":"context"}`))
		})
		nativeMust(t, err)
		nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
			other := q
			other.Key = "other"
			if _, err := QueryCoordinationCandidates(r, other); !errors.Is(err, ErrCoordinationCursor) {
				t.Fatalf("foreign key cursor: %v", err)
			}
			q.Limit = 2
			page, err := QueryCoordinationCandidates(r, q)
			nativeMust(t, err)
			if !reflect.DeepEqual(lifecycleKeys(page), []string{key + "/1", key + "/2"}) {
				t.Fatalf("live opaque-key cursor %+v", page)
			}
			return nil
		}))
		other := lifecycleAccess(t, native)
		nativeMust(t, other.ReadRecords(t.Context(), func(r CoordinationReader) error {
			// Legacy fixture roots are identical; explicitly mask identity below.
			_, err := QueryCoordinationCandidates(candidateForeignRootReader{r}, q)
			if !errors.Is(err, ErrCoordinationCursor) {
				t.Fatalf("foreign root cursor: %v", err)
			}
			return nil
		}))
	}
}

type candidateForeignRootReader struct{ CoordinationReader }

func (r candidateForeignRootReader) Identity() (CoordinationIdentity, error) {
	i, err := r.CoordinationReader.Identity()
	i.ParentID = "foreign-root"
	return i, err
}
