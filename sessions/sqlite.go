package sessions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	_ "modernc.org/sqlite"
)

const (
	schemaVersion            = 2
	artifactChunkSize        = 1 << 20
	journalSizeLimit         = 64 << 20
	boundedVacuumPages       = 128
	retentionNamed           = "named"
	retentionAuto            = "auto"
	defaultCleanupInterval   = time.Hour
	defaultLeaseHeartbeat    = 5 * time.Second
	defaultLeaseStaleAfter   = 20 * time.Second
	defaultLeaseAcquireLimit = 10 * time.Second
	defaultLeaseRetry        = 100 * time.Millisecond
	journalConfigRetryLimit  = 10 * time.Second
	journalConfigRetryDelay  = 10 * time.Millisecond
)

// Variables make the otherwise fixed production lease timings practical to
// exercise in focused package tests.
var (
	leaseHeartbeatInterval = defaultLeaseHeartbeat
	leaseStaleAfter        = defaultLeaseStaleAfter
	leaseAcquireTimeout    = defaultLeaseAcquireLimit
	leaseRetryInterval     = defaultLeaseRetry
	cleanupInterval        = defaultCleanupInterval
)

var (
	ErrSessionInUse     = errors.New("session is in use")
	ErrSessionLeaseLost = errors.New("session lease lost")
	ErrStoreClosed      = errors.New("session store is closed")
	ErrArtifactCorrupt  = errors.New("artifact is corrupt")
)

// SQLiteStore is the common implementation used for both ephemeral and
// persistent sessions.
type SQLiteStore struct {
	db       *sql.DB
	mode     StoreMode
	path     string
	defaults *Metadata
	autoTTL  time.Duration
	cleanup  time.Duration

	ctx    context.Context
	cancel context.CancelCauseFunc
	wg     sync.WaitGroup

	closed atomic.Bool
	mu     sync.Mutex
	open   map[*sqliteSession]struct{}
}

type sqliteSession struct {
	store      *SQLiteStore
	id         []byte
	ownerToken []byte
	artifacts  *sqliteArtifactStore

	ctx    context.Context
	cancel context.CancelCauseFunc

	heartbeatDone chan struct{}
	closeDone     chan struct{}
	closeMu       sync.Mutex
	closeErr      error

	expiresNS atomic.Int64
	closed    atomic.Bool
}

type sessionSnapshot struct {
	name      string
	createdNS int64
	updatedNS int64
	ttlNS     int64
	retention string
	settings  []byte
	nextSeq   int64
}

// OpenStore opens and validates a unified SQLite store. Disk mode never
// imports, changes, or removes the legacy contexts directory.
func OpenStore(config StoreConfig) (*SQLiteStore, error) {
	if config.Mode != ModeMemory && config.Mode != ModeDisk {
		return nil, fmt.Errorf("invalid session store mode %d", config.Mode)
	}
	if config.AutoSessionTTL < 0 {
		return nil, fmt.Errorf("auto-session TTL cannot be negative")
	}
	if config.DefaultMetadata != nil && config.DefaultMetadata.TTL < 0 {
		return nil, fmt.Errorf("default session TTL cannot be negative")
	}
	if config.CleanupInterval < 0 {
		return nil, fmt.Errorf("cleanup interval cannot be negative")
	}

	dsnPath := config.Path
	if config.Mode == ModeDisk {
		if strings.TrimSpace(config.Path) == "" {
			return nil, fmt.Errorf("disk session store path is required")
		}
		absolutePath, err := filepath.Abs(config.Path)
		if err != nil {
			return nil, fmt.Errorf("resolve session database path: %w", err)
		}
		dsnPath = absolutePath
		if err := prepareDiskStore(absolutePath); err != nil {
			return nil, err
		}
	} else {
		raw, err := randomBytes(16)
		if err != nil {
			return nil, fmt.Errorf("create memory database identity: %w", err)
		}
		dsnPath = "polly-" + hex.EncodeToString(raw)
	}
	dsn := sqliteDataSource(config.Mode, dsnPath)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open session database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	sweep := cleanupInterval
	if config.CleanupInterval > 0 {
		sweep = config.CleanupInterval
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	store := &SQLiteStore{
		db:       db,
		mode:     config.Mode,
		path:     dsnPath,
		defaults: cloneMetadata(config.DefaultMetadata),
		autoTTL:  config.AutoSessionTTL,
		cleanup:  sweep,
		ctx:      ctx,
		cancel:   cancel,
		open:     make(map[*sqliteSession]struct{}),
	}
	if store.defaults == nil {
		store.defaults = &Metadata{}
	}

	if err := store.configureAndMigrate(ctx); err != nil {
		cancel(err)
		_ = db.Close()
		return nil, err
	}
	if config.Mode == ModeDisk {
		if err := protectSQLiteFiles(dsnPath); err != nil {
			cancel(err)
			_ = db.Close()
			return nil, err
		}
	}
	if err := store.Expire(ctx); err != nil {
		cancel(err)
		_ = db.Close()
		return nil, fmt.Errorf("expire sessions at startup: %w", err)
	}
	store.wg.Add(1)
	go store.cleanupLoop()
	return store, nil
}

// prepareDiskStore closes the first-open exposure window without changing the
// process-wide umask or the permissions of a caller-owned parent directory.
// SQLite derives new WAL and SHM modes from the already-existing main database,
// so that file must be private before SQLite is allowed to open it.
func prepareDiskStore(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create session database directory: %w", err)
	}
	for {
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			// O_EXCL reports an existing directory as ErrExist on POSIX but
			// as an "is a directory" error on Windows; classify any existing
			// path through the same refusal logic.
			if !errors.Is(err, os.ErrExist) {
				if _, statErr := os.Lstat(path); statErr != nil {
					return fmt.Errorf("create session database securely: %w", err)
				}
			}
			err = protectExistingSQLiteFile(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			break
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return fmt.Errorf("protect session database: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close session database after secure creation: %w", err)
		}
		break
	}
	// A process killed during an older vulnerable first open may have left
	// permissive sidecars behind. Protect or reject them before SQLite can read,
	// map, or follow any of those paths. The post-open pass still covers
	// sidecars created by this process.
	return protectSQLiteFiles(path)
}

func protectSQLiteFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if err := protectExistingSQLiteFile(candidate); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
	}
	return nil
}

func protectExistingSQLiteFile(path string) error {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symbolic-link SQLite file %q", path)
	}
	if !pathInfo.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular SQLite file %q", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open SQLite file %q for protection: %w", path, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened SQLite file %q: %w", path, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return fmt.Errorf("SQLite file %q changed while opening", path)
	}
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("protect SQLite file %q: %w", path, err)
	}
	return nil
}

func (s *SQLiteStore) configureAndMigrate(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open SQLite connection: %w", err)
	}
	defer conn.Close()

	var quickCheck string
	if err := conn.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&quickCheck); err != nil {
		return fmt.Errorf("check session database: %w", err)
	}
	if quickCheck != "ok" {
		return fmt.Errorf("session database integrity check failed: %s", quickCheck)
	}

	// Every would-be migrator configures auto-vacuum before competing for the
	// schema write lock. Whichever connection wins therefore configures the
	// empty database before creating its first table. On existing databases this
	// is a no-op unless a VACUUM is run, so validation below still rejects an
	// incompatible setting.
	if s.mode == ModeDisk {
		if _, err := conn.ExecContext(ctx, "PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
			return fmt.Errorf("configure incremental auto-vacuum: %w", err)
		}
	}
	if err := configureJournal(ctx, conn, s.mode); err != nil {
		return err
	}
	if err := migrateSchema(ctx, conn); err != nil {
		return err
	}

	if err := validateSchemaV1(ctx, conn); err != nil {
		return err
	}
	foreignKeys, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("check session database foreign keys: %w", err)
	}
	violated := foreignKeys.Next()
	foreignKeyErr := foreignKeys.Err()
	if err := foreignKeys.Close(); err != nil && foreignKeyErr == nil {
		foreignKeyErr = err
	}
	if foreignKeyErr != nil {
		return fmt.Errorf("check session database foreign keys: %w", foreignKeyErr)
	}
	if violated {
		return fmt.Errorf("session database contains foreign-key violations")
	}
	if s.mode == ModeDisk {
		var autoVacuum int
		if err := conn.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&autoVacuum); err != nil {
			return fmt.Errorf("read session database auto-vacuum mode: %w", err)
		}
		if autoVacuum != 2 {
			return fmt.Errorf("session database auto-vacuum mode is %d, want incremental", autoVacuum)
		}
	}
	return nil
}

