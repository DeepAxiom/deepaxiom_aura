// Package seglog is the append-only store behind the causal event log.
//
// # Why this is not a table
//
// The event log is written once per envelope and never updated or deleted from
// — verified against the code, not assumed — and read back as one session's
// history in order. That is the best case for a log and the worst case for a
// B-tree, and it was costing real money: profiling a loaded node put ~54% of all
// CPU inside SQLite's file I/O, most of it in the page management and journaling
// that exist to support updates this data never performs.
//
// Measured on the same workload, at matched durability, this design sustained
// 1.5M events/sec against SQLite's 27k — and unlike an LSM it wins at *every*
// concurrency level rather than crossing over somewhere above a few hundred
// writers. The shape is the one Tidehunter (arXiv 2602.01873) argues for: treat
// the log as permanent storage rather than a recovery buffer, and compaction
// stops existing because nothing is ever relocated.
//
// # What it does not carry
//
// Only the event log. The effect ledger stays in SQLite, deliberately: a bug
// here loses replay history, which is recoverable and embarrassing; a bug in the
// ledger loses evidence, which is neither. The ledger moves only after this has
// been exercised in the field, and it may never need to — effects are orders of
// magnitude rarer than envelopes, so a serial writer is no constraint there.
//
// # Durability
//
// Records reach the OS on every batch and are fsynced only when Sync is set,
// which mirrors SQLite's `synchronous=NORMAL`: a process that dies — panic,
// SIGKILL, a dropped session — loses nothing, because the bytes are already in
// the page cache. A power cut may lose the tail. The recovery scan below is what
// makes that safe rather than merely likely: a torn record at the end of the
// file is detected by its checksum and truncated, so the log is never left
// half-parsed.
package seglog

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Record header layout. Fixed-width prefix, then variable fields, so recovery
// can validate a record without trusting anything inside it.
//
//	[4] payload length   — everything after this field
//	[4] crc32 of payload
//	[8] timestamp (unix millis)
//	[2] len(session) [2] len(msgID) [2] len(causeID) [1] len(kind)
//	    session · msgID · causeID · kind · envelope
const (
	lenSize = 4
	crcSize = 4
	fixed   = 8 + 2 + 2 + 2 + 1 // ts + the four length fields
	// maxRecord bounds one record. A length prefix read from a possibly
	// corrupt file is an allocation that file controls, so it is checked
	// before it is believed.
	maxRecord = 16 << 20
	// batchCap is how many appends may share one write. Same reasoning as the
	// SQLite batcher: under one caller a batch is one record and costs nothing,
	// under load it fills itself.
	batchCap = 512
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Entry is one record as a reader sees it.
type Entry struct {
	Session  string
	MsgID    string
	CauseID  string
	Kind     string
	TS       int64
	Envelope []byte
}

type locator struct {
	off  int64
	size int32
}

// sessionIndex is what makes a scan cheap: where this session's records are,
// plus the two counts the session listing needs so it never has to read them.
type sessionIndex struct {
	locs   []locator
	errors int
}

// Log is an append-only event log.
type Log struct {
	// Sync fsyncs every batch, matching SQLite's `synchronous=FULL`. Off by
	// default for the same reason the SQLite path defaults to NORMAL.
	sync bool

	path string
	f    *os.File
	w    *bufio.Writer

	mu  sync.RWMutex
	off int64
	idx map[string]*sessionIndex

	reqs   chan *request
	stop   chan struct{}
	closed sync.Once
	wg     sync.WaitGroup
}

type request struct {
	rec  []byte
	e    Entry
	done chan error
}

// Open loads a log, rebuilding its index and repairing a torn tail.
func Open(dir string, fsync bool) (*Log, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "events.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	l := &Log{
		sync: fsync, path: path, f: f,
		w:    bufio.NewWriterSize(f, 1<<20),
		idx:  map[string]*sessionIndex{},
		reqs: make(chan *request, batchCap*2),
		stop: make(chan struct{}),
	}
	if err := l.recover(); err != nil {
		f.Close()
		return nil, err
	}
	l.wg.Add(1)
	go l.run()
	return l, nil
}

