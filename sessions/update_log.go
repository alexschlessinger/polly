package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// A disk store keeps an update log beside its database: a directory of
// append-only files, one set per store handle. A coordination update answered
// from memory is written there first and reaches the database at the handle's
// next checkpoint, so the log is what a crash of the process leaves of the
// updates the database does not hold yet.
//
// A file starts with a header naming its owner and holds frames, each one
// write of the updates accepted since the last. A frame carries its length
// and a checksum over the length and the updates. Nothing is synced: like the
// database, the log survives the process and may lose its newest frames to a
// power loss. A reader takes frames while they verify and stops at the first
// that does not, so what it reads is always a prefix of what was accepted.
//
// The owner holds a lock on a file of its own for as long as it lives. A
// handle that finds another owner's files and can take that lock knows the
// owner is gone, writes what the files hold to the database and removes them.
const (
	logDirSuffix    = "-log"
	logMagic        = "pollylog"
	logVersion      = 1
	logHeaderBytes  = len(logMagic) + 4 + logIDBytes
	logIDBytes      = 16
	logFrameHeader  = 8
	logSegmentBytes = 16 << 20
	// The flags of what an update carries besides messages and records.
	logCarriesTranscript = 1
	logCarriesOutcome    = 2
)

var logChecksums = crc32.MakeTable(crc32.Castagnoli)

// appendLogUpdate encodes update after dst.
func appendLogUpdate(dst []byte, update *queuedUpdate) ([]byte, error) {
	for _, id := range [][]byte{update.parent, update.parentToken, update.session, update.sessionToken} {
		if len(id) != logIDBytes {
			return nil, errors.New("coordination update names a malformed session")
		}
		dst = append(dst, id...)
	}
	dst = binary.AppendVarint(dst, update.sequence)
	dst = binary.AppendVarint(dst, update.nowNS)
	dst = binary.AppendUvarint(dst, uint64(len(update.appended)))
	for _, payload := range update.appended {
		dst = appendLogBytes(dst, payload)
	}
	// What else the update carries for its session, as flags, then each in
	// the order of its flag.
	var identity []byte
	var carried uint64
	if update.transcript != nil {
		var err error
		if identity, err = json.Marshal(update.transcript); err != nil {
			return nil, err
		}
		carried |= logCarriesTranscript
	}
	if update.outcome != "" {
		carried |= logCarriesOutcome
	}
	dst = binary.AppendUvarint(dst, carried)
	if update.transcript != nil {
		dst = appendLogBytes(dst, identity)
	}
	if update.outcome != "" {
		dst = appendLogBytes(dst, []byte(update.outcome))
	}
	dst = binary.AppendUvarint(dst, uint64(len(update.changes)))
	for kind, group := range update.changes {
		dst = appendLogBytes(dst, []byte(kind))
		dst = binary.AppendUvarint(dst, uint64(len(group)))
		for id, value := range group {
			dst = appendLogBytes(dst, []byte(id))
			if value == nil {
				dst = binary.AppendUvarint(dst, 0)
				continue
			}
			dst = binary.AppendUvarint(dst, 1)
			dst = appendLogBytes(dst, value)
		}
	}
	return dst, nil
}

