package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// checkpointDelay is how long an accepted update may wait before the store
// starts a checkpoint for it. Updates accepted meanwhile ride along, and a
// record changed many times in between is written once.
var checkpointDelay = 50 * time.Millisecond

// checkpointRows is about how many rows one checkpoint transaction writes. A
// transaction pays for every page it changes, and rows written together
// share pages, so a large one costs less a row. It also holds the database's
// one writing connection: this size keeps a turn at it to a fraction of a
// second, which is how long a lease renewal or a session writing its own
// pending rows may then wait.
var checkpointRows = 16384

const (
	// checkpointBytes starts a checkpoint before checkpointDelay is up.
	checkpointBytes = 8 << 20
	// maxPendingBytes and maxPendingRows bound the updates the database does
	// not hold yet. A caller that would exceed either waits, so a slow disk
	// slows the callers down instead of growing what a checkpoint has to
	// write, and with it how long anything that settles waits. They leave
	// room for several rounds of a large swarm: a record changed in each is
	// then written once, and a session's messages together.
	maxPendingBytes = 256 << 20
	maxPendingRows  = 1 << 18
)

// queuedUpdate is a coordination update the store has accepted and applied to
// the family it holds in memory, and has yet to write to the database.
type queuedUpdate struct {
	// parent is the family's root and parentToken the lease its handle held
	// when the update was accepted.
	parent, parentToken []byte
	changes             map[string]map[string]json.RawMessage
	// session, sequence and appended are the transcript the update appended
	// to and the encoded messages, starting at sequence; sessionToken is the
	// lease the session's handle held. transcript carries the sequence the
	// append left the transcript at.
	session, sessionToken []byte
	sequence              int64
	appended              [][]byte
	transcript            *TranscriptIdentity
	// outcome is how the session's first run ended, for a session that has
	// no outcome yet.
	outcome ReportStatus
	nowNS   int64
}

// pendingWindow is what the updates accepted since a checkpoint come to: the
// last value of every record they changed, and what they appended to each
// transcript. last is the place of the newest update in it.
type pendingWindow struct {
	last     uint64
	bytes    int
	rows     int
	families map[string]*pendingFamily
	sessions map[string]*pendingSession
}

type pendingFamily struct {
	root, token []byte
	// records holds a nil payload for a record the updates deleted.
	records map[string]map[string]json.RawMessage
}

type pendingSession struct {
	id, token []byte
	// tail are the encoded messages appended from sequence start on.
	start      int64
	tail       [][]byte
	transcript *TranscriptIdentity
	nowNS      int64
	// outcome is the first one the updates gave, noted at outcomeNS.
	outcome   ReportStatus
	outcomeNS int64
}

// without takes written, an earlier copy of the session that a transaction
// has written, out of it, and reports whether nothing is left pending.
func (s *pendingSession) without(written *pendingSession) (empty bool) {
	s.start += int64(len(written.tail))
	s.tail = s.tail[len(written.tail):]
	// An identity given since replaced the one written.
	if s.transcript == written.transcript {
		s.transcript = nil
	}
	// The outcome is the first one given, so the one written is this one.
	if written.outcome != "" {
		s.outcome = ""
	}
	return len(s.tail) == 0 && s.transcript == nil && s.outcome == ""
}

// pendingPart is what one window held for a session when a transaction came
// to write it: session is the copy the transaction writes, and of the pending
// session it is a copy of.
type pendingPart struct {
	session pendingSession
	of      *pendingSession
}

func newPendingWindow() *pendingWindow {
	return &pendingWindow{families: map[string]*pendingFamily{}, sessions: map[string]*pendingSession{}}
}

