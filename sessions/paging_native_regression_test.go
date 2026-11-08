package sessions

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRuntimeStorageScaleNativeSummaryContinuationFullKeySeek(t *testing.T) {
	for _, filter := range []string{"unfiltered", "parent", "roots"} {
		t.Run(filter, func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			root := nativeHandle(t, store, "root")
			var parent []byte
			var parentID *string
			if filter != "unfiltered" {
				id := ""
				parentID = &id
				if filter == "parent" {
					id = root.ViewID()
					parent = root.id
				}
			}
			// Seed a deep equal-time group and adjacent timestamp ranges. There is
			// no payload growth or timing threshold in this index-seek regression.
			nativeMust(t, store.withWrite(t.Context(), func(conn *sql.Conn) error {
				for i := 0; i < 1026; i++ {
					used := 2
					if i == 0 {
						used = 3
					} else if i == 1025 {
						used = 1
					}
					if _, err := conn.ExecContext(t.Context(), `INSERT INTO sessions(id,name,retention,created_ns,updated_ns,settings_json,parent_id) VALUES(randomblob(16),?,'named',1,?,?,?)`, fmt.Sprintf("archive-%04d", i), used, []byte(`{}`), parent); err != nil {
						return err
					}
				}
				return nil
			}))
			page, err := store.ListSummaryPage(t.Context(), SummaryQuery{ParentID: parentID, Limit: 1000})
			nativeMust(t, err)
			if len(page.Summaries) != 1000 || page.Next == "" {
				t.Fatal("fixture page", len(page.Summaries))
			}
			var cursor nativeSummaryCursor
			data, err := base64.RawURLEncoding.DecodeString(page.Next)
			nativeMust(t, err)
			nativeMust(t, json.Unmarshal(data, &cursor))
			queries := summaryPageQueries(parentID != nil, parent, cursor, time.Now().UnixNano())
			if len(queries) != 3 {
				t.Fatalf("continuation ranges=%d", len(queries))
			}
			index := "sessions_summary_idx"
			prefix := 0
			if parentID != nil {
				index = "sessions_parent_summary_idx"
				prefix = 1
			}
			for i, query := range queries {
				assertSummaryRangeSeek(t, store, query, index, prefix+3-i)
			}
			// Cross range boundaries in one page and with single-row pages. The
			// returned stable IDs must exactly match the whole ordered read.
			var got []string
			for _, summary := range page.Summaries {
				got = append(got, summary.ID)
			}
			for page.Next != "" {
				page, err = store.ListSummaryPage(t.Context(), SummaryQuery{ParentID: parentID, After: page.Next, Limit: 1})
				nativeMust(t, err)
				if len(page.Summaries) != 1 || len(got) > 1027 {
					t.Fatal("empty page or cursor loop", len(got))
				}
				got = append(got, page.Summaries[0].ID)
			}
			where := ""
			var args []any
			if filter == "roots" {
				where = " WHERE parent_id IS NULL"
			} else if filter == "parent" {
				where, args = " WHERE parent_id=?", []any{parent}
			}
			position := 0
			nativeMust(t, queryEach(t.Context(), store.db, `SELECT lower(hex(id)) FROM sessions`+where+` ORDER BY updated_ns DESC,name,id`, args, func(rows *sql.Rows) error {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				if position >= len(got) || got[position] != id {
					t.Fatalf("page position %d: want %s", position, id)
				}
				position++
				return nil
			}))
			if position != len(got) {
				t.Fatalf("returned %d rows, want %d", len(got), position)
			}
		})
	}
}

func assertSummaryRangeSeek(t *testing.T, store *SQLiteStore, query summaryPageQuery, index string, keys int) {
	t.Helper()
	var rootPage int
	nativeMust(t, store.db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name=?`, index).Scan(&rootPage))
	indexCursor := -1
	keyedSeek := false
	var plan []string
	nativeMust(t, queryEach(t.Context(), store.db, "EXPLAIN "+query.sql, append(query.args, 2), func(rows *sql.Rows) error {
		var addr, p1, p2, p3, p5 int
		var opcode string
		var p4, comment sql.NullString
		if err := rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			return err
		}
		plan = append(plan, fmt.Sprintf("%d %s %d %d %d %s", addr, opcode, p1, p2, p3, p4.String))
		if opcode == "OpenRead" && p2 == rootPage {
			indexCursor = p1
		}
		if opcode == "SeekGT" && p1 == indexCursor && p4.String == strconv.Itoa(keys) {
			keyedSeek = true
		}
		if opcode == "SorterOpen" {
			t.Errorf("range requires an unbounded sort: %s", query.sql)
		}
		return nil
	}))
	if !keyedSeek {
		t.Fatalf("missing strict %d-key seek on %s (not a returned-row count):\n%s\n%s", keys, index, query.sql, strings.Join(plan, "\n"))
	}
	t.Logf("exact VM: %s SeekGT keys=%d", index, keys)
}

func TestNativeSpawnIndexLiteralValidation(t *testing.T) {
	for _, tc := range []struct {
		name, ddl string
		valid     bool
	}{
		{"path_case", `CREATE INDEX sessions_spawn_idx ON sessions(parent_id,json_extract(settings_json,'$.spawncallid'))`, false},
		{"path_space", `CREATE INDEX sessions_spawn_idx ON sessions(parent_id,json_extract(settings_json,'$.spawn CallID'))`, false},
		{"unique", `CREATE UNIQUE INDEX sessions_spawn_idx ON sessions(parent_id,json_extract(settings_json,'$.spawnCallID'))`, false},
		{"keyword_case_and_spacing", `create index sessions_spawn_idx ON SESSIONS ( parent_id, JSON_EXTRACT ( settings_json, '$.spawnCallID' ) )`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, path := openTestStore(t, ModeDisk, nil, 0)
			nativeMust(t, store.Close())
			raw, err := sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
			nativeMust(t, err)
			_, err = raw.Exec(`DROP INDEX sessions_spawn_idx; ` + tc.ddl)
			nativeMust(t, err)
			nativeMust(t, raw.Close())
			reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
			if reopened != nil {
				defer reopened.Close()
			}
			if (err == nil) != tc.valid {
				t.Fatalf("schema open error=%v, valid=%t: %s", err, tc.valid, tc.ddl)
			}
		})
	}
}

func TestCompactSchemaSQLPreservesQuotedTokens(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`JSON_EXTRACT ( settings_json, '$.spawnCallID' )`, `json_extract(settings_json,'$.spawnCallID')`},
		{"CHECK ( x = 'A \tB\nC' )", "check(x='A \tB\nC')"},
		{`CHECK(x='It''s Upper')`, `check(x='It''s Upper')`},
		{`CHECK("Quoted Column"='Value')`, `check("Quoted Column"='Value')`},
		{"CHECK(`Quoted Column`='Value')", "check(`Quoted Column`='Value')"},
		{`CHECK([Quoted Column]='Value')`, `check([Quoted Column]='Value')`},
	} {
		if got := compactSchemaSQL(tc.input); got != tc.want {
			t.Fatalf("compact %q = %q, want %q", tc.input, got, tc.want)
		}
	}
}