func appendLogBytes(dst, value []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

// logDecoder reads what appendLogUpdate wrote. A read past the end sets bad
// and returns zero values, so a caller checks once after decoding.
type logDecoder struct {
	data []byte
	bad  bool
}

func (d *logDecoder) fixed(n int) []byte {
	if d.bad || len(d.data) < n {
		d.bad = true
		return nil
	}
	value := d.data[:n:n]
	d.data = d.data[n:]
	return value
}

func (d *logDecoder) uvarint() uint64 {
	value, n := binary.Uvarint(d.data)
	if d.bad || n <= 0 {
		d.bad = true
		return 0
	}
	d.data = d.data[n:]
	return value
}

func (d *logDecoder) varint() int64 {
	value, n := binary.Varint(d.data)
	if d.bad || n <= 0 {
		d.bad = true
		return 0
	}
	d.data = d.data[n:]
	return value
}

func (d *logDecoder) bytes() []byte {
	n := d.uvarint()
	if n > uint64(len(d.data)) {
		d.bad = true
		return nil
	}
	return d.fixed(int(n))
}

// count reads a number of items that each take at least one byte.
func (d *logDecoder) count() int {
	n := d.uvarint()
	if n > uint64(len(d.data)) {
		d.bad = true
		return 0
	}
	return int(n)
}

func (d *logDecoder) update() queuedUpdate {
	update := queuedUpdate{
		parent: d.fixed(logIDBytes), parentToken: d.fixed(logIDBytes),
		session: d.fixed(logIDBytes), sessionToken: d.fixed(logIDBytes),
		sequence: d.varint(), nowNS: d.varint(),
	}
	for n := d.count(); n > 0 && !d.bad; n-- {
		update.appended = append(update.appended, d.bytes())
	}
	carried := d.uvarint()
	if carried&^(logCarriesTranscript|logCarriesOutcome) != 0 {
		d.bad = true
	}
	if carried&logCarriesTranscript != 0 {
		var identity TranscriptIdentity
		if json.Unmarshal(d.bytes(), &identity) != nil {
			d.bad = true
		}
		update.transcript = &identity
	}
	if carried&logCarriesOutcome != 0 {
		update.outcome = ReportStatus(d.bytes())
	}
	for kinds := d.count(); kinds > 0 && !d.bad; kinds-- {
		kind := string(d.bytes())
		group := map[string]json.RawMessage{}
		for ids := d.count(); ids > 0 && !d.bad; ids-- {
			id := string(d.bytes())
			var value json.RawMessage
			if d.uvarint() == 1 {
				value = d.bytes()
			}
			group[id] = value
		}
		if update.changes == nil {
			update.changes = map[string]map[string]json.RawMessage{}
		}
		update.changes[kind] = group
	}
	return update
}

// loggedUpdate is an update read back from a log, with its place among those
// its owner accepted.
type loggedUpdate struct {
	place uint64
	queuedUpdate
}

// appendLogFrame writes the frame holding updates, the encoded updates from
// place first on, after dst.
func appendLogFrame(dst []byte, first uint64, updates []byte) []byte {
	start := len(dst)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(8+len(updates)))
	dst = binary.LittleEndian.AppendUint32(dst, 0)
	dst = binary.LittleEndian.AppendUint64(dst, first)
	dst = append(dst, updates...)
	sum := crc32.Update(crc32.Checksum(dst[start:start+4], logChecksums), logChecksums, dst[start+logFrameHeader:])
	binary.LittleEndian.PutUint32(dst[start+4:], sum)
	return dst
}

// readLogFile returns the owner of a log file and the updates in the frames
// that verify, in order. A file cut short, or one whose tail is not what its
// owner wrote, yields the updates before the damage.
func readLogFile(data []byte) (owner []byte, updates []loggedUpdate, err error) {
	if len(data) < logHeaderBytes || string(data[:len(logMagic)]) != logMagic {
		return nil, nil, errors.New("not an update log")
	}
	if version := binary.LittleEndian.Uint32(data[len(logMagic):]); version != logVersion {
		return nil, nil, fmt.Errorf("update log version %d is not supported", version)
	}
	owner = data[len(logMagic)+4 : logHeaderBytes]
	data = data[logHeaderBytes:]
	for len(data) >= logFrameHeader {
		size := int(binary.LittleEndian.Uint32(data))
		// An empty frame is never written, and a run of zeros would read as one.
		if size < 8 || size > len(data)-logFrameHeader {
			break
		}
		body := data[logFrameHeader : logFrameHeader+size]
		sum := crc32.Update(crc32.Checksum(data[:4], logChecksums), logChecksums, body)
		if sum != binary.LittleEndian.Uint32(data[4:]) {
			break
		}
		place := binary.LittleEndian.Uint64(body)
		decoder := logDecoder{data: body[8:]}
		var frame []loggedUpdate
		for len(decoder.data) > 0 && !decoder.bad {
			frame = append(frame, loggedUpdate{place, decoder.update()})
			place++
		}
		if decoder.bad {
			break
		}
		updates = append(updates, frame...)
		data = data[logFrameHeader+size:]
	}
	return owner, updates, nil
}

// updateLog is one store handle's part of the update log.
type updateLog struct {
	dir   string
	owner []byte
	// lock stays locked while the owner lives.
	lock *os.File

	mu     sync.Mutex
	active *os.File
	size   int64
	number int
	// files are the owner's log files, oldest first, each with the place of
	// the last update it holds. The last one is the active file.
	files []logFile
	frame []byte
}

type logFile struct {
	path string
	last uint64
}

func logDir(databasePath string) string { return databasePath + logDirSuffix }

func logLockPath(dir string, owner []byte) string {
	return filepath.Join(dir, hex.EncodeToString(owner)+".lock")
}