func (w *pendingWindow) add(update *queuedUpdate, place uint64) {
	w.last = place
	if len(update.changes) > 0 {
		family := w.families[string(update.parent)]
		// A lease taken again is another tenure: what the last one left
		// here can no longer be written.
		if family == nil || !bytes.Equal(family.token, update.parentToken) {
			family = &pendingFamily{root: update.parent, token: update.parentToken, records: map[string]map[string]json.RawMessage{}}
			w.families[string(update.parent)] = family
		}
		for kind, group := range update.changes {
			records := family.records[kind]
			if records == nil {
				records = make(map[string]json.RawMessage, len(group))
				family.records[kind] = records
			}
			for id, value := range group {
				if _, pending := records[id]; !pending {
					w.rows++
				}
				records[id] = value
				w.bytes += len(value)
			}
		}
	}
	if len(update.appended) == 0 && update.transcript == nil && update.outcome == "" {
		return
	}
	session := w.sessions[string(update.session)]
	if session == nil || !bytes.Equal(session.token, update.sessionToken) {
		session = &pendingSession{id: update.session, token: update.sessionToken, start: update.sequence}
		w.sessions[string(update.session)] = session
	}
	for _, payload := range update.appended {
		session.tail = append(session.tail, payload)
		w.bytes += len(payload)
		w.rows++
	}
	if update.transcript != nil {
		session.transcript = update.transcript
	}
	if update.outcome != "" && session.outcome == "" {
		session.outcome, session.outcomeNS = update.outcome, update.nowNS
	}
	// An outcome alone is no use of the session: one that has an outcome
	// already is left as it was.
	if len(update.appended) > 0 || update.transcript != nil {
		session.nowNS = update.nowNS
	}
}

// coordinationWriter takes the coordination updates the store answers from
// memory. An update is visible to this store the moment it is accepted. It is
// kept from then on in two ways:
//
//   - A disk store writes it to its update log, one write for every update
//     accepted since the last. Once that write returns the update survives a
//     crash of the process, which is what the update's caller waits for
//     (CoordinationState.Written). A memory store has nothing to survive.
//   - A checkpoint writes what the accepted updates come to into the
//     database: each changed record once, each transcript's new messages
//     together. Checkpoints run behind the callers, soon after an update
//     arrives and one after another while updates keep arriving.
//
// Until a checkpoint has written an update, the database is behind this
// store. An operation that reads or writes rows an update touches therefore
// either answers from what is pending here, or settles first: it asks for a
// checkpoint and waits for it. Transactions settle unless they say otherwise.
// The store's lease-free reads (List, Exists, GetMetadata, ListSummaries,
// GetLast) do neither, and report a session's message count, last-used time,
// transcript identity and spawn outcome as of the last checkpoint.
//
// A checkpoint writes a family's records only while the family's root is
// leased as it was when they were accepted, and a transcript's messages only
// while its session is; the same holds for whoever recovers the log of a
// store that is gone. A lease lost means someone else has gone on from what
// the database held, and the updates are dropped.
//
// A log write that fails stops the store taking updates this way; what it
// accepted still reaches the database. A checkpoint that fails is tried once
// more, and then every wait for the database reports the failure: the log
// keeps what the database lacks until a store that opens it later writes it.
type coordinationWriter struct {
	store *SQLiteStore

	mu sync.Mutex
	// Updates are numbered as they are accepted. logged and applied are the
	// places up to which the log and the database hold them; wanted is the
	// place a caller is waiting for the database to reach.
	accepted, logged, applied, wanted uint64
	// unlogged are the encoded updates from place unloggedFirst to
	// unloggedLast, which the log does not hold yet.
	unlogged, spare             []byte
	unloggedFirst, unloggedLast uint64
	// window collects the updates no checkpoint has taken; applying is the
	// window a checkpoint is writing. since is when window got its first.
	window, applying *pendingWindow
	since            time.Time
	logErr, applyErr error
	closed           bool
	// stopped is logErr != nil || applyErr != nil || closed, for healthy.
	stopped atomic.Bool
	// loggedMoved and appliedMoved are closed and replaced when the place
	// of that name moves or its error is set, so a waiter is woken only by
	// the progress it waits for.
	loggedMoved, appliedMoved chan struct{}
	wakeLog, wakeApply        chan struct{}
	started                   bool
	logDone, applyDone        chan struct{}
	// log is this handle's part of the update log; a memory store has none.
	log *updateLog
}

func newCoordinationWriter(store *SQLiteStore) *coordinationWriter {
	return &coordinationWriter{
		store: store, window: newPendingWindow(), loggedMoved: make(chan struct{}), appliedMoved: make(chan struct{}),
		wakeLog: make(chan struct{}, 1), wakeApply: make(chan struct{}, 1),
		logDone: make(chan struct{}), applyDone: make(chan struct{}),
	}
}

// openLog starts logging beside the database at path. Without a log a disk
// store cannot keep an update it has not written, so it stops answering
// updates from memory.
func (w *coordinationWriter) openLog(path string) {
	log, err := openUpdateLog(path)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.logErr = err
		w.stopped.Store(true)
		slog.Warn("update_log_unavailable", "error", err)
		return
	}
	w.log = log
}

