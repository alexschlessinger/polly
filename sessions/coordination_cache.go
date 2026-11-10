package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
)

// maxCachedFamilies bounds the families whose records stay in memory. A
// process normally drives a handful; past the bound the cache starts over.
const maxCachedFamilies = 64

// coordinationCache holds each family's records in memory so a read or an
// update does not scan the table for them. The table stays the durable copy:
// every update still writes its changes before it returns.
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
	conn     any
	version  int64
	families map[string]cachedFamily
	// revisions never repeats, so equal revisions mean equal records even
	// after a family was dropped and read again.
	revisions uint64
}

type cachedFamily struct {
	records  map[string]map[string]json.RawMessage
	revision uint64
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
	if driver != c.conn || version != c.version {
		clear(c.families)
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
		clear(c.families)
	}
	c.revisions++
	c.families[string(parent)] = cachedFamily{records, c.revisions}
	return records, c.revisions, nil
}

// replace records the family's committed records and returns their revision.
// The caller holds the cache and gives up records.
func (c *coordinationCache) replace(parent []byte, records map[string]map[string]json.RawMessage) uint64 {
	c.revisions++
	if _, ok := c.families[string(parent)]; ok {
		c.families[string(parent)] = cachedFamily{records, c.revisions}
	}
	return c.revisions
}

// forget drops a family whose stored records may no longer match memory.
func (c *coordinationCache) forget(parent []byte) { delete(c.families, string(parent)) }

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
