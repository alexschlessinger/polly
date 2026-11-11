package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"sync/atomic"
)

// maxCachedFamilies bounds the families whose records stay in memory. A
// process normally drives a handful; past the bound the cache starts over.
const maxCachedFamilies = 64

// coordinationCache holds each family's records in memory so a read or an
// update does not scan the table for them. For a family this store drives,
// one whose root session it holds, memory is ahead of the table between
// checkpoints and is what every read and update answers from;
// coordinationWriter writes the changes behind them. For any other family the
// table is the truth and memory only spares a scan while nothing was
// committed through another connection.
//
// Callers hold the cache across their whole transaction and the bookkeeping
// that follows its commit, so nobody sees a family between the two. A caller
// outside this package gets a copy: it may edit a payload's bytes in place.
type coordinationCache struct {
	held chan struct{}
	// conn and version identify the database content the families were read
	// from. SQLite's data_version moves when another connection, in this
	// process or another, commits; a different connection has a counter of
	// its own. Either difference discards every family.
	conn    any
	version int64
	// mu lets a reader that does not hold the cache ask a family's revision.
	// Only the holder writes families, and it takes mu to do so.
	mu       sync.RWMutex
	families map[string]cachedFamily
	// revisions never repeats, so equal revisions mean equal records even
	// after a family was dropped and read again.
	revisions uint64
	// distrusted is set by a checkpoint that could not write a family's
	// records: memory then holds what the table never will, and the next
	// holder forgets every family.
	distrusted atomic.Bool
}

type cachedFamily struct {
	records  map[string]map[string]json.RawMessage
	revision uint64
}

// family returns the family as memory holds it. The caller holds the cache
// and has made sure this store drives the family.
func (c *coordinationCache) family(parent []byte) (cachedFamily, bool) {
	if c.distrusted.CompareAndSwap(true, false) {
		c.clear()
	}
	family, ok := c.families[string(parent)]
	return family, ok
}

// peek is family's revision for a caller that does not hold the cache.
func (c *coordinationCache) peek(parent string) (uint64, bool) {
	if c.distrusted.Load() {
		return 0, false
	}
	c.mu.RLock()
	family, ok := c.families[parent]
	c.mu.RUnlock()
	return family.revision, ok
}

func (c *coordinationCache) set(parent []byte, family cachedFamily) {
	c.mu.Lock()
	c.families[string(parent)] = family
	c.mu.Unlock()
}

func (c *coordinationCache) clear() {
	c.mu.Lock()
	clear(c.families)
	c.mu.Unlock()
}

func newCoordinationCache() *coordinationCache {
	return &coordinationCache{held: make(chan struct{}, 1), families: map[string]cachedFamily{}}
}

// hold waits for exclusive use of the cache.
func (c *coordinationCache) hold(ctx context.Context) error {
	select {
	case c.held <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (c *coordinationCache) release() { <-c.held }

// records returns the family's records as conn's open transaction sees them,
// and their revision, reading the table only when memory does not hold them.
// The caller holds the cache, has already read from the transaction, and must
// not modify the result.
func (c *coordinationCache) records(ctx context.Context, conn *sql.Conn, parent []byte) (map[string]map[string]json.RawMessage, uint64, error) {
	var version int64
	if err := conn.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&version); err != nil {
		return nil, 0, err
	}
	var driver any
	if err := conn.Raw(func(d any) error { driver = d; return nil }); err != nil {
		return nil, 0, err
	}
	if driver != c.conn || version != c.version || c.distrusted.CompareAndSwap(true, false) {
		c.clear()
		c.conn, c.version = driver, version
	}
	if family, ok := c.families[string(parent)]; ok {
		return family.records, family.revision, nil
	}
	records, err := readSwarmRecords(ctx, conn, parent)
	if err != nil {
		return nil, 0, err
	}
	if len(c.families) >= maxCachedFamilies {
		c.clear()
	}
	c.revisions++
	c.set(parent, cachedFamily{records, c.revisions})
	return records, c.revisions, nil
}

// replace records the family's committed records and returns their revision.
// The caller holds the cache and gives up records.
func (c *coordinationCache) replace(parent []byte, records map[string]map[string]json.RawMessage) uint64 {
	c.revisions++
	if _, ok := c.families[string(parent)]; ok {
		c.set(parent, cachedFamily{records, c.revisions})
	}
	return c.revisions
}

// forget drops a family whose stored records may no longer match memory.
func (c *coordinationCache) forget(parent []byte) {
	c.mu.Lock()
	delete(c.families, string(parent))
	c.mu.Unlock()
}

// applyRecordChanges moves changes into records, the cache's own maps: a nil
// payload deletes.
func applyRecordChanges(records, changes map[string]map[string]json.RawMessage) {
	for kind, group := range changes {
		for id, value := range group {
			if value == nil {
				delete(records[kind], id)
				continue
			}
			if records[kind] == nil {
				records[kind] = map[string]json.RawMessage{}
			}
			records[kind][id] = value
		}
	}
}

// copyRecords returns records with maps and payload bytes of its own. The
// payloads share one allocation, each capped so an append cannot reach the
// next.
func copyRecords(records map[string]map[string]json.RawMessage) map[string]map[string]json.RawMessage {
	size := 0
	for _, group := range records {
		for _, value := range group {
			size += len(value)
		}
	}
	bytes := make([]byte, 0, size)
	out := make(map[string]map[string]json.RawMessage, len(records))
	for kind, group := range records {
		copied := make(map[string]json.RawMessage, len(group))
		for id, value := range group {
			start := len(bytes)
			bytes = append(bytes, value...)
			copied[id] = bytes[start:len(bytes):len(bytes)]
		}
		out[kind] = copied
	}
	return out
}