// owner names this handle's part of the update log, or nothing.
func (w *coordinationWriter) owner() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.log == nil {
		return ""
	}
	return hex.EncodeToString(w.log.owner)
}

// healthy reports whether the writer can still take updates.
func (w *coordinationWriter) healthy() bool {
	return w != nil && !w.stopped.Load()
}

func wake(signal chan struct{}) {
	select {
	case signal <- struct{}{}:
	default:
	}
}

// accept takes update, waiting while too much is pending. It returns the
// update's place among those accepted, and whether nothing is left to wait
// for: otherwise wait takes the place.
func (w *coordinationWriter) accept(ctx context.Context, update *queuedUpdate) (place uint64, kept bool, err error) {
	w.mu.Lock()
	logging := w.log != nil
	w.mu.Unlock()
	var encoded []byte
	if logging {
		if encoded, err = appendLogUpdate(nil, update); err != nil {
			return 0, false, err
		}
	}
	w.mu.Lock()
	for {
		if err := w.failure(); err != nil {
			w.mu.Unlock()
			return 0, false, err
		}
		if w.closed {
			w.mu.Unlock()
			return 0, false, ErrStoreClosed
		}
		bytes, rows := w.window.bytes, w.window.rows
		if w.applying != nil {
			bytes, rows = bytes+w.applying.bytes, rows+w.applying.rows
		}
		if bytes < maxPendingBytes && rows < maxPendingRows {
			break
		}
		w.wanted = max(w.wanted, w.accepted)
		moved := w.appliedMoved
		w.mu.Unlock()
		wake(w.wakeApply)
		select {
		case <-moved:
		case <-ctx.Done():
			return 0, false, context.Cause(ctx)
		}
		w.mu.Lock()
	}
	if w.log != nil && encoded == nil {
		// The store was promoted to disk while this update was on its way.
		if encoded, err = appendLogUpdate(nil, update); err != nil {
			w.mu.Unlock()
			return 0, false, err
		}
	}
	w.accepted++
	place = w.accepted
	first := w.window.last == 0
	if first {
		w.since = time.Now()
	}
	w.window.add(update, place)
	kept = w.log == nil
	if kept {
		w.logged = place
	} else {
		if len(w.unlogged) == 0 {
			w.unloggedFirst = place
		}
		w.unlogged = append(w.unlogged, encoded...)
		w.unloggedLast = place
	}
	if !w.started {
		w.started = true
		go w.logLoop()
		go w.applyLoop()
	}
	early := w.window.bytes >= checkpointBytes
	w.mu.Unlock()
	if !kept {
		wake(w.wakeLog)
	}
	if first || early {
		wake(w.wakeApply)
	}
	return place, kept, nil
}

// failure is why the writer takes no more updates. The caller holds mu.
func (w *coordinationWriter) failure() error {
	if w.applyErr != nil {
		return w.applyErr
	}
	return w.logErr
}

