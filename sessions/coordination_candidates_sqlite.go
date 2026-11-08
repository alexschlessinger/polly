package sessions

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// These are source-table indexes, not materialized relation predicates. Schema
// 10 creates them and validation owns their shape. Changing a referenced
// task/snapshot is immediately visible without fanout.
// BLOB substring avoids SQLite TEXT substring's embedded-NUL truncation for IDs.
const candidateWorkflowIDSQL = `CAST(substr(CAST(id AS BLOB),1,instr(CAST(id AS BLOB),x'2f')-1) AS TEXT)`
const candidateDeliveryRefSQL = `coalesce(json_extract(payload_json,'$.delivery.ref'),'')`

type candidateSourceIndex struct{ name, columns, predicate string }

var candidateSourceIndexes = []candidateSourceIndex{
	{"integration_forget", `parent_id,id,length(payload_json)`, `kind='integration' AND coalesce(json_extract(payload_json,'$.successor'),'')='' AND coalesce(json_extract(payload_json,'$.status'),'') NOT IN ('applied','superseded')`},
	{"workflow_unsettled", `parent_id,id,length(payload_json)`, `kind='workflow' AND (coalesce(json_extract(payload_json,'$.status'),'')='running' OR coalesce(json_type(payload_json,'$.acknowledged'),'null')<>'true')`},
	{"workflow_running", `parent_id,id,length(payload_json)`, `kind='workflow' AND (coalesce(json_extract(payload_json,'$.status'),'')='running' OR coalesce(json_type(payload_json,'$.status'),'null') NOT IN ('text','null'))`},
	{"mail_settlement", `parent_id,coalesce(json_extract(payload_json,'$.to'),''),id,length(payload_json)`, `kind='mail' AND ((coalesce(json_extract(payload_json,'$.kind'),'')='request' AND coalesce(json_extract(payload_json,'$.replyID'),'')='') OR (coalesce(json_type(payload_json,'$.delivered'),'null')<>'true' AND (coalesce(json_extract(payload_json,'$.kind'),'')='reply' OR coalesce(json_extract(payload_json,'$.workflow'),'')<>'')))`},
	{"host_step", `parent_id,` + candidateWorkflowIDSQL + `,id,length(payload_json)`, `kind='workflow_step' AND (coalesce(json_extract(payload_json,'$.status'),'')='running' AND coalesce(json_extract(payload_json,'$.kind'),'') NOT IN ('agent','followup'))`},
	{"task_notice", `parent_id,coalesce(json_extract(payload_json,'$.task'),''),coalesce(json_extract(payload_json,'$.execution'),''),coalesce(json_extract(payload_json,'$.revision'),0),id,length(payload_json)`, `kind='mail'`},
	{"open_owner", `parent_id,coalesce(json_extract(payload_json,'$.owner'),''),id,length(payload_json)`, `kind='task' AND coalesce(json_extract(payload_json,'$.status'),'') NOT IN ('done','canceled')`},
	{"unresolved_apply", `parent_id,id,length(payload_json)`, `kind='apply' AND coalesce(json_extract(payload_json,'$.status'),'') IN ('applying','recovery_required')`},
	{"snapshot_tree", `parent_id,coalesce(json_extract(payload_json,'$.source'),''),coalesce(json_extract(payload_json,'$.tree'),''),id`, `kind='snapshot'`},
	{"done_snapshot", `parent_id,coalesce(json_extract(payload_json,'$.snapshot'),''),id,length(payload_json)`, `kind='task' AND coalesce(json_extract(payload_json,'$.status'),'')='done'`},
	{"context_step", `parent_id,` + candidateWorkflowIDSQL + `,json_extract(payload_json,'$.value'),id,length(payload_json)`, `kind='workflow_step' AND coalesce(json_extract(payload_json,'$.status'),'')='completed' AND coalesce(json_extract(payload_json,'$.kind'),'')='context' AND json_type(payload_json,'$.value')='text'`},
	{"followup_preparing", `parent_id,id,length(payload_json)`, `kind='followup' AND (json_type(payload_json,'$.refresh')='true' AND coalesce(json_extract(payload_json,'$.phase'),'')='preparing')`},
	{"followup_held", `parent_id,id,length(payload_json)`, `kind='followup' AND coalesce(json_extract(payload_json,'$.heldScratch'),'')<>''`},
	{"workflow_delivery", `parent_id,` + candidateDeliveryRefSQL + `,id,length(payload_json)`, `kind='task' AND coalesce(json_extract(payload_json,'$.delivery.via'),'')='workflow_step'`},
}