func sqliteDataSource(mode StoreMode, path string) string {
	query := make(url.Values)
	query.Set("_foreign_keys", "on")
	query.Set("_busy_timeout", "10000")
	query.Set("_synchronous", "FULL")
	query.Set("_txlock", "immediate")
	query.Add("_pragma", "trusted_schema(OFF)")
	uri := url.URL{Scheme: "file"}
	if mode == ModeMemory {
		uri.Opaque = path
		query.Set("mode", "memory")
		query.Set("cache", "shared")
	} else {
		normalized := filepath.ToSlash(path)
		if !isWindowsDrivePath(normalized) && !filepath.IsAbs(path) {
			if absolute, err := filepath.Abs(path); err == nil {
				normalized = filepath.ToSlash(absolute)
			}
		}
		// Keep drive letters in the URI path. Without the leading slash,
		// net/url renders C:/... as file://C:/..., where SQLite interprets C:
		// as a forbidden URI authority instead of a Windows volume.
		if isWindowsDrivePath(normalized) {
			normalized = strings.ReplaceAll(normalized, "\\", "/")
			normalized = "/" + normalized
		}
		uri.Path = normalized
	}
	uri.RawQuery = query.Encode()
	return uri.String()
}

// isWindowsDrivePath reports whether path starts with a drive letter, either
// as given or after filepath.Abs resolved a relative path on Windows.
func isWindowsDrivePath(path string) bool {
	return len(path) >= 3 && path[1] == ':' && (path[2] == '/' || path[2] == '\\')
}

func configureJournal(ctx context.Context, conn *sql.Conn, mode StoreMode) error {
	journal := "MEMORY"
	if mode == ModeDisk {
		journal = "WAL"
	}
	deadline := time.Now().Add(journalConfigRetryLimit)
	for {
		_, err := conn.ExecContext(ctx, "PRAGMA journal_mode = "+journal)
		if err == nil {
			break
		}
		if !isSQLiteBusy(err) || time.Now().After(deadline) {
			return fmt.Errorf("configure SQLite journal mode %s: %w", journal, err)
		}
		timer := time.NewTimer(journalConfigRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("configure SQLite journal mode %s: %w", journal, context.Cause(ctx))
		case <-timer.C:
		}
	}
	if mode == ModeDisk {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA journal_size_limit = %d", journalSizeLimit)); err != nil {
			return fmt.Errorf("configure SQLite journal size: %w", err)
		}
	}
	return nil
}