// wait returns once the update accepted at place, and every one before it,
// would survive a crash of the process.
func (w *coordinationWriter) wait(ctx context.Context, place uint64) error {
	w.mu.Lock()
	for {
		if w.logged >= place || w.applied >= place {
			w.mu.Unlock()
			return nil
		}
		if err := w.failure(); err != nil {
			w.mu.Unlock()
			return err
		}
		logged, applied := w.loggedMoved, w.appliedMoved
		w.mu.Unlock()
		select {
		case <-logged:
		case <-applied:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		w.mu.Lock()
	}
}

// settle returns once the database holds every update accepted before the
// call. It asks for a checkpoint now instead of waiting for the next one.
func (w *coordinationWriter) settle(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	place := w.accepted
	for {
		if w.applied >= place {
			w.mu.Unlock()
			return nil
		}
		if w.applyErr != nil {
			err := w.applyErr
			w.mu.Unlock()
			return err
		}
		w.wanted = max(w.wanted, place)
		moved := w.appliedMoved
		w.mu.Unlock()
		wake(w.wakeApply)
		select {
		case <-moved:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		w.mu.Lock()
	}
}

// checkpointSoon asks for a checkpoint of what is pending without waiting.
func (w *coordinationWriter) checkpointSoon() {
	w.mu.Lock()
	w.wanted = max(w.wanted, w.accepted)
	w.mu.Unlock()
	wake(w.wakeApply)
}

// settleFamily settles when updates to the family rooted at root are pending.
func (w *coordinationWriter) settleFamily(ctx context.Context, root []byte) error {
	w.mu.Lock()
	pending := w.window.families[string(root)] != nil || w.applying != nil && w.applying.families[string(root)] != nil
	w.mu.Unlock()
	if !pending {
		return nil
	}
	return w.settle(ctx)
}

// pendingFor returns what is pending for the session id under the lease
// token, oldest first, for the caller to write in the transaction it is in.
// That includes what a checkpoint under way has not come to, so the caller
// waits for its turn at the connection and not for the checkpoint.
//
// What it returns stays pending until the caller says, with wrote, that its
// transaction committed, which it does before it gives up its turn. A
// checkpoint's transaction needs the turn too, so none comes between the
// session's rows reaching the database and their leaving here: a checkpoint
// never records that it wrote updates whose rows are still in a caller's
// hands. A caller that fails leaves everything as it was.
//
// What another lease left pending is not the caller's to write: a checkpoint
// drops it. When the checkpoint that holds part of it has failed, pendingFor
// fails as every wait for the database then does.
func (w *coordinationWriter) pendingFor(id, token []byte) ([]pendingPart, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var parts []pendingPart
	for _, window := range []*pendingWindow{w.applying, w.window} {
		if window == nil {
			continue
		}
		session := window.sessions[string(id)]
		if session == nil || !bytes.Equal(session.token, token) {
			continue
		}
		if window == w.applying && w.applyErr != nil {
			return nil, w.applyErr
		}
		// A copy: an update accepted meanwhile adds to the session in place.
		parts = append(parts, pendingPart{session: *session, of: session})
	}
	return parts, nil
}

// wrote takes parts, which a transaction wrote and committed, out of what is
// pending. The caller still holds its turn at the connection. What was
// accepted for a session since its part was copied stays pending. No caller
// has that today: a session's handle takes no update from memory while a
// write of its own is under way, and a lease being released has no handle.
func (w *coordinationWriter) wrote(parts []pendingPart) {
	if len(parts) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range parts {
		part := &parts[i]
		id := string(part.of.id)
		// The window the part came from may have become the checkpoint's.
		for _, window := range []*pendingWindow{w.applying, w.window} {
			if window != nil && window.sessions[id] == part.of && part.of.without(&part.session) {
				delete(window.sessions, id)
			}
		}
	}
}

// pendingTail returns the messages appended to the session id that the
// database may not hold yet, and the sequence of the first.
func (w *coordinationWriter) pendingTail(id []byte) (start int64, tail [][]byte) {
	if w == nil {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, window := range []*pendingWindow{w.applying, w.window} {
		if window == nil {
			continue
		}
		if session := window.sessions[string(id)]; session != nil && len(session.tail) > 0 {
			if tail == nil {
				start = session.start
			}
			tail = append(tail, session.tail...)
		}
	}
	return start, tail
}

// pendingIdentity returns the transcript identity, last-used time and spawn
// outcome that pending updates give the session id, when they give it any.
func (w *coordinationWriter) pendingIdentity(id []byte) (identity *TranscriptIdentity, usedNS int64, outcome ReportStatus) {
	if w == nil {
		return nil, 0, ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, window := range []*pendingWindow{w.applying, w.window} {
		if window == nil {
			continue
		}
		if session := window.sessions[string(id)]; session != nil {
			if session.transcript != nil {
				copied := *session.transcript
				identity = &copied
			}
			usedNS = max(usedNS, session.nowNS)
			if outcome == "" {
				outcome = session.outcome
			}
		}
	}
	return identity, usedNS, outcome
}

// close writes what is pending to the database and stops the writer.
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
	wake(w.wakeLog)
	wake(w.wakeApply)
	<-w.logDone
	<-w.applyDone
}

// finish gives the log up once the store's sessions are closed. A log whose
// updates all reached the database is removed.
func (w *coordinationWriter) finish() {
	w.mu.Lock()
	log := w.log
	w.log = nil
	clean := w.applyErr == nil && w.applying == nil && w.window.last == 0
	w.mu.Unlock()
	if log == nil {
		return
	}
	if clean {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		w.store.dbMu.RLock()
		err := w.store.execNow(ctx, `DELETE FROM update_log_marks WHERE owner = ?`, log.owner)
		w.store.dbMu.RUnlock()
		cancel()
		clean = err == nil
	}
	_ = log.close(clean)
}

// announce wakes the waiters on moved, one of loggedMoved and appliedMoved.
// The caller holds mu.
func announce(moved *chan struct{}) {
	close(*moved)
	*moved = make(chan struct{})
}

// logLoop writes accepted updates to the log, everything accepted since its
// last write in one frame.
func (w *coordinationWriter) logLoop() {
	defer close(w.logDone)
	for {
		w.mu.Lock()
		updates, first, last := w.unlogged, w.unloggedFirst, w.unloggedLast
		log, closed := w.log, w.closed
		if len(updates) > 0 {
			// The buffer being written is nobody's until the write is over.
			w.unlogged, w.spare = w.spare[:0], nil
		}
		w.mu.Unlock()
		if len(updates) == 0 {
			if closed {
				return
			}
			<-w.wakeLog
			continue
		}
		err := log.append(first, last, updates)
		w.mu.Lock()
		w.spare = updates[:0]
		if err != nil {
			// A frame cut short hides every frame after it from a reader,
			// so nothing more goes into this log.
			w.logErr = fmt.Errorf("accepted coordination updates were not logged: %w", err)
			w.stopped.Store(true)
			slog.Error("update_log_write_failed", "error", err)
		} else {
			w.logged = last
		}
		announce(&w.loggedMoved)
		w.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// applyLoop runs the checkpoints.
func (w *coordinationWriter) applyLoop() {
	defer close(w.applyDone)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		w.mu.Lock()
		if w.applyErr != nil {
			w.mu.Unlock()
			return
		}
		pending := w.window.last > 0
		wait := checkpointDelay - time.Since(w.since)
		if !pending || !w.closed && w.wanted <= w.applied && w.window.bytes < checkpointBytes && wait > 0 {
			closed := w.closed
			w.mu.Unlock()
			if closed {
				return
			}
			if !pending {
				<-w.wakeApply
				continue
			}
			timer.Reset(wait)
			select {
			case <-w.wakeApply:
			case <-timer.C:
			}
			continue
		}
		window := w.window
		w.applying, w.window = window, newPendingWindow()
		log := w.log
		w.mu.Unlock()
		err := w.apply(window, log)
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			err = w.apply(window, log)
		}
		w.mu.Lock()
		if err != nil {
			w.applyErr = fmt.Errorf("accepted coordination updates were not written: %w", err)
			w.stopped.Store(true)
			slog.Error("coordination_checkpoint_failed", "error", err)
		} else {
			w.applied, w.applying = max(w.applied, window.last), nil
		}
		announce(&w.appliedMoved)
		w.mu.Unlock()
		if err != nil {
			return
		}
		if log != nil {
			log.release(window.last)
		}
	}
}

// pendingRecord is one record a checkpoint writes; a nil value deletes it.
type pendingRecord struct {
	kind, id string
	value    json.RawMessage
}

// apply writes window to the database, in as many transactions as keep each
// near checkpointRows. It outlives every caller's context: the callers have
// already been told their updates succeeded. Writing a window twice leaves
// the database as writing it once does.
func (w *coordinationWriter) apply(window *pendingWindow, log *updateLog) error {
	type step struct {
		rows  int
		write func(context.Context, *sql.Conn) error
	}
	var steps []step
	for _, family := range window.families {
		var records []pendingRecord
		for kind, group := range family.records {
			for id, value := range group {
				records = append(records, pendingRecord{kind, id, value})
			}
		}
		for len(records) > 0 {
			chunk := records[:min(len(records), checkpointRows)]
			records = records[len(chunk):]
			steps = append(steps, step{len(chunk), func(ctx context.Context, conn *sql.Conn) error {
				return w.writePendingRecords(ctx, conn, family, chunk)
			}})
		}
	}
	// A session's handle, or a release of its lease, may write what is
	// pending for it in a transaction of its own before or between this
	// checkpoint's, and take it out of the window, so the map is read
	// under mu.
	w.mu.Lock()
	sessions := make([]*pendingSession, 0, len(window.sessions))
	for _, session := range window.sessions {
		sessions = append(sessions, session)
	}
	w.mu.Unlock()
	for {
		var writing []*pendingSession
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		err := w.store.withTransactionThen(ctx, "BEGIN IMMEDIATE", func(conn *sql.Conn) error {
			rows, taken := 0, 0
			for taken < len(steps) && rows < checkpointRows {
				if err := steps[taken].write(ctx, conn); err != nil {
					return err
				}
				rows += steps[taken].rows
				taken++
			}
			steps = steps[taken:]
			if len(steps) == 0 {
				writing, sessions = w.claim(window, sessions, checkpointRows-rows)
				for _, session := range writing {
					if err := writePendingSession(ctx, conn, session, true); err != nil {
						return err
					}
				}
			}
			if len(steps) > 0 || len(sessions) > 0 || log == nil {
				return nil
			}
			// The mark tells whoever recovers this log which of its updates
			// the database already holds.
			_, err := conn.ExecContext(ctx, `INSERT INTO update_log_marks(owner, place) VALUES(?, ?)
				ON CONFLICT(owner) DO UPDATE SET place = excluded.place`, log.owner, int64(window.last))
			return err
		}, func() {
			// Written, the sessions are no longer pending. A handle that
			// has the turn next must not find them here.
			w.mu.Lock()
			for _, session := range writing {
				delete(window.sessions, string(session.id))
			}
			w.mu.Unlock()
		})
		cancel()
		if err != nil || len(steps) == 0 && len(sessions) == 0 {
			return err
		}
	}
}

// claim returns the next sessions of window, about rows' worth of them, for
// the transaction that asked to write, and the rest. A session its handle
// wrote meanwhile is no longer pending.
func (w *coordinationWriter) claim(window *pendingWindow, sessions []*pendingSession, rows int) (claimed, rest []*pendingSession) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(sessions) > 0 && rows > 0 {
		session := sessions[0]
		sessions = sessions[1:]
		if window.sessions[string(session.id)] != session {
			continue
		}
		claimed = append(claimed, session)
		rows -= len(session.tail) + 2
	}
	return claimed, sessions
}

// writePendingRecords writes records of a family whose root is still leased
// as it was when they were accepted.
func (w *coordinationWriter) writePendingRecords(ctx context.Context, conn *sql.Conn, family *pendingFamily, records []pendingRecord) error {
	_, held, err := leasedSequence(ctx, conn, family.root, family.token)
	if err != nil {
		return err
	}
	if !held {
		// Memory may hold records the database never will.
		w.store.coordination.distrusted.Store(true)
		slog.Warn("coordination_updates_dropped", "family", hex.EncodeToString(family.root), "records", len(records))
		return nil
	}
	for _, record := range records {
		if record.value == nil {
			err = deleteRecord(ctx, conn, family.root, record.kind, record.id)
		} else {
			err = putRecord(ctx, conn, family.root, record.kind, record.id, record.value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// writePendingSession writes what is pending for one session: its new
// messages and its transcript identity. With fenced set it first checks that
// the session is still leased as it was when they were accepted; a caller
// that passes false has checked the lease in the same transaction.
func writePendingSession(ctx context.Context, conn *sql.Conn, session *pendingSession, fenced bool) error {
	next, held, err := leasedSequence(ctx, conn, session.id, session.token)
	if err != nil {
		return err
	}
	end := session.start + int64(len(session.tail))
	// A transcript at end holds the messages already: a checkpoint that was
	// cut short wrote them.
	if fenced && !held || len(session.tail) > 0 && next != session.start && next != end {
		slog.Warn("coordination_updates_dropped", "session", hex.EncodeToString(session.id), "messages", len(session.tail))
		return nil
	}
	appended := len(session.tail) > 0 && next == session.start
	if appended {
		if _, err := insertMessages(ctx, conn, session.id, session.start, session.tail); err != nil {
			return err
		}
	}
	// The session's row is changed once for the turn and the identity.
	switch {
	case appended && session.transcript != nil:
		err = recordTurnWithIdentity(ctx, conn, session.id, end, session.transcript, session.transcript.Sequence)
	case appended:
		err = recordTurn(ctx, conn, session.id, end, session.nowNS)
	case session.transcript != nil:
		err = writeTranscriptIdentity(ctx, conn, session.id, session.transcript, session.transcript.Sequence)
	}
	if err != nil {
		return err
	}
	if session.outcome != "" {
		return writeSpawnOutcome(ctx, conn, session.id, session.outcome, session.outcomeNS)
	}
	return nil
}