func (index candidateSourceIndex) indexName() string {
	return "swarm_candidates_" + index.name + "_idx"
}
func (index candidateSourceIndex) definition() string {
	return `ON swarm_records(` + index.columns + `) WHERE ` + index.predicate
}

func candidateIndex(name string) candidateSourceIndex {
	for _, index := range candidateSourceIndexes {
		if index.name == name {
			return index
		}
	}
	panic("unknown internal candidate index: " + name)
}

// candidateSelectionSQL returns keys and sizes before loading any payload into
// Go. INDEXED BY prevents accidental fallback to historical kind scans. The
// unresolved-owner query visits actual unresolved applies and their references;
// done-tree visits matching snapshots and done tasks pointing at those snapshots.
// Their work is proportional to those candidate partitions, not closed history.
func candidateSelectionSQL(parent []byte, q CoordinationCandidateQuery, spec candidateSpec, after string) (string, []any) {
	name := ""
	where := ""
	args := []any{parent}
	switch q.Selector {
	case CandidateIntegrationForget:
		name = "integration_forget"
	case CandidateWorkflowUnsettled:
		name = "workflow_unsettled"
	case CandidateWorkflowRunning:
		name = "workflow_running"
	case CandidateMailSettlement:
		name, where = "mail_settlement", ` AND coalesce(json_extract(payload_json,'$.to'),'')=?`
		args = append(args, q.Key)
	case CandidateTaskNotice:
		name, where = "task_notice", ` AND coalesce(json_extract(payload_json,'$.task'),'')=? AND coalesce(json_extract(payload_json,'$.execution'),'')=? AND coalesce(json_extract(payload_json,'$.revision'),0)=?`
		revision, _ := strconv.ParseInt(spec.parts[2], 10, 64)
		args = append(args, spec.parts[0], spec.parts[1], revision)
	case CandidateWorkflowHostRunningStep:
		name, where = "host_step", ` AND `+candidateWorkflowIDSQL+`=?`
		args = append(args, q.Key)
	case CandidateWorkflowContextStep:
		name, where = "context_step", ` AND `+candidateWorkflowIDSQL+`=? AND json_extract(payload_json,'$.value')=?`
		args = append(args, spec.parts[0], spec.parts[1])
	case CandidateTaskOpenOwner:
		name, where = "open_owner", ` AND coalesce(json_extract(payload_json,'$.owner'),'')=?`
		args = append(args, q.Key)
	case CandidateFollowupPreparing:
		name = "followup_preparing"
	case CandidateFollowupHeldScratch:
		name = "followup_held"
	case CandidateTaskWorkflowDelivery:
		name, where = "workflow_delivery", ` AND `+candidateDeliveryRefSQL+`>=? AND `+candidateDeliveryRefSQL+`<?`
		// '/' is one byte; the half-open range is exactly prefix workflow+'/'.
		// Neither LIKE wildcard semantics nor TEXT length/substr is involved.
		args = append(args, q.Key+"/", q.Key+"0")
	case CandidateApplyUnresolvedOwner:
		index := candidateIndex("unresolved_apply")
		query := `SELECT a.id,length(a.payload_json) FROM swarm_records a INDEXED BY ` + index.indexName() + ` WHERE a.parent_id=? AND ` + strings.ReplaceAll(index.predicate, "payload_json", "a.payload_json") + ` AND EXISTS (SELECT 1 FROM json_each(a.payload_json,'$.tasks') ref CROSS JOIN swarm_records t WHERE t.parent_id=a.parent_id AND t.kind='task' AND t.id=json_extract(ref.value,'$.task') AND coalesce(json_extract(t.payload_json,'$.owner'),'')=?)`
		args = append(args, q.Key)
		if after != "" {
			query += ` AND a.id>?`
			args = append(args, after)
		}
		return query + ` ORDER BY a.id LIMIT ?`, append(args, q.Limit+1)
	case CandidateDoneTaskTree:
		snap, task := candidateIndex("snapshot_tree"), candidateIndex("done_snapshot")
		// CROSS JOIN fixes the index-supported loop order: matching source/tree
		// snapshots first, then exact done-task snapshot references, never all done tasks.
		// Unary + removes s.id's TEXT affinity without changing its value: the
		// expression index has no affinity, so SQLite otherwise seeks only parent.
		query := `SELECT t.id,length(t.payload_json) FROM swarm_records s INDEXED BY ` + snap.indexName() + ` CROSS JOIN swarm_records t INDEXED BY ` + task.indexName() + ` WHERE s.parent_id=? AND s.kind='snapshot' AND coalesce(json_extract(s.payload_json,'$.source'),'')=? AND coalesce(json_extract(s.payload_json,'$.tree'),'')=? AND t.parent_id=s.parent_id AND t.kind='task' AND coalesce(json_extract(t.payload_json,'$.status'),'')='done' AND coalesce(json_extract(t.payload_json,'$.snapshot'),'')=+s.id`
		args = append(args, spec.parts[0], spec.parts[1])
		if after != "" {
			query += ` AND t.id>?`
			args = append(args, after)
		}
		return query + ` ORDER BY t.id LIMIT ?`, append(args, q.Limit+1)
	}
	index := candidateIndex(name)
	query := `SELECT id,length(payload_json) FROM swarm_records INDEXED BY ` + index.indexName() + ` WHERE parent_id=? AND ` + index.predicate + where
	if after != "" {
		query += ` AND id>?`
		args = append(args, after)
	}
	return query + ` ORDER BY id LIMIT ?`, append(args, q.Limit+1)
}