// openUpdateLog creates a new owner's part of the log beside the database.
func openUpdateLog(databasePath string) (*updateLog, error) {
	dir := logDir(databasePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create update log directory: %w", err)
	}
	owner, err := randomBytes(logIDBytes)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(logLockPath(dir, owner), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create update log lock: %w", err)
	}
	if held, err := lockLogFile(lock); err != nil || !held {
		if err == nil {
			err = errors.New("this platform cannot lock it")
		}
		return nil, fmt.Errorf("lock update log: %w", errors.Join(err, lock.Close(), os.Remove(lock.Name())))
	}
	return &updateLog{dir: dir, owner: owner, lock: lock}, nil
}

// append writes one frame holding updates, whose places run from first to last.
func (l *updateLog) append(first, last uint64, updates []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active == nil || l.size >= logSegmentBytes {
		if err := l.rotate(); err != nil {
			return err
		}
	}
	l.frame = appendLogFrame(l.frame[:0], first, updates)
	n, err := l.active.Write(l.frame)
	l.size += int64(n)
	if err != nil {
		return fmt.Errorf("write update log: %w", err)
	}
	l.files[len(l.files)-1].last = last
	return nil
}

// rotate starts a new active file. The caller holds mu.
func (l *updateLog) rotate() error {
	l.number++
	path := filepath.Join(l.dir, fmt.Sprintf("%x-%08d.log", l.owner, l.number))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("create update log file: %w", err)
	}
	header := append([]byte(logMagic), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(header[len(logMagic):], logVersion)
	header = append(header, l.owner...)
	if _, err := file.Write(header); err != nil {
		_ = file.Close()
		return fmt.Errorf("write update log header: %w", errors.Join(err, os.Remove(path)))
	}
	if l.active != nil {
		_ = l.active.Close()
	}
	l.active, l.size = file, int64(len(header))
	l.files = append(l.files, logFile{path: path})
	return nil
}

// release removes the files whose every update the database holds, which is
// every update up to place applied. The active file stays.
func (l *updateLog) release(applied uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.files) > 1 && l.files[0].last <= applied {
		if removeIfPresent(l.files[0].path) != nil {
			return
		}
		l.files = l.files[1:]
	}
}

// close gives the log up. With remove set the database holds every update, so
// the owner's files go; otherwise they stay for whoever opens the store next.
func (l *updateLog) close(remove bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var err error
	if l.active != nil {
		err = l.active.Close()
		l.active = nil
	}
	if remove {
		for _, file := range l.files {
			err = errors.Join(err, os.Remove(file.path))
		}
		l.files = nil
	}
	lock := l.lock.Name()
	err = errors.Join(err, l.lock.Close())
	if remove {
		err = errors.Join(err, os.Remove(lock))
	}
	return err
}

// applyLoggedSession writes one update's transcript part as its owner would
// have, when the session's lease is still the one the update was accepted
// under and the transcript is where the update left it.
func applyLoggedSession(ctx context.Context, conn *sql.Conn, update *queuedUpdate) error {
	if len(update.appended) == 0 && update.transcript == nil && update.outcome == "" {
		return nil
	}
	next, held, err := leasedSequence(ctx, conn, update.session, update.sessionToken)
	if err != nil || !held {
		return err
	}
	end := update.sequence + int64(len(update.appended))
	if update.transcript != nil {
		if err := writeTranscriptIdentity(ctx, conn, update.session, update.transcript, end); err != nil {
			return err
		}
	}
	if update.outcome != "" {
		if err := writeSpawnOutcome(ctx, conn, update.session, update.outcome, update.nowNS); err != nil {
			return err
		}
	}
	// A transcript past the update already holds it; one short of it lost
	// an earlier update, and this one cannot follow.
	if len(update.appended) == 0 || next != update.sequence {
		return nil
	}
	if _, err := insertMessages(ctx, conn, update.session, update.sequence, update.appended); err != nil {
		return err
	}
	return recordTurn(ctx, conn, update.session, end, update.nowNS)
}

