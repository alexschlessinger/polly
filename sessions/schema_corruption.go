package sessions

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrStoreCorrupt fences writes after confirmed SQLite, foreign-key or schema
// corruption. The latch lasts for the handle's lifetime; repair requires closing
// the store and opening a new, fully validated handle.
var ErrStoreCorrupt = errors.New("session database is corrupt")

// Only a successful inspection proving an invariant missing/different uses this
// classification. SQL execution, scanning, cancellation and I/O errors retain
// their original identity and must not acquire the corruption sentinel.
func schemaCorruption(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrStoreCorrupt, fmt.Sprintf(format, args...))
}

// A required sqlite_master object absent from a completed lookup is a confirmed
// structural violation, unlike a lookup that failed to read the catalog.
func schemaObjectError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %w", ErrStoreCorrupt, err)
	}
	return err
}