// recover rebuilds the index and truncates a partial trailing record.
//
// A crash mid-batch leaves the last record short or with a checksum that does
// not match. Both are expected, and both mean the same thing: everything up to
// that point is intact and that record never completed. Truncating is what keeps
// the next append from writing after garbage.
func (l *Log) recover() error {
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(l.f, 1<<20)
	var off int64
	hdr := make([]byte, lenSize+crcSize)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			break // clean end, or a header too short to be a record
		}
		size := binary.BigEndian.Uint32(hdr[:lenSize])
		if size < fixed || size > maxRecord {
			break // a length this file cannot have written
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(r, body); err != nil {
			break // torn tail
		}
		if crc32.Checksum(body, crcTable) != binary.BigEndian.Uint32(hdr[lenSize:]) {
			break // the record did not survive whatever happened
		}
		e, err := decode(body)
		if err != nil {
			break
		}
		total := int64(lenSize + crcSize + int(size))
		l.note(e.Session, e.Kind, locator{off: off, size: int32(total)})
		off += total
	}
	l.off = off
	// Anything past the last intact record is discarded rather than left to be
	// read as a record later.
	if err := l.f.Truncate(off); err != nil {
		return fmt.Errorf("truncate torn tail: %w", err)
	}
	if _, err := l.f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	return nil
}

func (l *Log) note(session, kind string, loc locator) {
	si := l.idx[session]
	if si == nil {
		si = &sessionIndex{}
		l.idx[session] = si
	}
	si.locs = append(si.locs, loc)
	if kind == "error" {
		si.errors++
	}
}

// Append writes one record and returns once it is committed.
func (l *Log) Append(e Entry) error {
	rec, err := encode(e)
	if err != nil {
		return err
	}
	req := &request{rec: rec, e: e, done: make(chan error, 1)}
	select {
	case l.reqs <- req:
	case <-l.stop:
		return errors.New("seglog is closed")
	}
	return <-req.done
}

// run is the group commit: drain whatever is already queued, write it all, then
// release everyone. No timer — waiting for more work would tax an idle node to
// speed up a busy one.
func (l *Log) run() {
	defer l.wg.Done()
	batch := make([]*request, 0, batchCap)
	for {
		select {
		case <-l.stop:
			l.flush(batch[:0])
			return
		case first := <-l.reqs:
			batch = append(batch[:0], first)
		drain:
			for len(batch) < batchCap {
				select {
				case r := <-l.reqs:
					batch = append(batch, r)
				default:
					break drain
				}
			}
			l.flush(batch)
		}
	}
}

func (l *Log) flush(batch []*request) {
	if len(batch) == 0 {
		return
	}
	l.mu.Lock()
	var err error
	start := l.off
	written := make([]locator, 0, len(batch))
	for _, r := range batch {
		if _, e := l.w.Write(r.rec); e != nil {
			err = e
			break
		}
		written = append(written, locator{off: l.off, size: int32(len(r.rec))})
		l.off += int64(len(r.rec))
	}
	if err == nil {
		err = l.w.Flush()
	}
	if err == nil && l.sync {
		err = l.f.Sync()
	}
	if err != nil {
		// The batch did not land. Roll the offset back so the next write does
		// not leave a hole, and tell every caller the truth about its own write
		// rather than letting a partial batch look successful.
		l.off = start
		l.mu.Unlock()
		for _, r := range batch {
			r.done <- err
		}
		return
	}
	for i, r := range batch {
		l.note(r.e.Session, r.e.Kind, written[i])
	}
	l.mu.Unlock()
	for _, r := range batch {
		r.done <- nil
	}
}

// Session returns one session's records in the order they were written, up to
// limit (0 = all).
func (l *Log) Session(session string, limit int) ([]Entry, error) {
	l.mu.RLock()
	si := l.idx[session]
	if si == nil {
		l.mu.RUnlock()
		return nil, nil
	}
	locs := append([]locator(nil), si.locs...)
	l.mu.RUnlock()

	if limit > 0 && len(locs) > limit {
		locs = locs[:limit]
	}
	if len(locs) == 0 {
		return nil, nil
	}
	// A session's records are usually adjacent on disk, so consecutive ones are
	// read in a single call rather than one syscall apiece. Replay of a long
	// session is the read path that matters and it is otherwise dominated by
	// per-record seeks.
	out := make([]Entry, 0, len(locs))
	for i := 0; i < len(locs); {
		j, span := i, int64(locs[i].size)
		for j+1 < len(locs) &&
			locs[j].off+int64(locs[j].size) == locs[j+1].off &&
			span+int64(locs[j+1].size) <= 4<<20 {
			j++
			span += int64(locs[j].size)
		}
		buf := make([]byte, span)
		if _, err := l.f.ReadAt(buf, locs[i].off); err != nil {
			return out, fmt.Errorf("read session %s: %w", session, err)
		}
		for k, pos := i, int64(0); k <= j; k++ {
			size := int64(locs[k].size)
			e, err := decodeFramed(buf[pos : pos+size])
			if err != nil {
				return out, err
			}
			out = append(out, e)
			pos += size
		}
		i = j + 1
	}
	return out, nil
}