// leasedSequence reads a session's next sequence and whether its lease is
// still the one token names.
func leasedSequence(ctx context.Context, conn rowQuerier, id, token []byte) (next int64, held bool, err error) {
	var owner []byte
	err = conn.QueryRowContext(ctx, `SELECT s.next_sequence, l.owner_token FROM sessions s
		LEFT JOIN session_leases l ON l.session_id = s.id WHERE s.id = ?`, id).Scan(&next, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return next, bytes.Equal(owner, token), err
}

// errRecoverLogs is tryAcquire's request to recover the update log before it
// takes a lease its holder let run out.
var errRecoverLogs = errors.New("recover the update log first")

// recoverLogs writes to the database what owners that are gone left in the
// update log, and removes their files. An update's records are written only
// while the family's root is still leased as it was when the update was
// accepted, and its transcript part only while its session is: a lease taken
// since means someone has gone on from what the database held. The caller
// says whether it holds the coordination cache, which forgets every family
// once records were written behind it.
func (s *SQLiteStore) recoverLogs(ctx context.Context, holdingCache bool) error {
	s.dbMu.RLock()
	mode, dir := s.mode, logDir(s.path)
	s.dbMu.RUnlock()
	if mode != ModeDisk {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read update log directory: %w", err)
	}
	own := s.writer.owner()
	owners := map[string][]string{}
	for _, entry := range entries {
		name := entry.Name()
		if len(name) < 2*logIDBytes || entry.IsDir() {
			continue
		}
		owner := name[:2*logIDBytes]
		if owner == own {
			continue
		}
		if strings.HasSuffix(name, ".log") {
			owners[owner] = append(owners[owner], filepath.Join(dir, name))
		} else if _, seen := owners[owner]; !seen {
			owners[owner] = nil
		}
	}
	recovered := false
	for owner, files := range owners {
		id, err := hex.DecodeString(owner)
		if err != nil {
			continue
		}
		wrote, err := s.recoverOwner(ctx, dir, id, files)
		if err != nil {
			return fmt.Errorf("recover update log of %s: %w", owner, err)
		}
		recovered = recovered || wrote
	}
	if recovered {
		cache := s.coordination
		if !holdingCache {
			if err := cache.hold(ctx); err != nil {
				return err
			}
			defer cache.release()
		}
		cache.clear()
	}
	return nil
}

// recoverOwner recovers one owner's files if the owner is gone, and reports
// whether it wrote anything to the database.
func (s *SQLiteStore) recoverOwner(ctx context.Context, dir string, owner []byte, files []string) (wrote bool, err error) {
	lock, err := os.OpenFile(logLockPath(dir, owner), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return false, err
	}
	// The lock file goes last, after it is closed: an open file cannot be
	// removed everywhere. Whoever locks it in between finds nothing to do.
	done := false
	defer func() {
		closeErr := lock.Close()
		if done {
			err = errors.Join(err, closeErr, removeIfPresent(lock.Name()))
		}
	}()
	if held, err := lockLogFile(lock); err != nil || !held {
		// The owner is alive and writes its own updates.
		return false, err
	}
	forget := func() error {
		s.dbMu.RLock()
		defer s.dbMu.RUnlock()
		return s.execNow(ctx, `DELETE FROM update_log_marks WHERE owner = ?`, owner)
	}
	if len(files) == 0 {
		done = true
		return false, forget()
	}
	sort.Strings(files)
	var updates []loggedUpdate
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		named, read, err := readLogFile(data)
		if err != nil || !bytes.Equal(named, owner) {
			// Not a log this version reads. Its updates stay where they
			// are, for a store that can read them.
			slog.Warn("update_log_left_unread", "file", path, "error", err)
			return false, nil
		}
		updates = append(updates, read...)
	}
	var last uint64
	if len(updates) > 0 {
		last = updates[len(updates)-1].place
	}
	err = s.withTransactionNow(ctx, "BEGIN IMMEDIATE", func(conn *sql.Conn) error {
		var mark uint64
		err := conn.QueryRowContext(ctx, `SELECT place FROM update_log_marks WHERE owner = ?`, owner).Scan(&mark)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		roots := map[string]bool{}
		for i := range updates {
			update := &updates[i]
			if update.place <= mark {
				continue
			}
			wrote = true
			if len(update.changes) > 0 {
				held, known := roots[string(update.parent)+string(update.parentToken)]
				if !known {
					if _, held, err = leasedSequence(ctx, conn, update.parent, update.parentToken); err != nil {
						return err
					}
					roots[string(update.parent)+string(update.parentToken)] = held
				}
				if held {
					if _, err := checkChanges(update.changes); err != nil {
						return err
					}
					if err := writeRecordDelta(ctx, conn, update.parent, update.changes); err != nil {
						return err
					}
				}
			}
			if err := applyLoggedSession(ctx, conn, &update.queuedUpdate); err != nil {
				return err
			}
		}
		// The mark outlives the files: a recovery that stops before it
		// removes them finds nothing left to write the next time.
		_, err = conn.ExecContext(ctx, `INSERT INTO update_log_marks(owner, place) VALUES(?, ?)
			ON CONFLICT(owner) DO UPDATE SET place = max(place, excluded.place)`, owner, int64(last))
		return err
	})
	if err != nil {
		return false, err
	}
	for _, path := range files {
		if err := removeIfPresent(path); err != nil {
			return wrote, err
		}
	}
	done = true
	return wrote, forget()
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
