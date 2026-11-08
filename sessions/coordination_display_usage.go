package sessions

import "database/sql"

// DisplayMemberUsage contains exact sums across every execution of a member,
// including old follow-ups and released workspaces. Missing counts remain
// distinct from reported zero. An execution with no samples or counts has not
// reported a response and contributes neither counts nor missing flags.
// Label and Brief retain the inputs to frontend label formatting, not results,
// intents, transcripts, workspace state, or mutation authority.
type DisplayMemberUsage struct {
	Member, Label, Brief        string
	InputTokens, OutputTokens   int
	InputMissing, OutputMissing bool
}

// CoordinationUsageReader is an optional callback-local read projection. Native
// implementations aggregate maintained compact execution descriptors, never
// load original execution payloads or unrelated family records. Returned values
// are owned and ordered by member identity, including orphan executions.
type CoordinationUsageReader interface {
	DisplayUsage() ([]DisplayMemberUsage, error)
}

func (r *sqliteRecordTx) DisplayUsage() ([]DisplayMemberUsage, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := r.checkKind("execution"); err != nil {
		return nil, err
	}
	var out []DisplayMemberUsage
	nativeSQL(r.ctx)
	err := queryEach(r.ctx, r.conn, `WITH usage AS (
 SELECT subject AS member,
 sum(coalesce(json_extract(value,'$.usage.inputTokens'),0)) AS input,
 sum(coalesce(json_extract(value,'$.usage.outputTokens'),0)) AS output,
 max(CASE WHEN (coalesce(json_extract(value,'$.usage.samples'),0) != 0
 OR json_extract(value,'$.usage.inputTokens') IS NOT NULL
 OR json_extract(value,'$.usage.outputTokens') IS NOT NULL)
 AND json_extract(value,'$.usage.inputTokens') IS NULL THEN 1 ELSE 0 END) AS input_missing,
 max(CASE WHEN (coalesce(json_extract(value,'$.usage.samples'),0) != 0
 OR json_extract(value,'$.usage.inputTokens') IS NOT NULL
 OR json_extract(value,'$.usage.outputTokens') IS NOT NULL)
 AND json_extract(value,'$.usage.outputTokens') IS NULL THEN 1 ELSE 0 END) AS output_missing
 FROM swarm_display_sources WHERE parent_id=? AND kind='execution' GROUP BY subject
 ) SELECT u.member,coalesce(json_extract(m.value,'$.label'),''),
 coalesce(nullif(json_extract(e.value,'$.request.task'),''),nullif(json_extract(t.value,'$.description'),''),json_extract(m.value,'$.name'),''),
 u.input,u.output,u.input_missing,u.output_missing
 FROM usage u
 LEFT JOIN swarm_display_sources m ON m.parent_id=? AND m.kind='member' AND m.id=u.member
 LEFT JOIN swarm_display_sources e ON e.parent_id=m.parent_id AND e.kind='execution' AND e.id=json_extract(m.value,'$.execution')
 LEFT JOIN swarm_display_sources t ON t.parent_id=m.parent_id AND t.kind='task' AND t.id=json_extract(m.value,'$.task')
 ORDER BY u.member`, []any{r.parent, r.parent}, func(rows *sql.Rows) error {
		var v DisplayMemberUsage
		if err := rows.Scan(&v.Member, &v.Label, &v.Brief, &v.InputTokens, &v.OutputTokens, &v.InputMissing, &v.OutputMissing); err != nil {
			return err
		}
		nativeRows(r.ctx, 1)
		nativePayload(r.ctx, []byte(v.Member+v.Label+v.Brief))
		out = append(out, v)
		return nil
	})
	return out, err
}
