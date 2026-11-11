package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxQueuedUpdates bounds the updates waiting to be written. A caller
	// that would exceed it waits, so a slow disk slows the callers down
	// instead of growing the queue.
	maxQueuedUpdates = 4096
	// familyCheckEvery is how long updates and reads of a family are answered
	// from memory before one goes to the database again and so notices a
	// commit made through another connection.
	familyCheckEvery = 100 * time.Millisecond
)

// queuedUpdate is a coordination update the store has accepted and applied to
// the family it holds in memory, and has yet to write.
type queuedUpdate struct {
	parent  []byte
	changes map[string]map[string]json.RawMessage
	// session, sequence and appended are the transcript the update appended
	// to and the encoded messages, starting at sequence.
	session    []byte
	sequence   int64
	appended   [][]byte
	transcript *TranscriptIdentity
	nowNS      int64
}

// coordinationWriter writes accepted coordination updates behind their
// callers: one goroutine takes everything queued and commits it in a single
// transaction, in the order it was accepted. An update is therefore visible
// to this store the moment it returns and reaches the database a moment
// later; a process that dies in between loses it, which is why the update
// hands its caller a wait for the write (CoordinationState.Written). Every
// other operation that runs in a transaction first waits for the queue to
// empty, so it reads and writes the rows the accepted updates produced. What
// reads the database outside one does not wait: lease renewals, artifact
// reads and the store's lease-free reads (List, Exists, GetMetadata,
// ListSummaries, GetLast). Of the rows those read, an update here writes only
// a session's message count, last-used time and transcript identity, which
// they can report as they were before the next commit.
//
// A write that fails is tried once more. If it fails again the writer stops
// and every later coordination update and wait reports the failure: updates
// the callers were told had succeeded are not in the database, and nothing
// here can make that true again.
type coordinationWriter struct {
	store *SQLiteStore

	mu       sync.Mutex
	queue    []queuedUpdate
	accepted uint64
	written  uint64
	err      error
	closed   bool
	// stopped is err != nil || closed, for healthy.
	stopped atomic.Bool
	// moved is closed and replaced whenever written or err changes, or the
	// queue shrinks.
	moved   chan struct{}
	wake    chan struct{}
	started bool
	done    chan struct{}
}

func newCoordinationWriter(store *SQLiteStore) *coordinationWriter {
	return &coordinationWriter{store: store, moved: make(chan struct{}), wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// healthy reports whether the writer can still take updates.
func (w *coordinationWriter) healthy() bool {
	return w != nil && !w.stopped.Load()
}

// accept queues update, waiting while the queue is full. It returns the
// update's place among those accepted, which wait takes.
func (w *coordinationWriter) accept(ctx context.Context, update queuedUpdate) (uint64, error) {
	w.mu.Lock()
	for {
		if w.err != nil {
			err := w.err
			w.mu.Unlock()
			return 0, err
		}
		if w.closed {
			w.mu.Unlock()
			return 0, ErrStoreClosed
		}
		if len(w.queue) < maxQueuedUpdates {
			break
		}
		moved := w.moved
		w.mu.Unlock()
		select {
		case <-moved:
		case <-ctx.Done():
			return 0, context.Cause(ctx)
		}
		w.mu.Lock()
	}
	w.queue = append(w.queue, update)
	w.accepted++
	place := w.accepted
	if !w.started {
		w.started = true
		go w.run()
	}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return place, nil
}

// wait returns once the update accepted at place, and every one before it,
// is written.
func (w *coordinationWriter) wait(ctx context.Context, place uint64) error {
	w.mu.Lock()
	for {
		if w.written >= place {
			w.mu.Unlock()
			return nil
		}
		if w.err != nil {
			err := w.err
			w.mu.Unlock()
			return err
		}
		moved := w.moved
		w.mu.Unlock()
		select {
		case <-moved:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		w.mu.Lock()
	}
}

// settle returns once every update accepted before the call is written.
func (w *coordinationWriter) settle(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	place := w.accepted
	w.mu.Unlock()
	return w.wait(ctx, place)
}

// close writes what is queued and stops the writer.
func (w *coordinationWriter) close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.closed = true
	w.stopped.Store(true)
	started := w.started
	w.mu.Unlock()
	if !started {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
	<-w.done
}

func (w *coordinationWriter) announce() {
	close(w.moved)
	w.moved = make(chan struct{})
}

func (w *coordinationWriter) run() {
	defer close(w.done)
	for {
		w.mu.Lock()
		batch := w.queue
		w.queue = nil
		closed := w.closed
		if len(batch) > 0 {
			// Room in the queue is news to a caller waiting to add to it.
			w.announce()
		}
		w.mu.Unlock()
		if len(batch) == 0 {
			if closed {
				return
			}
			<-w.wake
			continue
		}
		err := w.write(batch)
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			err = w.write(batch)
		}
		w.mu.Lock()
		if err != nil {
			w.err = fmt.Errorf("accepted coordination updates were not written: %w", err)
			w.stopped.Store(true)
			slog.Error("coordination_write_behind_failed", "updates", len(batch), "error", err)
		} else {
			w.written += uint64(len(batch))
		}
		w.announce()
		w.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// write commits batch in one transaction. It outlives every caller's
// context: the callers have already been told their updates succeeded.
func (w *coordinationWriter) write(batch []queuedUpdate) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return w.store.withTransactionNow(ctx, "BEGIN IMMEDIATE", func(conn *sql.Conn) error {
		for _, update := range batch {
			if err := writeRecordDelta(ctx, conn, update.parent, update.changes); err != nil {
				return err
			}
			next := update.sequence + int64(len(update.appended))
			if update.transcript != nil {
				if err := writeTranscriptIdentity(ctx, conn, update.session, update.transcript, next); err != nil {
					return err
				}
			}
			if len(update.appended) == 0 {
				continue
			}
			if _, err := insertMessages(ctx, conn, update.session, update.sequence, update.appended); err != nil {
				return err
			}
			if err := recordTurn(ctx, conn, update.session, next, update.nowNS); err != nil {
				return err
			}
		}
		return nil
	})
}
