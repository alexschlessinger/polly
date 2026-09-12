package sessions

import (
	"context"
	"database/sql"
	"fmt"
)

// GetModelCache reads shared discovery data without acquiring a session lease.
func (s *SQLiteStore) GetModelCache(ctx context.Context, key string) ([]byte, error) {
	s.dbMu.RLock()
	defer s.dbMu.RUnlock()
	var data []byte
	err := s.db.QueryRowContext(ctx, "SELECT payload FROM model_metadata_cache WHERE cache_key = ?", key).Scan(&data)
	return data, err
}
func (s *SQLiteStore) PutModelCache(ctx context.Context, key string, data []byte) error {
	if len(data) > 16<<20 {
		return fmt.Errorf("model metadata cache record too large")
	}
	s.dbMu.RLock()
	defer s.dbMu.RUnlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO model_metadata_cache(cache_key,payload) VALUES(?,?) ON CONFLICT(cache_key) DO UPDATE SET payload=excluded.payload`, key, data)
	return err
}
func applySchemaV7(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS model_metadata_cache(cache_key TEXT PRIMARY KEY NOT NULL, payload BLOB NOT NULL) STRICT`); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, "PRAGMA user_version = 7")
	return err
}