func (r sqliteSemanticReader) QueryCoordinationCandidates(q CoordinationCandidateQuery) (RecordPage, error) {
	return r.CoordinationReader.(CoordinationCandidateReader).QueryCoordinationCandidates(q)
}

func (r *sqliteRecordTx) QueryCoordinationCandidates(q CoordinationCandidateQuery) (RecordPage, error) {
	if err := r.ready(); err != nil {
		return RecordPage{}, err
	}
	cursor, spec, err := candidatePageCursor(r, q)
	if err != nil {
		return RecordPage{}, err
	}
	if err := r.checkKind(spec.kind); err != nil {
		return RecordPage{}, err
	}
	query, args := candidateSelectionSQL(r.parent, q, spec, string(cursor.ID))
	type selectedRecord struct {
		id   string
		size int
	}
	var selected []selectedRecord
	nativeSQL(r.ctx)
	err = queryEach(r.ctx, r.conn, query, args, func(rows *sql.Rows) error {
		var item selectedRecord
		if err := rows.Scan(&item.id, &item.size); err != nil {
			return err
		}
		nativeRows(r.ctx, 1)
		selected = append(selected, item)
		return nil
	})
	if err != nil {
		return RecordPage{}, err
	}
	page := RecordPage{Total: -1}
	used := 0
	for _, item := range selected {
		if len(page.Records) == q.Limit || q.MaxBytes > 0 && len(page.Records) > 0 && item.size > q.MaxBytes-used {
			break
		}
		record, found, err := r.Get(RecordKey{spec.kind, item.id})
		if err != nil {
			return RecordPage{}, err
		}
		if !found {
			return RecordPage{}, fmt.Errorf("%w: candidate disappeared", ErrCoordinationFormat)
		}
		matches, err := candidateMatches(r, q, spec, record)
		if err != nil {
			return RecordPage{}, err
		}
		if !matches {
			return RecordPage{}, fmt.Errorf("%w: candidate source predicate differs for %s/%s", ErrCoordinationFormat, spec.kind, item.id)
		}
		page.Records = append(page.Records, record)
		used += len(record.Payload)
		if q.MaxBytes > 0 && used > q.MaxBytes {
			page.Oversized = true
			break
		}
	}
	if len(page.Records) > 0 && len(page.Records) < len(selected) {
		page.Next = candidatePageNext(cursor, page.Records[len(page.Records)-1].Key.ID)
	}
	return page, r.ready()
}