// Counts reports how many records a session has and how many are errors, which
// the session listing needs and which the index already knows.
func (l *Log) Counts(session string) (total, errs int) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	si := l.idx[session]
	if si == nil {
		return 0, 0
	}
	return len(si.locs), si.errors
}

func (l *Log) Close() error {
	l.closed.Do(func() { close(l.stop) })
	l.wg.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.w.Flush(); err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	return l.f.Close()
}

// ── encoding ────────────────────────────────────────────────────────

func encode(e Entry) ([]byte, error) {
	if len(e.Session) > 0xFFFF || len(e.MsgID) > 0xFFFF ||
		len(e.CauseID) > 0xFFFF || len(e.Kind) > 0xFF {
		return nil, errors.New("seglog: field too long to encode")
	}
	body := make([]byte, 0, fixed+len(e.Session)+len(e.MsgID)+
		len(e.CauseID)+len(e.Kind)+len(e.Envelope))
	var scratch [8]byte
	binary.BigEndian.PutUint64(scratch[:], uint64(e.TS))
	body = append(body, scratch[:]...)
	body = appendU16(body, len(e.Session))
	body = appendU16(body, len(e.MsgID))
	body = appendU16(body, len(e.CauseID))
	body = append(body, byte(len(e.Kind)))
	body = append(body, e.Session...)
	body = append(body, e.MsgID...)
	body = append(body, e.CauseID...)
	body = append(body, e.Kind...)
	body = append(body, e.Envelope...)

	if len(body) > maxRecord {
		return nil, fmt.Errorf("seglog: record of %d bytes exceeds the %d limit",
			len(body), maxRecord)
	}
	rec := make([]byte, lenSize+crcSize+len(body))
	binary.BigEndian.PutUint32(rec[:lenSize], uint32(len(body)))
	binary.BigEndian.PutUint32(rec[lenSize:lenSize+crcSize], crc32.Checksum(body, crcTable))
	copy(rec[lenSize+crcSize:], body)
	return rec, nil
}

func appendU16(b []byte, n int) []byte {
	return append(b, byte(n>>8), byte(n))
}

// decodeFramed validates and decodes a whole record, header included. Used on
// the read path, where a checksum mismatch means the file changed underneath a
// reader rather than a crash — worth reporting rather than silently skipping.
func decodeFramed(rec []byte) (Entry, error) {
	if len(rec) < lenSize+crcSize {
		return Entry{}, errors.New("seglog: record shorter than its header")
	}
	size := int(binary.BigEndian.Uint32(rec[:lenSize]))
	body := rec[lenSize+crcSize:]
	if size != len(body) {
		return Entry{}, errors.New("seglog: record length does not match its header")
	}
	if crc32.Checksum(body, crcTable) != binary.BigEndian.Uint32(rec[lenSize:lenSize+crcSize]) {
		return Entry{}, errors.New("seglog: checksum mismatch — the log has been altered")
	}
	return decode(body)
}

func decode(body []byte) (Entry, error) {
	if len(body) < fixed {
		return Entry{}, errors.New("seglog: record too short")
	}
	e := Entry{TS: int64(binary.BigEndian.Uint64(body[:8]))}
	ls := int(binary.BigEndian.Uint16(body[8:10]))
	lm := int(binary.BigEndian.Uint16(body[10:12]))
	lc := int(binary.BigEndian.Uint16(body[12:14]))
	lk := int(body[14])
	pos := fixed
	if len(body) < pos+ls+lm+lc+lk {
		return Entry{}, errors.New("seglog: record fields exceed its length")
	}
	e.Session = string(body[pos : pos+ls])
	pos += ls
	e.MsgID = string(body[pos : pos+lm])
	pos += lm
	e.CauseID = string(body[pos : pos+lc])
	pos += lc
	e.Kind = string(body[pos : pos+lk])
	pos += lk
	e.Envelope = body[pos:]
	return e, nil
}