func migrateSchema(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin session schema migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rollbackConn(conn)
		}
	}()

	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read session schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("session database schema version %d is newer than supported version %d", version, schemaVersion)
	}
	if version == 0 {
		var tableCount int
		if err := conn.QueryRowContext(ctx, `
			SELECT count(*) FROM sqlite_master
			WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&tableCount); err != nil {
			return fmt.Errorf("inspect unversioned database: %w", err)
		}
		if tableCount != 0 {
			return fmt.Errorf("refusing unrecognized unversioned session database")
		}
		if err := applySchemaV1(ctx, conn); err != nil {
			return err
		}
		version = 1
	}
	if version == 1 {
		if err := applySchemaV2(ctx, conn); err != nil {
			return err
		}
		version = 2
	}
	if version != schemaVersion {
		return fmt.Errorf("no session schema migration from version %d", version)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit session schema migration: %w", err)
	}
	committed = true
	return nil
}

func applySchemaV1(ctx context.Context, conn *sql.Conn) error {
	statements := []string{
		`CREATE TABLE sessions (
			id BLOB PRIMARY KEY NOT NULL CHECK(length(id) = 16),
			name TEXT NOT NULL UNIQUE,
			retention TEXT NOT NULL CHECK(retention IN ('named','auto')),
			created_ns INTEGER NOT NULL,
			updated_ns INTEGER NOT NULL,
			ttl_ns INTEGER NOT NULL DEFAULT 0 CHECK(ttl_ns >= 0),
			ttl_explicit INTEGER NOT NULL DEFAULT 0 CHECK(ttl_explicit IN (0,1)),
			settings_json BLOB NOT NULL,
			next_sequence INTEGER NOT NULL DEFAULT 0 CHECK(next_sequence >= 0),
			has_turn INTEGER NOT NULL DEFAULT 0 CHECK(has_turn IN (0,1))
		) STRICT`,
		`CREATE INDEX sessions_updated_idx ON sessions(updated_ns DESC, name)`,
		`CREATE INDEX sessions_expiry_idx ON sessions(ttl_ns, updated_ns)`,
		`CREATE TABLE messages (
			session_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			sequence INTEGER NOT NULL CHECK(sequence >= 0),
			payload_json BLOB NOT NULL,
			PRIMARY KEY(session_id, sequence)
		) STRICT, WITHOUT ROWID`,
		`CREATE TABLE artifact_blobs (
			digest BLOB PRIMARY KEY NOT NULL CHECK(length(digest) = 32),
			byte_count INTEGER NOT NULL CHECK(byte_count >= 0),
			chunk_count INTEGER NOT NULL CHECK(chunk_count >= 0),
			created_ns INTEGER NOT NULL
		) STRICT, WITHOUT ROWID`,
		`CREATE TABLE artifact_chunks (
			digest BLOB NOT NULL REFERENCES artifact_blobs(digest) ON DELETE CASCADE,
			chunk_index INTEGER NOT NULL CHECK(chunk_index >= 0),
			data BLOB NOT NULL CHECK(length(data) <= 1048576),
			PRIMARY KEY(digest, chunk_index)
		) STRICT, WITHOUT ROWID`,
		`CREATE TABLE session_artifacts (
			session_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			digest BLOB NOT NULL REFERENCES artifact_blobs(digest) ON DELETE CASCADE,
			PRIMARY KEY(session_id, digest)
		) STRICT, WITHOUT ROWID`,
		`CREATE TABLE session_leases (
			session_id BLOB PRIMARY KEY NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			owner_token BLOB NOT NULL CHECK(length(owner_token) = 16),
			heartbeat_ns INTEGER NOT NULL,
			expires_ns INTEGER NOT NULL
		) STRICT, WITHOUT ROWID`,
		"PRAGMA user_version = 1",
	}
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply session schema v1: %w", err)
		}
	}
	return nil
}

// legacySystemPromptDefaults are the pre-display-contract default system
// prompts that v1 databases stored verbatim, both in settings_json and as the
// seeded system row at sequence 0. Schema v2 strips both, so a resumed
// context reads as persona-less and the request-time display contract is the
// only contract the model sees.
var legacySystemPromptDefaults = []string{
	"Your output will be displayed in a unix terminal. Be terse, 512 characters max. Do not use markdown.",
	"Your output will be displayed in a unix tui. Be terse. Use markdown where it aids readability. Use code blocks where appropriate, including for markdown.",
}

func isLegacySystemPrompt(s string) bool {
	return slices.Contains(legacySystemPromptDefaults, s)
}

// applySchemaV2 rewrites data, not shape, so validateSchemaV1 still describes
// the tables: it strips the legacy default prompts (settings_json and the
// seeded sequence-0 system row, renumbering the rest of that session) and
// moves --add text imports from the "=== name ===" Content form into the
// text-part-with-FileName form the writer produces now.
func applySchemaV2(ctx context.Context, conn *sql.Conn) error {
	if err := stripLegacySystemPrompts(ctx, conn); err != nil {
		return fmt.Errorf("apply session schema v2: %w", err)
	}
	if err := upgradeImportedTextFiles(ctx, conn); err != nil {
		return fmt.Errorf("apply session schema v2: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		return fmt.Errorf("apply session schema v2: %w", err)
	}
	return nil
}

// stripLegacySystemPrompts blanks a legacy default in settings_json and drops
// the seeded system row that repeats it. Untouched rows stay byte-identical,
// updated_ns stays put (a migration is not a use), and the gap left by the
// dropped row is closed because GetHistory requires contiguous sequences
// that account for next_sequence.
func stripLegacySystemPrompts(ctx context.Context, conn *sql.Conn) error {
	type settingsRow struct {
		id       []byte
		settings []byte
	}
	var rewrites []settingsRow
	rows, err := conn.QueryContext(ctx, "SELECT id, settings_json FROM sessions")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			_ = rows.Close()
			return err
		}
		// Decode through Metadata, the blob's own codec, so every other
		// field round-trips unchanged.
		var metadata Metadata
		if err := json.Unmarshal(raw, &metadata); err != nil {
			_ = rows.Close()
			return fmt.Errorf("decode session settings: %w", err)
		}
		if !isLegacySystemPrompt(metadata.SystemPrompt) {
			continue
		}
		metadata.SystemPrompt = ""
		settings, err := json.Marshal(metadata)
		if err != nil {
			_ = rows.Close()
			return err
		}
		rewrites = append(rewrites, settingsRow{id: id, settings: settings})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range rewrites {
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET settings_json = ? WHERE id = ?", row.settings, row.id); err != nil {
			return err
		}
	}

	var stripped [][]byte
	rows, err = conn.QueryContext(ctx, "SELECT session_id, payload_json FROM messages WHERE sequence = 0")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, payload []byte
		if err := rows.Scan(&id, &payload); err != nil {
			_ = rows.Close()
			return err
		}
		var message messages.ChatMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			_ = rows.Close()
			return fmt.Errorf("decode session message: %w", err)
		}
		if message.Role == messages.MessageRoleSystem && len(message.Parts) == 0 && isLegacySystemPrompt(message.Content) {
			stripped = append(stripped, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// Renumbering moves every remaining row through a far-off range first,
	// so no intermediate step collides on the (session_id, sequence) key.
	const shift = int64(1) << 40
	for _, id := range stripped {
		for _, step := range []struct {
			sql  string
			args []any
		}{
			{"DELETE FROM messages WHERE session_id = ? AND sequence = 0", []any{id}},
			{"UPDATE messages SET sequence = sequence + ? WHERE session_id = ?", []any{shift, id}},
			{"UPDATE messages SET sequence = sequence - ? WHERE session_id = ?", []any{shift + 1, id}},
			{"UPDATE sessions SET next_sequence = next_sequence - 1 WHERE id = ?", []any{id}},
		} {
			if _, err := conn.ExecContext(ctx, step.sql, step.args...); err != nil {
				return err
			}
		}
	}
	return nil
}

// upgradeImportedTextFiles rewrites the v1 form of an --add text import, a
// context-import user message whose Content is "=== name ===\n<body>", into
// one text part carrying the same text plus FileName. The model-visible text
// is unchanged; the REPL can now show "[attached: name]" without parsing the
// header. Only rows flagged as context imports qualify, so a typed prompt
// that happens to start with the header is left alone.
func upgradeImportedTextFiles(ctx context.Context, conn *sql.Conn) error {
	type importRow struct {
		id       []byte
		sequence int64
		payload  []byte
	}
	var rewrites []importRow
	rows, err := conn.QueryContext(ctx, `SELECT session_id, sequence, payload_json FROM messages WHERE instr(payload_json, '"content":"=== ') > 0`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, payload []byte
		var sequence int64
		if err := rows.Scan(&id, &sequence, &payload); err != nil {
			_ = rows.Close()
			return err
		}
		var message messages.ChatMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			_ = rows.Close()
			return fmt.Errorf("decode session message: %w", err)
		}
		name, ok := importedTextFileName(message)
		if !ok {
			continue
		}
		message.Parts = []messages.ContentPart{{Type: "text", Text: message.Content, FileName: name}}
		message.Content = ""
		upgraded, err := json.Marshal(message)
		if err != nil {
			_ = rows.Close()
			return err
		}
		rewrites = append(rewrites, importRow{id: id, sequence: sequence, payload: upgraded})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range rewrites {
		if _, err := conn.ExecContext(ctx, "UPDATE messages SET payload_json = ? WHERE session_id = ? AND sequence = ?", row.payload, row.id, row.sequence); err != nil {
			return err
		}
	}
	return nil
}

// importedTextFileName reports the file name of a v1-form --add text import.
func importedTextFileName(msg messages.ChatMessage) (string, bool) {
	if msg.Role != messages.MessageRoleUser || len(msg.Parts) != 0 {
		return "", false
	}
	if imported, _ := msg.Metadata[messages.MetadataKeyContextImport].(bool); !imported {
		return "", false
	}
	if !strings.HasPrefix(msg.Content, "=== ") {
		return "", false
	}
	lineEnd := strings.IndexByte(msg.Content, '\n')
	if lineEnd < 8 {
		return "", false
	}
	header := msg.Content[:lineEnd]
	if !strings.HasSuffix(header, " ===") {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(header, "=== "), " ==="))
	return name, name != ""
}

type schemaColumnSpec struct {
	name       string
	typeName   string
	notNull    int
	primaryKey int
	defaultSQL string
}

type schemaTableSpec struct {
	columns      []schemaColumnSpec
	withoutRowID int
	requiredSQL  []string
}

var schemaV1Tables = map[string]schemaTableSpec{
	"sessions": {
		columns: []schemaColumnSpec{
			{"id", "BLOB", 1, 1, ""}, {"name", "TEXT", 1, 0, ""},
			{"retention", "TEXT", 1, 0, ""}, {"created_ns", "INTEGER", 1, 0, ""},
			{"updated_ns", "INTEGER", 1, 0, ""}, {"ttl_ns", "INTEGER", 1, 0, "0"},
			{"ttl_explicit", "INTEGER", 1, 0, "0"}, {"settings_json", "BLOB", 1, 0, ""},
			{"next_sequence", "INTEGER", 1, 0, "0"}, {"has_turn", "INTEGER", 1, 0, "0"},
		},
		requiredSQL: []string{
			"check(length(id)=16)", "check(retentionin('named','auto'))", "check(ttl_ns>=0)",
			"check(ttl_explicitin(0,1))", "check(next_sequence>=0)", "check(has_turnin(0,1))",
		},
	},
	"messages": {
		columns: []schemaColumnSpec{
			{"session_id", "BLOB", 1, 1, ""}, {"sequence", "INTEGER", 1, 2, ""},
			{"payload_json", "BLOB", 1, 0, ""},
		},
		withoutRowID: 1,
		requiredSQL:  []string{"check(sequence>=0)"},
	},
	"artifact_blobs": {
		columns: []schemaColumnSpec{
			{"digest", "BLOB", 1, 1, ""}, {"byte_count", "INTEGER", 1, 0, ""},
			{"chunk_count", "INTEGER", 1, 0, ""}, {"created_ns", "INTEGER", 1, 0, ""},
		},
		withoutRowID: 1,
		requiredSQL: []string{
			"check(length(digest)=32)", "check(byte_count>=0)", "check(chunk_count>=0)",
		},
	},
	"artifact_chunks": {
		columns: []schemaColumnSpec{
			{"digest", "BLOB", 1, 1, ""}, {"chunk_index", "INTEGER", 1, 2, ""},
			{"data", "BLOB", 1, 0, ""},
		},
		withoutRowID: 1,
		requiredSQL:  []string{"check(chunk_index>=0)", "check(length(data)<=1048576)"},
	},
	"session_artifacts": {
		columns: []schemaColumnSpec{
			{"session_id", "BLOB", 1, 1, ""}, {"digest", "BLOB", 1, 2, ""},
		},
		withoutRowID: 1,
	},
	"session_leases": {
		columns: []schemaColumnSpec{
			{"session_id", "BLOB", 1, 1, ""}, {"owner_token", "BLOB", 1, 0, ""},
			{"heartbeat_ns", "INTEGER", 1, 0, ""}, {"expires_ns", "INTEGER", 1, 0, ""},
		},
		withoutRowID: 1,
		requiredSQL:  []string{"check(length(owner_token)=16)"},
	},
}

func validateSchemaV1(ctx context.Context, conn *sql.Conn) error {
	for table, spec := range schemaV1Tables {
		if err := validateSchemaTable(ctx, conn, table, spec); err != nil {
			return fmt.Errorf("session database schema v1 table %s: %w", table, err)
		}
	}
	if err := requireIndexColumns(ctx, conn, "sessions_updated_idx", []string{"updated_ns", "name"}); err != nil {
		return err
	}
	if err := requireIndexColumns(ctx, conn, "sessions_expiry_idx", []string{"ttl_ns", "updated_ns"}); err != nil {
		return err
	}
	if err := requireUniqueColumn(ctx, conn, "sessions", "name"); err != nil {
		return err
	}
	expectedForeignKeys := map[string]map[string]bool{
		"messages":          {"session_id>sessions.id:CASCADE": true},
		"artifact_chunks":   {"digest>artifact_blobs.digest:CASCADE": true},
		"session_artifacts": {"session_id>sessions.id:CASCADE": true, "digest>artifact_blobs.digest:CASCADE": true},
		"session_leases":    {"session_id>sessions.id:CASCADE": true},
	}
	for table, expected := range expectedForeignKeys {
		if err := validateForeignKeys(ctx, conn, table, expected); err != nil {
			return err
		}
	}
	return nil
}

func validateSchemaTable(ctx context.Context, conn *sql.Conn, table string, spec schemaTableSpec) error {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%q)", table))
	if err != nil {
		return err
	}
	var actual []schemaColumnSpec
	for rows.Next() {
		var cid int
		var column schemaColumnSpec
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &column.name, &column.typeName, &column.notNull, &defaultValue, &column.primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		if cid != len(actual) {
			_ = rows.Close()
			return fmt.Errorf("non-contiguous column IDs")
		}
		if defaultValue.Valid {
			column.defaultSQL = defaultValue.String
		}
		actual = append(actual, column)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, spec.columns) {
		return fmt.Errorf("column shape differs: got %+v", actual)
	}
	var columnCount, withoutRowID, strict int
	if err := conn.QueryRowContext(ctx, `
		SELECT ncol,wr,strict FROM pragma_table_list
		WHERE schema='main' AND type='table' AND name=?`, table).Scan(&columnCount, &withoutRowID, &strict); err != nil {
		return err
	}
	if columnCount != len(spec.columns) || withoutRowID != spec.withoutRowID || strict != 1 {
		return fmt.Errorf("table flags differ (columns=%d without-rowid=%d strict=%d)", columnCount, withoutRowID, strict)
	}
	var createSQL string
	if err := conn.QueryRowContext(ctx,
		"SELECT sql FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&createSQL); err != nil {
		return err
	}
	compact := compactSchemaSQL(createSQL)
	for _, required := range spec.requiredSQL {
		if !strings.Contains(compact, required) {
			return fmt.Errorf("missing constraint %s", required)
		}
	}
	return nil
}

func compactSchemaSQL(value string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, value)
}

func requireIndexColumns(ctx context.Context, conn *sql.Conn, index string, expected []string) error {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("PRAGMA index_info(%q)", index))
	if err != nil {
		return err
	}
	var columns []string
	for rows.Next() {
		var sequence, cid int
		var name string
		if err := rows.Scan(&sequence, &cid, &name); err != nil {
			_ = rows.Close()
			return err
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !reflect.DeepEqual(columns, expected) {
		return fmt.Errorf("session database index %s has columns %v, want %v", index, columns, expected)
	}
	return nil
}

func requireUniqueColumn(ctx context.Context, conn *sql.Conn, table, column string) error {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("PRAGMA index_list(%q)", table))
	if err != nil {
		return err
	}
	var uniqueIndexes []string
	for rows.Next() {
		var sequence, unique, partial int
		var name, origin string
		if err := rows.Scan(&sequence, &name, &unique, &origin, &partial); err != nil {
			_ = rows.Close()
			return err
		}
		if unique == 1 && partial == 0 {
			uniqueIndexes = append(uniqueIndexes, name)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, index := range uniqueIndexes {
		indexRows, err := conn.QueryContext(ctx, fmt.Sprintf("PRAGMA index_info(%q)", index))
		if err != nil {
			return err
		}
		var columns []string
		for indexRows.Next() {
			var sequence, cid int
			var name string
			if err := indexRows.Scan(&sequence, &cid, &name); err != nil {
				_ = indexRows.Close()
				return err
			}
			columns = append(columns, name)
		}
		if err := indexRows.Err(); err != nil {
			_ = indexRows.Close()
			return err
		}
		if err := indexRows.Close(); err != nil {
			return err
		}
		if reflect.DeepEqual(columns, []string{column}) {
			return nil
		}
	}
	return fmt.Errorf("session database table %s lacks UNIQUE(%s)", table, column)
}

func validateForeignKeys(ctx context.Context, conn *sql.Conn, table string, expected map[string]bool) error {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("PRAGMA foreign_key_list(%q)", table))
	if err != nil {
		return err
	}
	actual := make(map[string]bool)
	for rows.Next() {
		var id, sequence int
		var parent, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &sequence, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			_ = rows.Close()
			return err
		}
		actual[from+">"+parent+"."+to+":"+strings.ToUpper(onDelete)] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("session database table %s foreign keys differ: got %v", table, actual)
	}
	return nil
}

func (s *SQLiteStore) cleanupLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cleanup)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			_ = s.Expire(s.ctx)
		}
	}
}

func (s *SQLiteStore) ensureOpen() error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	return nil
}

func (s *SQLiteStore) withWrite(ctx context.Context, fn func(*sql.Conn) error) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			rollbackConn(conn)
		}
	}()
	if err = fn(conn); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *SQLiteStore) withRead(ctx context.Context, fn func(*sql.Conn) error) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			rollbackConn(conn)
		}
	}()
	if err = fn(conn); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func rollbackConn(conn *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = conn.ExecContext(ctx, "ROLLBACK")
}

func (s *SQLiteStore) track(session *sqliteSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return false
	}
	s.open[session] = struct{}{}
	return true
}

func (s *SQLiteStore) untrack(session *sqliteSession) {
	s.mu.Lock()
	delete(s.open, session)
	s.mu.Unlock()
}

// Acquire opens or creates name and obtains its exclusive lease.
func (s *SQLiteStore) Acquire(ctx context.Context, name string, options AcquireOptions) (Session, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	if err := validateSessionName(name); err != nil {
		return nil, fmt.Errorf("invalid session name %q: %w", name, err)
	}
	owner, err := randomBytes(16)
	if err != nil {
		return nil, fmt.Errorf("generate session lease owner: %w", err)
	}

	acquireCtx, acquireCancel := context.WithCancel(ctx)
	stopStoreCancel := context.AfterFunc(s.ctx, acquireCancel)
	defer func() {
		stopStoreCancel()
		acquireCancel()
	}()
	waitCtx, cancel := context.WithTimeout(acquireCtx, leaseAcquireTimeout)
	defer cancel()
	for {
		id, expiresNS, busy, err := s.tryAcquire(waitCtx, name, options, owner)
		if err == nil && !busy {
			sessionCtx, sessionCancel := context.WithCancelCause(s.ctx)
			session := &sqliteSession{
				store:         s,
				id:            id,
				ownerToken:    owner,
				ctx:           sessionCtx,
				cancel:        sessionCancel,
				heartbeatDone: make(chan struct{}),
				closeDone:     make(chan struct{}),
			}
			session.expiresNS.Store(expiresNS)
			session.artifacts = &sqliteArtifactStore{session: session}
			if !s.track(session) {
				sessionCancel(ErrStoreClosed)
				_ = s.releaseLease(context.Background(), id, owner)
				return nil, ErrStoreClosed
			}
			go session.heartbeatLoop()
			return session, nil
		}
		if err != nil && !isSQLiteBusy(err) {
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			if cause := context.Cause(s.ctx); cause != nil {
				return nil, cause
			}
			if waitCtx.Err() != nil {
				return nil, fmt.Errorf("%w: %s", ErrSessionInUse, name)
			}
			return nil, fmt.Errorf("acquire session %q: %w", name, err)
		}

		timer := time.NewTimer(leaseRetryInterval)
		select {
		case <-waitCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			if cause := context.Cause(s.ctx); cause != nil {
				return nil, cause
			}
			return nil, fmt.Errorf("%w: %s", ErrSessionInUse, name)
		case <-timer.C:
		}
	}
}

func (s *SQLiteStore) tryAcquire(ctx context.Context, name string, options AcquireOptions, owner []byte) (id []byte, expiresNS int64, busy bool, err error) {
	now := time.Now().UTC()
	nowNS := now.UnixNano()
	expiresNS = now.Add(leaseStaleAfter).UnixNano()
	err = s.withWrite(ctx, func(conn *sql.Conn) error {
		var storedID []byte
		var updatedNS, ttlNS int64
		err := conn.QueryRowContext(ctx,
			"SELECT id, updated_ns, ttl_ns FROM sessions WHERE name = ?", name).Scan(&storedID, &updatedNS, &ttlNS)
		if err == nil && ttlNS > 0 && updatedNS <= nowNS && ttlNS <= nowNS-updatedNS {
			// The session idled past its TTL: retire it now instead of
			// serving stale history until the next sweep observes it. A live
			// lease means an active holder, so the guarded delete leaves the
			// row alone and the lease upsert below reports busy.
			result, deleteErr := conn.ExecContext(ctx, `
				DELETE FROM sessions
				WHERE id = ?
				  AND NOT EXISTS (
					SELECT 1 FROM session_leases
					WHERE session_leases.session_id = sessions.id
					  AND session_leases.expires_ns > ?
				  )`, storedID, nowNS)
			if deleteErr != nil {
				return deleteErr
			}
			retired, deleteErr := result.RowsAffected()
			if deleteErr != nil {
				return deleteErr
			}
			if retired > 0 {
				if gcErr := garbageCollectArtifacts(ctx, conn); gcErr != nil {
					return gcErr
				}
				err = sql.ErrNoRows
			}
		}
		if errors.Is(err, sql.ErrNoRows) {
			storedID, err = randomBytes(16)
			if err != nil {
				return err
			}
			metadata := cloneMetadata(s.defaults)
			if metadata == nil {
				metadata = &Metadata{}
			}
			metadata.Name = name
			metadata.Created = now
			metadata.LastUsed = now
			retention := retentionNamed
			if options.Auto {
				retention = retentionAuto
			}
			ttlExplicit := 0
			if metadata.TTL > 0 {
				ttlExplicit = 1
			} else if options.Auto {
				metadata.TTL = s.autoTTL
			}
			settings, err := json.Marshal(metadata)
			if err != nil {
				return fmt.Errorf("encode default session metadata: %w", err)
			}
			_, err = conn.ExecContext(ctx, `
				INSERT INTO sessions
				(id,name,retention,created_ns,updated_ns,ttl_ns,ttl_explicit,settings_json,next_sequence)
				VALUES(?,?,?,?,?,?,?,?,0)`, storedID, name, retention, nowNS, nowNS, int64(metadata.TTL), ttlExplicit, settings)
			if err != nil {
				return err
			}
			if metadata.SystemPrompt != "" {
				payload, err := json.Marshal(messages.ChatMessage{Role: messages.MessageRoleSystem, Content: metadata.SystemPrompt})
				if err != nil {
					return err
				}
				if _, err := conn.ExecContext(ctx,
					"INSERT INTO messages(session_id,sequence,payload_json) VALUES(?,?,?)",
					storedID, 0, payload); err != nil {
					return err
				}
				if _, err := conn.ExecContext(ctx, "UPDATE sessions SET next_sequence = 1 WHERE id = ?", storedID); err != nil {
					return err
				}
			}
		} else if err != nil {
			return err
		}

		result, err := conn.ExecContext(ctx, `
			INSERT INTO session_leases(session_id,owner_token,heartbeat_ns,expires_ns)
			VALUES(?,?,?,?)
			ON CONFLICT(session_id) DO UPDATE SET
				owner_token=excluded.owner_token,
				heartbeat_ns=excluded.heartbeat_ns,
				expires_ns=excluded.expires_ns
			WHERE session_leases.expires_ns <= ?`, storedID, owner, nowNS, expiresNS, nowNS)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			busy = true
			return nil
		}
		id = append([]byte(nil), storedID...)
		return nil
	})
	return id, expiresNS, busy, err
}

func (s *SQLiteStore) releaseLease(ctx context.Context, id, owner []byte) error {
	releaseCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(releaseCtx,
		"DELETE FROM session_leases WHERE session_id = ? AND owner_token = ?", id, owner)
	return err
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database is busy")
}

func (s *SQLiteStore) Delete(ctx context.Context, name string) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	if err := validateSessionName(name); err != nil {
		return fmt.Errorf("invalid session name %q: %w", name, err)
	}
	nowNS := time.Now().UTC().UnixNano()
	deleted := false
	err := s.withWrite(ctx, func(conn *sql.Conn) error {
		var id []byte
		if err := conn.QueryRowContext(ctx, "SELECT id FROM sessions WHERE name = ?", name).Scan(&id); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		var active int
		if err := conn.QueryRowContext(ctx,
			"SELECT count(*) FROM session_leases WHERE session_id = ? AND expires_ns > ?", id, nowNS).Scan(&active); err != nil {
			return err
		}
		if active != 0 {
			return fmt.Errorf("%w: %s", ErrSessionInUse, name)
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id); err != nil {
			return err
		}
		deleted = true
		return garbageCollectArtifacts(ctx, conn)
	})
	if err != nil {
		return fmt.Errorf("delete session %q: %w", name, err)
	}
	if deleted {
		s.incrementalVacuum(ctx)
	}
	return nil
}

func (s *SQLiteStore) List(ctx context.Context) ([]string, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM sessions ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func (s *SQLiteStore) Exists(ctx context.Context, name string) (bool, error) {
	if err := s.ensureOpen(); err != nil {
		return false, err
	}
	if err := validateSessionName(name); err != nil {
		return false, nil
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sessions WHERE name = ?)", name).Scan(&exists)
	return exists, err
}

func (s *SQLiteStore) GetAllMetadata(ctx context.Context) (map[string]*Metadata, error) {
	summaries, err := s.ListSummaries(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string]*Metadata, len(summaries))
	for _, summary := range summaries {
		result[summary.Metadata.Name] = summary.Metadata
	}
	return result, nil
}

func (s *SQLiteStore) ListSummaries(ctx context.Context) ([]SessionSummary, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT name,created_ns,updated_ns,ttl_ns,settings_json,next_sequence
		FROM sessions ORDER BY updated_ns DESC,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SessionSummary
	for rows.Next() {
		var snap sessionSnapshot
		if err := rows.Scan(&snap.name, &snap.createdNS, &snap.updatedNS, &snap.ttlNS, &snap.settings, &snap.nextSeq); err != nil {
			return nil, err
		}
		metadata, err := metadataFromSnapshot(snap)
		if err != nil {
			return nil, err
		}
		result = append(result, SessionSummary{Metadata: metadata, MessageCount: int(snap.nextSeq)})
	}
	return result, rows.Err()
}

func (s *SQLiteStore) GetLast(ctx context.Context) (string, error) {
	if err := s.ensureOpen(); err != nil {
		return "", err
	}
	var name string
	err := s.db.QueryRowContext(ctx,
		"SELECT name FROM sessions ORDER BY updated_ns DESC, name LIMIT 1").Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

func (s *SQLiteStore) Expire(ctx context.Context) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	nowNS := time.Now().UTC().UnixNano()
	deleted := false
	err := s.withWrite(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `
			DELETE FROM sessions
			WHERE ttl_ns > 0
			  AND updated_ns <= ?
			  AND ttl_ns <= ? - updated_ns
			  AND NOT EXISTS (
				SELECT 1 FROM session_leases
				WHERE session_leases.session_id = sessions.id
				  AND session_leases.expires_ns > ?
			  )`, nowNS, nowNS, nowNS)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		deleted = count > 0
		if deleted {
			return garbageCollectArtifacts(ctx, conn)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if deleted {
		s.incrementalVacuum(ctx)
	}
	return nil
}

func (s *SQLiteStore) incrementalVacuum(ctx context.Context) {
	if s.mode != ModeDisk || ctx.Err() != nil {
		return
	}
	_, _ = s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", boundedVacuumPages))
}

func (s *SQLiteStore) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.cancel(ErrStoreClosed)
	s.wg.Wait()

	s.mu.Lock()
	open := make([]*sqliteSession, 0, len(s.open))
	for session := range s.open {
		open = append(open, session)
	}
	s.mu.Unlock()

	var closeErr error
	for _, session := range open {
		if err := session.close(context.Canceled); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	if err := s.db.Close(); err != nil {
		closeErr = errors.Join(closeErr, err)
	}
	return closeErr
}

func validateSessionName(name string) error {
	if name == "" {
		return fmt.Errorf("name cannot be empty")
	}
	if strings.ContainsAny(name, "/\\:*?\"<>|") {
		return fmt.Errorf("name contains invalid characters")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("name cannot be %q", name)
	}
	if strings.HasPrefix(name, " ") || strings.HasSuffix(name, " ") ||
		strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return fmt.Errorf("name cannot start or end with spaces or dots")
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return fmt.Errorf("name contains control characters")
		}
	}
	return nil
}

// validateContextName is retained as the package's focused validation helper;
// context was the previous user-facing term for a session.
func validateContextName(name string) error {
	return validateSessionName(name)
}

func randomBytes(size int) ([]byte, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return nil, err
	}
	return value, nil
}

func cloneMetadata(metadata *Metadata) *Metadata {
	if metadata == nil {
		return nil
	}
	out := *metadata
	if metadata.ActiveTools != nil {
		out.ActiveTools = append([]tools.ToolLoaderInfo(nil), metadata.ActiveTools...)
	}
	if metadata.ActiveSkills != nil {
		out.ActiveSkills = append([]string(nil), metadata.ActiveSkills...)
	}
	if metadata.SkillDirs != nil {
		out.SkillDirs = append([]string(nil), metadata.SkillDirs...)
	}
	if metadata.SkillSources != nil {
		out.SkillSources = append([]string(nil), metadata.SkillSources...)
	}
	return &out
}

func metadataFromSnapshot(snap sessionSnapshot) (*Metadata, error) {
	var metadata Metadata
	if err := json.Unmarshal(snap.settings, &metadata); err != nil {
		return nil, fmt.Errorf("decode metadata for session %q: %w", snap.name, err)
	}
	metadata.Name = snap.name
	metadata.Created = time.Unix(0, snap.createdNS).UTC()
	metadata.LastUsed = time.Unix(0, snap.updatedNS).UTC()
	metadata.TTL = time.Duration(snap.ttlNS)
	return cloneMetadata(&metadata), nil
}

func garbageCollectArtifacts(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, `
		DELETE FROM artifact_blobs
		WHERE NOT EXISTS (
			SELECT 1 FROM session_artifacts
			WHERE session_artifacts.digest = artifact_blobs.digest
		)`)
	return err
}

func (s *sqliteSession) Context() context.Context {
	return s.ctx
}

func (s *sqliteSession) heartbeatLoop() {
	defer close(s.heartbeatDone)
	ticker := time.NewTicker(leaseHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(s.ctx, leaseHeartbeatInterval)
			owned, expiresNS, err := s.renewLease(ctx)
			cancel()
			if err == nil && owned {
				s.expiresNS.Store(expiresNS)
				continue
			}
			if err == nil && !owned {
				s.cancel(ErrSessionLeaseLost)
				return
			}
			if s.ctx.Err() != nil {
				return
			}
			// Waiting for the store's sole pooled connection can outlive the
			// stale threshold during a legitimate long transaction. Only an
			// owner-token mismatch proves that this process lost the lease.
		}
	}
}

func (s *sqliteSession) renewLease(ctx context.Context) (bool, int64, error) {
	now := time.Now().UTC()
	nowNS := now.UnixNano()
	expiresNS := now.Add(leaseStaleAfter).UnixNano()
	var owned bool
	err := s.store.withWrite(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `
			UPDATE session_leases
			SET heartbeat_ns = ?, expires_ns = ?
			WHERE session_id = ? AND owner_token = ?`,
			nowNS, expiresNS, s.id, s.ownerToken)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		owned = changed == 1
		return nil
	})
	return owned, expiresNS, err
}

func (s *sqliteSession) operationContext(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cause := context.Cause(s.ctx); cause != nil {
		return nil, nil, cause
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	cleanup := func() {
		stop()
		cancel()
	}
	return opCtx, cleanup, nil
}

func (s *sqliteSession) mapError(caller context.Context, err error) error {
	if err == nil {
		return nil
	}
	if cause := context.Cause(s.ctx); cause != nil {
		return cause
	}
	if caller != nil && caller.Err() != nil {
		return context.Cause(caller)
	}
	return err
}

func (s *sqliteSession) loseLease() error {
	s.cancel(ErrSessionLeaseLost)
	return ErrSessionLeaseLost
}

func (s *sqliteSession) requireLease(ctx context.Context, conn interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) error {
	var expiresNS int64
	err := conn.QueryRowContext(ctx, `
		SELECT expires_ns FROM session_leases
		WHERE session_id = ? AND owner_token = ?`, s.id, s.ownerToken).Scan(&expiresNS)
	if errors.Is(err, sql.ErrNoRows) {
		return s.loseLease()
	}
	return err
}

func (s *sqliteSession) snapshot(ctx context.Context) (sessionSnapshot, error) {
	if err := s.requireLease(ctx, s.store.db); err != nil {
		return sessionSnapshot{}, err
	}
	var snap sessionSnapshot
	err := s.store.db.QueryRowContext(ctx, `
		SELECT name,created_ns,updated_ns,ttl_ns,retention,settings_json,next_sequence
		FROM sessions WHERE id = ?`, s.id).Scan(
		&snap.name, &snap.createdNS, &snap.updatedNS, &snap.ttlNS,
		&snap.retention, &snap.settings, &snap.nextSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionSnapshot{}, s.loseLease()
	}
	return snap, err
}

func (s *sqliteSession) GetHistory(ctx context.Context) ([]messages.ChatMessage, error) {
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var history []messages.ChatMessage
	err = s.store.withRead(opCtx, func(conn *sql.Conn) error {
		if err := s.requireLease(opCtx, conn); err != nil {
			return err
		}
		var nextSequence int64
		if err := conn.QueryRowContext(opCtx,
			"SELECT next_sequence FROM sessions WHERE id = ?", s.id).Scan(&nextSequence); err != nil {
			return err
		}
		rows, err := conn.QueryContext(opCtx, `
			SELECT sequence,payload_json FROM messages
			WHERE session_id = ? ORDER BY sequence`, s.id)
		if err != nil {
			return err
		}
		var expected int64
		for rows.Next() {
			var sequence int64
			var payload []byte
			if err := rows.Scan(&sequence, &payload); err != nil {
				_ = rows.Close()
				return err
			}
			if sequence != expected {
				_ = rows.Close()
				return fmt.Errorf("session message sequence is corrupt: got %d, want %d", sequence, expected)
			}
			var message messages.ChatMessage
			if err := json.Unmarshal(payload, &message); err != nil {
				_ = rows.Close()
				return fmt.Errorf("decode session message %d: %w", sequence, err)
			}
			history = append(history, message)
			expected++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if expected != nextSequence {
			return fmt.Errorf("session message sequence is corrupt: read %d messages, expected %d", expected, nextSequence)
		}
		return nil
	})
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	return CopyHistory(history), nil
}

func (s *sqliteSession) AddMessage(ctx context.Context, message messages.ChatMessage) error {
	return s.AddMessages(ctx, []messages.ChatMessage{message})
}

func (s *sqliteSession) AddMessages(ctx context.Context, messagesToAdd []messages.ChatMessage) error {
	if len(messagesToAdd) == 0 {
		if cause := context.Cause(s.ctx); cause != nil {
			return cause
		}
		if ctx != nil {
			return context.Cause(ctx)
		}
		return nil
	}
	if ctx != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
	}
	payloads := make([][]byte, len(messagesToAdd))
	for i := range messagesToAdd {
		payload, err := json.Marshal(messagesToAdd[i])
		if err != nil {
			return fmt.Errorf("encode message %d: %w", i, err)
		}
		payloads[i] = payload
	}

	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	nowNS := time.Now().UTC().UnixNano()
	err = s.store.withWrite(opCtx, func(conn *sql.Conn) error {
		if err := s.requireLease(opCtx, conn); err != nil {
			return err
		}
		var next int64
		if err := conn.QueryRowContext(opCtx,
			"SELECT next_sequence FROM sessions WHERE id = ?", s.id).Scan(&next); err != nil {
			return err
		}
		for i, payload := range payloads {
			if _, err := conn.ExecContext(opCtx,
				"INSERT INTO messages(session_id,sequence,payload_json) VALUES(?,?,?)",
				s.id, next+int64(i), payload); err != nil {
				return err
			}
		}
		_, err := conn.ExecContext(opCtx, `
			UPDATE sessions SET next_sequence = ?, updated_ns = ?, has_turn = 1 WHERE id = ?`,
			next+int64(len(payloads)), nowNS, s.id)
		return err
	})
	return s.mapError(ctx, err)
}

func (s *sqliteSession) Clear(ctx context.Context) error {
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	nowNS := time.Now().UTC().UnixNano()
	err = s.store.withWrite(opCtx, func(conn *sql.Conn) error {
		if err := s.requireLease(opCtx, conn); err != nil {
			return err
		}
		var settings []byte
		if err := conn.QueryRowContext(opCtx,
			"SELECT settings_json FROM sessions WHERE id = ?", s.id).Scan(&settings); err != nil {
			return err
		}
		var metadata Metadata
		if err := json.Unmarshal(settings, &metadata); err != nil {
			return fmt.Errorf("decode session metadata: %w", err)
		}
		next, err := replaceSessionContents(opCtx, conn, s.id, metadata.SystemPrompt)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(opCtx,
			"UPDATE sessions SET next_sequence = ?, updated_ns = ? WHERE id = ?", next, nowNS, s.id); err != nil {
			return err
		}
		return nil
	})
	if err == nil {
		s.store.incrementalVacuum(opCtx)
	}
	return s.mapError(ctx, err)
}

// Reset atomically replaces the session settings and conversation contents.
// Canonical identity and timestamps remain owned by the store; the supplied
// metadata controls all other settings, including the rebuilt system message.
func (s *sqliteSession) Reset(ctx context.Context, info *Metadata) error {
	if info == nil {
		return fmt.Errorf("metadata cannot be nil")
	}
	metadata := cloneMetadata(info)
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	now := time.Now().UTC()
	err = s.store.withWrite(opCtx, func(conn *sql.Conn) error {
		if err := s.requireLease(opCtx, conn); err != nil {
			return err
		}
		var snap sessionSnapshot
		var ttlExplicit int
		if err := conn.QueryRowContext(opCtx, `
			SELECT name,created_ns,updated_ns,ttl_ns,retention,settings_json,next_sequence,ttl_explicit
			FROM sessions WHERE id = ?`, s.id).Scan(
			&snap.name, &snap.createdNS, &snap.updatedNS, &snap.ttlNS, &snap.retention,
			&snap.settings, &snap.nextSeq, &ttlExplicit); err != nil {
			return err
		}
		metadata.Name = snap.name
		metadata.Created = time.Unix(0, snap.createdNS).UTC()
		metadata.LastUsed = now
		if metadata.TTL < 0 {
			return fmt.Errorf("session TTL cannot be negative")
		}
		newTTLExplicit := ttlExplicit
		if ttlExplicit == 0 && int64(metadata.TTL) != snap.ttlNS {
			newTTLExplicit = 1
		}
		settings, err := json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("encode session metadata: %w", err)
		}
		next, err := replaceSessionContents(opCtx, conn, s.id, metadata.SystemPrompt)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(opCtx, `
			UPDATE sessions
			SET updated_ns = ?, ttl_ns = ?, ttl_explicit = ?, settings_json = ?, next_sequence = ?
			WHERE id = ?`, now.UnixNano(), int64(metadata.TTL), newTTLExplicit, settings, next, s.id)
		return err
	})
	if err == nil {
		s.store.incrementalVacuum(opCtx)
	}
	return s.mapError(ctx, err)
}

func replaceSessionContents(ctx context.Context, conn *sql.Conn, sessionID []byte, systemPrompt string) (int64, error) {
	if _, err := conn.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ?", sessionID); err != nil {
		return 0, err
	}
	next := int64(0)
	if systemPrompt != "" {
		payload, err := json.Marshal(messages.ChatMessage{Role: messages.MessageRoleSystem, Content: systemPrompt})
		if err != nil {
			return 0, err
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO messages(session_id,sequence,payload_json) VALUES(?,?,?)",
			sessionID, 0, payload); err != nil {
			return 0, err
		}
		next = 1
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM session_artifacts WHERE session_id = ?", sessionID); err != nil {
		return 0, err
	}
	if err := garbageCollectArtifacts(ctx, conn); err != nil {
		return 0, err
	}
	return next, nil
}

func (s *sqliteSession) GetName(ctx context.Context) (string, error) {
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()
	snap, err := s.snapshot(opCtx)
	return snap.name, s.mapError(ctx, err)
}

func (s *sqliteSession) Rename(ctx context.Context, newName string) error {
	if err := validateSessionName(newName); err != nil {
		return fmt.Errorf("invalid session name %q: %w", newName, err)
	}
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	now := time.Now().UTC()
	err = s.store.withWrite(opCtx, func(conn *sql.Conn) error {
		if err := s.requireLease(opCtx, conn); err != nil {
			return err
		}
		var snap sessionSnapshot
		var ttlExplicit int
		if err := conn.QueryRowContext(opCtx, `
			SELECT name,created_ns,updated_ns,ttl_ns,retention,settings_json,next_sequence,ttl_explicit
			FROM sessions WHERE id = ?`, s.id).Scan(
			&snap.name, &snap.createdNS, &snap.updatedNS, &snap.ttlNS, &snap.retention,
			&snap.settings, &snap.nextSeq, &ttlExplicit); err != nil {
			return err
		}
		if snap.name != newName {
			var exists bool
			if err := conn.QueryRowContext(opCtx,
				"SELECT EXISTS(SELECT 1 FROM sessions WHERE name = ?)", newName).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return fmt.Errorf("session %q already exists", newName)
			}
		}
		metadata, err := metadataFromSnapshot(snap)
		if err != nil {
			return err
		}
		metadata.Name = newName
		metadata.LastUsed = now
		if ttlExplicit == 0 {
			metadata.TTL = 0
			snap.ttlNS = 0
		}
		settings, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(opCtx, `
			UPDATE sessions
			SET name = ?, retention = 'named', updated_ns = ?, ttl_ns = ?, settings_json = ?
			WHERE id = ?`, newName, now.UnixNano(), snap.ttlNS, settings, s.id)
		return err
	})
	return s.mapError(ctx, err)
}

func (s *sqliteSession) GetMetadata(ctx context.Context) (*Metadata, error) {
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	snap, err := s.snapshot(opCtx)
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	metadata, err := metadataFromSnapshot(snap)
	return metadata, s.mapError(ctx, err)
}

func (s *sqliteSession) SetMetadata(ctx context.Context, info *Metadata) error {
	if info == nil {
		return fmt.Errorf("metadata cannot be nil")
	}
	metadata := cloneMetadata(info)
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	now := time.Now().UTC()
	err = s.store.withWrite(opCtx, func(conn *sql.Conn) error {
		if err := s.requireLease(opCtx, conn); err != nil {
			return err
		}
		var snap sessionSnapshot
		var ttlExplicit int
		if err := conn.QueryRowContext(opCtx, `
			SELECT name,created_ns,updated_ns,ttl_ns,retention,settings_json,next_sequence,ttl_explicit
			FROM sessions WHERE id = ?`, s.id).Scan(
			&snap.name, &snap.createdNS, &snap.updatedNS, &snap.ttlNS, &snap.retention,
			&snap.settings, &snap.nextSeq, &ttlExplicit); err != nil {
			return err
		}
		current, err := metadataFromSnapshot(snap)
		if err != nil {
			return err
		}
		metadata.Name = snap.name
		metadata.Created = time.Unix(0, snap.createdNS).UTC()
		// LastUsed is canonical storage state, not a caller-controlled setting.
		metadata.LastUsed = current.LastUsed
		if metadata.TTL < 0 {
			return fmt.Errorf("session TTL cannot be negative")
		}
		if reflect.DeepEqual(metadata, current) {
			return nil
		}
		metadata.LastUsed = now
		newTTLExplicit := ttlExplicit
		if ttlExplicit == 0 && int64(metadata.TTL) != snap.ttlNS {
			newTTLExplicit = 1
		}
		settings, err := json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("encode session metadata: %w", err)
		}
		_, err = conn.ExecContext(opCtx, `
			UPDATE sessions
			SET updated_ns = ?, ttl_ns = ?, ttl_explicit = ?, settings_json = ?
			WHERE id = ?`, now.UnixNano(), int64(metadata.TTL), newTTLExplicit, settings, s.id)
		return err
	})
	return s.mapError(ctx, err)
}

func (s *sqliteSession) GetLastUsed(ctx context.Context) (time.Time, error) {
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer cleanup()
	snap, err := s.snapshot(opCtx)
	if err != nil {
		return time.Time{}, s.mapError(ctx, err)
	}
	return time.Unix(0, snap.updatedNS).UTC(), nil
}

func (s *sqliteSession) CacheSessionID(ctx context.Context) (string, error) {
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if err := s.requireLease(opCtx, s.store.db); err != nil {
		return "", s.mapError(ctx, err)
	}
	digest := sha256.Sum256(append([]byte("polly-cache-session-v1\x00"), s.id...))
	return hex.EncodeToString(digest[:]), nil
}

func (s *sqliteSession) ArtifactStore() artifacts.Store {
	return s.artifacts
}

func (s *sqliteSession) GetTotalTokens(ctx context.Context) (int, error) {
	history, err := s.GetHistory(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, message := range history {
		total += GetMessageTokens(message)
	}
	return total, nil
}

func (s *sqliteSession) GetCapacityPercentage(ctx context.Context) (float64, error) {
	metadata, err := s.GetMetadata(ctx)
	if err != nil {
		return 0, err
	}
	if metadata.MaxHistoryTokens == 0 {
		return 0, nil
	}
	total, err := s.GetTotalTokens(ctx)
	if err != nil {
		return 0, err
	}
	return float64(total) / float64(metadata.MaxHistoryTokens) * 100, nil
}

func (s *sqliteSession) GetTimeToExpiry(ctx context.Context) (time.Duration, error) {
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	snap, err := s.snapshot(opCtx)
	if err != nil {
		return 0, s.mapError(ctx, err)
	}
	if snap.ttlNS == 0 {
		return 0, nil
	}
	remaining := time.Duration(snap.ttlNS) - time.Since(time.Unix(0, snap.updatedNS))
	if remaining < 0 {
		return 0, nil
	}
	return remaining, nil
}

func (s *sqliteSession) GetMessageCounts(ctx context.Context) (map[string]int, error) {
	history, err := s.GetHistory(ctx)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int)
	for _, message := range history {
		counts[string(message.Role)]++
	}
	return counts, nil
}

func (s *sqliteSession) GetToolCallCount(ctx context.Context) (int, error) {
	history, err := s.GetHistory(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, message := range history {
		total += len(message.ToolCalls)
	}
	return total, nil
}

func (s *sqliteSession) Close() error {
	return s.close(context.Canceled)
}

func (s *sqliteSession) close(cause error) error {
	if !s.closed.CompareAndSwap(false, true) {
		<-s.closeDone
		s.closeMu.Lock()
		defer s.closeMu.Unlock()
		return s.closeErr
	}
	s.cancel(cause)
	<-s.heartbeatDone
	defer s.store.untrack(s)
	var result error
	defer func() {
		s.closeMu.Lock()
		s.closeErr = result
		s.closeMu.Unlock()
		close(s.closeDone)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deleted := false
	err := s.store.withWrite(ctx, func(conn *sql.Conn) error {
		var retention string
		var hasTurn int
		var owned int
		err := conn.QueryRowContext(ctx, `
			SELECT sessions.retention,sessions.has_turn,
			       COALESCE(session_leases.owner_token = ?, 0)
			FROM sessions
			LEFT JOIN session_leases ON session_leases.session_id = sessions.id
			WHERE sessions.id = ?`, s.ownerToken, s.id).Scan(&retention, &hasTurn, &owned)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if owned == 0 {
			return nil
		}

		if retention == retentionAuto && hasTurn == 0 {
			if _, err := conn.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", s.id); err != nil {
				return err
			}
			deleted = true
			return garbageCollectArtifacts(ctx, conn)
		}
		_, err = conn.ExecContext(ctx,
			"DELETE FROM session_leases WHERE session_id = ? AND owner_token = ?", s.id, s.ownerToken)
		return err
	})
	if deleted {
		s.store.incrementalVacuum(ctx)
	}
	if err != nil && s.store.closed.Load() && strings.Contains(strings.ToLower(err.Error()), "closed") {
		err = nil
	}
	result = err
	return result
}

type sqliteArtifactStore struct {
	session *sqliteSession
}

func (s *sqliteArtifactStore) Put(ctx context.Context, blob artifacts.Blob) (artifacts.Ref, error) {
	if ctx != nil {
		if cause := context.Cause(ctx); cause != nil {
			return artifacts.Ref{}, cause
		}
	}
	ref := artifacts.RefForBlob(blob)
	digest, err := artifactDigest(ref.ID)
	if err != nil {
		return artifacts.Ref{}, err
	}
	chunkCount := (len(blob.Data) + artifactChunkSize - 1) / artifactChunkSize

	opCtx, cleanup, err := s.session.operationContext(ctx)
	if err != nil {
		return artifacts.Ref{}, err
	}
	defer cleanup()
	err = s.session.store.withWrite(opCtx, func(conn *sql.Conn) error {
		if err := s.session.requireLease(opCtx, conn); err != nil {
			return err
		}
		var storedBytes, storedChunks int64
		err := conn.QueryRowContext(opCtx,
			"SELECT byte_count,chunk_count FROM artifact_blobs WHERE digest = ?", digest).Scan(&storedBytes, &storedChunks)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := conn.ExecContext(opCtx, `
				INSERT INTO artifact_blobs(digest,byte_count,chunk_count,created_ns)
				VALUES(?,?,?,?)`, digest, len(blob.Data), chunkCount, time.Now().UTC().UnixNano()); err != nil {
				return err
			}
			for index := 0; index < chunkCount; index++ {
				start := index * artifactChunkSize
				end := min(start+artifactChunkSize, len(blob.Data))
				if _, err := conn.ExecContext(opCtx, `
					INSERT INTO artifact_chunks(digest,chunk_index,data) VALUES(?,?,?)`,
					digest, index, blob.Data[start:end]); err != nil {
					return err
				}
			}
		case err != nil:
			return err
		default:
			if storedBytes != int64(len(blob.Data)) || storedChunks != int64(chunkCount) {
				return fmt.Errorf("%w: digest size mismatch", ErrArtifactCorrupt)
			}
			if err := verifyStoredArtifact(opCtx, conn, digest, storedBytes, storedChunks); err != nil {
				return err
			}
		}
		_, err = conn.ExecContext(opCtx,
			"INSERT OR IGNORE INTO session_artifacts(session_id,digest) VALUES(?,?)",
			s.session.id, digest)
		return err
	})
	if err != nil {
		return artifacts.Ref{}, s.session.mapError(ctx, err)
	}
	return ref, nil
}

func verifyStoredArtifact(ctx context.Context, conn *sql.Conn, digest []byte, expectedBytes, expectedChunks int64) error {
	rows, err := conn.QueryContext(ctx, `
		SELECT chunk_index,data FROM artifact_chunks
		WHERE digest = ? ORDER BY chunk_index`, digest)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasher := sha256.New()
	var index, size int64
	for rows.Next() {
		var storedIndex int64
		var data []byte
		if err := rows.Scan(&storedIndex, &data); err != nil {
			return err
		}
		if storedIndex != index || len(data) > artifactChunkSize {
			return fmt.Errorf("%w: invalid chunk ordering", ErrArtifactCorrupt)
		}
		if index < expectedChunks-1 && len(data) != artifactChunkSize {
			return fmt.Errorf("%w: short non-final chunk", ErrArtifactCorrupt)
		}
		_, _ = hasher.Write(data)
		size += int64(len(data))
		index++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if index != expectedChunks || size != expectedBytes || !equalBytes(hasher.Sum(nil), digest) {
		return fmt.Errorf("%w: digest or size mismatch", ErrArtifactCorrupt)
	}
	return nil
}

func (s *sqliteArtifactStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	if ctx != nil {
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
	}
	digest, err := artifactDigest(id)
	if err != nil {
		return nil, err
	}
	opCtx, cleanup, err := s.session.operationContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.session.requireLease(opCtx, s.session.store.db); err != nil {
		cleanup()
		return nil, s.session.mapError(ctx, err)
	}
	var byteCount, chunkCount int64
	err = s.session.store.db.QueryRowContext(opCtx, `
		SELECT artifact_blobs.byte_count,artifact_blobs.chunk_count
		FROM artifact_blobs
		JOIN session_artifacts ON session_artifacts.digest = artifact_blobs.digest
		WHERE session_artifacts.session_id = ? AND artifact_blobs.digest = ?`,
		s.session.id, digest).Scan(&byteCount, &chunkCount)
	if errors.Is(err, sql.ErrNoRows) {
		cleanup()
		return nil, os.ErrNotExist
	}
	if err != nil {
		cleanup()
		return nil, s.session.mapError(ctx, err)
	}
	if byteCount < 0 || chunkCount < 0 ||
		(byteCount == 0 && chunkCount != 0) ||
		(byteCount > 0 && chunkCount != (byteCount+artifactChunkSize-1)/artifactChunkSize) {
		cleanup()
		return nil, fmt.Errorf("%w: invalid artifact dimensions", ErrArtifactCorrupt)
	}
	return &artifactReader{
		store:      s.session.store,
		session:    s.session,
		ctx:        opCtx,
		cleanup:    cleanup,
		digest:     digest,
		expected:   byteCount,
		chunkCount: chunkCount,
		hasher:     sha256.New(),
	}, nil
}

func (s *sqliteArtifactStore) RemoveAll(ctx context.Context) error {
	opCtx, cleanup, err := s.session.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	err = s.session.store.withWrite(opCtx, func(conn *sql.Conn) error {
		if err := s.session.requireLease(opCtx, conn); err != nil {
			return err
		}
		if _, err := conn.ExecContext(opCtx,
			"DELETE FROM session_artifacts WHERE session_id = ?", s.session.id); err != nil {
			return err
		}
		return garbageCollectArtifacts(opCtx, conn)
	})
	if err == nil {
		s.session.store.incrementalVacuum(opCtx)
	}
	return s.session.mapError(ctx, err)
}

type artifactReader struct {
	store   *SQLiteStore
	session *sqliteSession
	ctx     context.Context
	cleanup func()
	digest  []byte
	hasher  hash.Hash

	expected   int64
	chunkCount int64
	chunkIndex int64
	readBytes  int64
	chunk      []byte
	chunkPos   int
	finished   bool
	closed     bool
	mu         sync.Mutex
}

func (r *artifactReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	if cause := context.Cause(r.session.ctx); cause != nil {
		return 0, cause
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		return 0, r.session.mapError(r.ctx, err)
	}
	if r.finished {
		return 0, io.EOF
	}

	for len(r.chunk) == r.chunkPos {
		if r.chunkIndex == r.chunkCount {
			r.finished = true
			var storedChunks int64
			if err := r.store.db.QueryRowContext(r.ctx,
				"SELECT count(*) FROM artifact_chunks WHERE digest = ?", r.digest).Scan(&storedChunks); err != nil {
				return 0, r.session.mapError(r.ctx, err)
			}
			if storedChunks != r.chunkCount {
				return 0, fmt.Errorf("%w: stored chunk count is %d, declared %d", ErrArtifactCorrupt, storedChunks, r.chunkCount)
			}
			if r.readBytes != r.expected || !equalBytes(r.hasher.Sum(nil), r.digest) {
				return 0, fmt.Errorf("%w: digest or size mismatch", ErrArtifactCorrupt)
			}
			return 0, io.EOF
		}
		var storedIndex int64
		var data []byte
		err := r.store.db.QueryRowContext(r.ctx, `
			SELECT chunk_index,data FROM artifact_chunks
			WHERE digest = ? AND chunk_index = ?`, r.digest, r.chunkIndex).Scan(&storedIndex, &data)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: missing chunk %d", ErrArtifactCorrupt, r.chunkIndex)
		}
		if err != nil {
			return 0, r.session.mapError(r.ctx, err)
		}
		if storedIndex != r.chunkIndex || len(data) > artifactChunkSize ||
			(r.chunkIndex < r.chunkCount-1 && len(data) != artifactChunkSize) {
			return 0, fmt.Errorf("%w: invalid chunk %d", ErrArtifactCorrupt, r.chunkIndex)
		}
		r.chunk = data
		r.chunkPos = 0
		r.chunkIndex++
	}

	n := copy(p, r.chunk[r.chunkPos:])
	_, _ = r.hasher.Write(r.chunk[r.chunkPos : r.chunkPos+n])
	r.chunkPos += n
	r.readBytes += int64(n)
	if r.readBytes > r.expected {
		return n, fmt.Errorf("%w: stored bytes exceed declared size", ErrArtifactCorrupt)
	}
	if r.chunkPos == len(r.chunk) {
		r.chunk = nil
		r.chunkPos = 0
	}
	return n, nil
}

func (r *artifactReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	r.chunk = nil
	r.cleanup()
	return nil
}

func artifactDigest(id string) ([]byte, error) {
	if !artifacts.ValidID(id) {
		return nil, artifacts.ErrInvalidID
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(id, "sha256:"))
	if err != nil {
		return nil, artifacts.ErrInvalidID
	}
	return digest, nil
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var different byte
	for i := range left {
		different |= left[i] ^ right[i]
	}
	return different == 0
}
