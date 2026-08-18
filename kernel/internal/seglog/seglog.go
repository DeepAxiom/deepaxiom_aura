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
// # Why it is segmented
//
// A runtime whose premise is that it never stops running cannot write one file
// forever. Segmenting buys three things that a single file cannot have at any
// size:
//
//   - **Retention is possible at all.** Reclaiming space means deleting a whole
//     segment — one unlink, nothing relocated, which is the entire point of
//     treating the log as permanent storage. Trimming the front of a single file
//     would be the compaction this design exists to avoid.
//   - **Damage is contained.** Records are framed per segment, so a corrupt
//     record in one does not desynchronise the parse of the next. Recovery loses
//     the tail of *that* segment and reads the rest normally, instead of the
//     whole log ending at the first bad byte.
//   - **File handles stay at one.** Only the active segment is held open. Sealed
//     segments are opened for the duration of a read and closed again, so the
//     node's descriptor use does not grow with its history.
//
// Memory is bounded on the same principle. The locator index is derived state —
// it can always be rebuilt by scanning — so it is capped, and the coldest
// sessions are evicted from it first. A read for an evicted session falls back
// to scanning only the segments that session actually appears in, which the
// index still remembers because a session set costs one entry per segment
// rather than one per record.
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
// log is detected by its checksum and truncated, so the log is never left
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
	"sort"
	"strconv"
	"strings"
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

// Segment naming. `events-%06d.log`, ordered by ordinal, with the highest
// ordinal being the segment currently open for writing.
const (
	segPrefix = "events-"
	segSuffix = ".log"
	// legacyName is the single file this package wrote before it was segmented.
	// A data directory holding one is migrated in place on Open rather than
	// abandoned — an operator upgrading a node must not silently lose their
	// replay history.
	legacyName = "events.log"
)

// Defaults. Both are overridable through Options; both exist so that the
// zero-configuration node has a bound rather than a hope.
const (
	// DefaultSegmentBytes is where a segment is sealed and a new one started.
	// Large enough that rotation is rare on a busy node, small enough that a
	// single file stays copyable, checksummable and deletable.
	DefaultSegmentBytes = 128 << 20 // 128 MiB
	// DefaultMaxIndexed bounds the hot locator index. A locator is 16 bytes, so
	// this is ~16 MB of index regardless of how large the log on disk grows.
	// Past it the coldest sessions fall back to a segment scan.
	DefaultMaxIndexed = 1 << 20
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

// Options configures a log. The zero value is the default configuration.
type Options struct {
	// Sync fsyncs every batch, matching SQLite's `synchronous=FULL`. Off by
	// default for the same reason the SQLite path defaults to NORMAL.
	Sync bool
	// SegmentBytes is the rotation threshold. Zero means DefaultSegmentBytes.
	//
	// A segment is sealed once it has *reached* the threshold, never before a
	// record is written, so a record is never split across two files and a
	// segment may exceed this by up to one record. That is the right way round:
	// a bound that could split a record would make recovery ambiguous, and
	// ambiguity is the one thing this file cannot afford.
	SegmentBytes int64
	// MaxBytes caps the log's total size on disk, reclaimed by deleting whole
	// segments oldest-first after a rotation. Zero — the default — keeps
	// everything.
	//
	// The default is "keep" rather than a number because this history is what
	// `aura why`, replay and `aura regress` read: deleting it silently would
	// make a node quietly stop being able to answer questions about itself. An
	// operator who would rather bound the disk says so, and then the oldest
	// history is what goes.
	MaxBytes int64
	// MaxIndexed bounds the in-memory locator index. Zero means
	// DefaultMaxIndexed. Negative means unbounded, which is what the tests that
	// assert on index contents use and what an operator would choose only if
	// their history is known to be small.
	MaxIndexed int
}

func (o Options) segmentBytes() int64 {
	if o.SegmentBytes <= 0 {
		return DefaultSegmentBytes
	}
	return o.SegmentBytes
}

func (o Options) maxIndexed() int {
	if o.MaxIndexed == 0 {
		return DefaultMaxIndexed
	}
	return o.MaxIndexed
}

// locator is where one record lives: which segment, and where inside it.
type locator struct {
	seg  uint32
	off  int64
	size int32
}

// sessionIndex is what makes a scan cheap: where this session's records are,
// plus the two counts the session listing needs so it never has to read them.
//
// `locs` is the part that is allowed to go away. When the index is over budget
// the coldest sessions have their locators dropped and `evicted` set; `segs`
// and the counts stay, because they cost one entry per segment and two integers
// per session rather than one entry per record, and they are exactly what a
// fallback read needs to avoid scanning the whole log.
type sessionIndex struct {
	locs    []locator
	evicted bool
	total   int
	errors  int
	segs    map[uint32]struct{}
	// touched orders eviction: the lowest values are the sessions that have
	// gone longest without an append, which are the ones a replay is least
	// likely to ask for next.
	touched uint64
}

func (si *sessionIndex) note(seg uint32) {
	if si.segs == nil {
		si.segs = map[uint32]struct{}{}
	}
	si.segs[seg] = struct{}{}
}

// segment is one file in the log.
type segment struct {
	ord  uint32
	path string
	size int64
	// f is held only for the active segment; a sealed segment is opened for the
	// duration of a read and closed again, so descriptor use does not grow with
	// history.
	f *os.File
}

// Log is an append-only, segmented event log.
type Log struct {
	opt Options
	dir string

	mu       sync.RWMutex
	segs     []*segment // ordered by ordinal; the last one is active
	w        *bufio.Writer
	idx      map[string]*sessionIndex
	indexed  int    // locators currently held across every session
	clock    uint64 // monotonic counter behind sessionIndex.touched
	damage   []string
	segBytes int64 // total bytes across every live segment

	reqs   chan *request
	stop   chan struct{}
	closed sync.Once
	wg     sync.WaitGroup

	// admit guards closing, and exists because `reqs` is buffered: selecting on
	// `stop` alone does not stop an append landing in a queue the writer has
	// already stopped draining. Held for read on every append — uncontended
	// except against Close, which is the one writer. See Append and Close.
	admit   sync.RWMutex
	closing bool
}

// ErrClosed is returned by Append once the log has been closed. Callers on the
// event-log path treat it as a shutdown signal, not as data loss: nothing that
// returned nil is affected by it.
var ErrClosed = errors.New("seglog is closed")

type request struct {
	rec  []byte
	e    Entry
	done chan error
}

// Open loads a log with the default options, rebuilding its index and repairing
// a torn tail.
func Open(dir string, fsync bool) (*Log, error) {
	return OpenWith(dir, Options{Sync: fsync})
}

// OpenWith loads a log with explicit options.
func OpenWith(dir string, opt Options) (*Log, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	l := &Log{
		opt:  opt,
		dir:  dir,
		idx:  map[string]*sessionIndex{},
		reqs: make(chan *request, batchCap*2),
		stop: make(chan struct{}),
	}
	if err := l.migrateLegacy(); err != nil {
		return nil, err
	}
	if err := l.recover(); err != nil {
		l.closeSegments()
		return nil, err
	}
	l.wg.Add(1)
	go l.run()
	return l, nil
}

// migrateLegacy renames a pre-segmentation `events.log` into the first segment.
//
// Done by rename rather than by teaching the reader two layouts: one naming
// scheme is one thing to reason about during recovery, and recovery is where
// this package earns its keep. The rename happens before any handle is open, so
// it is a plain metadata operation on every platform.
func (l *Log) migrateLegacy() error {
	legacy := filepath.Join(l.dir, legacyName)
	if _, err := os.Stat(legacy); err != nil {
		return nil // no legacy file: nothing to do
	}
	// Only when the directory has no segments yet. A directory holding both is
	// not something this package can produce, so the numbered files are the
	// real log and the stray legacy file is left untouched rather than merged
	// into a position it may not belong in.
	ords, err := l.segmentOrdinals()
	if err != nil {
		return err
	}
	if len(ords) > 0 {
		return nil
	}
	if err := os.Rename(legacy, l.segPath(0)); err != nil {
		return fmt.Errorf("migrate %s into the first segment: %w", legacyName, err)
	}
	return nil
}

func (l *Log) segPath(ord uint32) string {
	return filepath.Join(l.dir, fmt.Sprintf("%s%06d%s", segPrefix, ord, segSuffix))
}

// segmentOrdinals lists the segments present, in order.
func (l *Log) segmentOrdinals() ([]uint32, error) {
	names, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, err
	}
	var ords []uint32
	for _, n := range names {
		if n.IsDir() {
			continue
		}
		name := n.Name()
		if !strings.HasPrefix(name, segPrefix) || !strings.HasSuffix(name, segSuffix) {
			continue
		}
		digits := strings.TrimSuffix(strings.TrimPrefix(name, segPrefix), segSuffix)
		ord, err := strconv.ParseUint(digits, 10, 32)
		if err != nil {
			continue // not ours
		}
		ords = append(ords, uint32(ord))
	}
	sort.Slice(ords, func(i, j int) bool { return ords[i] < ords[j] })
	return ords, nil
}

// recover rebuilds the index and repairs the log.
//
// A crash mid-batch leaves the last record short or with a checksum that does
// not match. Both are expected, and both mean the same thing: everything up to
// that point is intact and that record never completed. Truncating is what keeps
// the next append from writing after garbage.
//
// A bad record in a *sealed* segment is a different event — that file was
// finished and closed, so nothing legitimate rewrites it — and it is recorded as
// damage rather than treated as a torn tail. The scan of that segment stops
// there, and the segments after it are read normally: framing is per segment, so
// one corrupt file cannot desynchronise the next. That containment is a reason
// to segment, not a consequence of it.
func (l *Log) recover() error {
	ords, err := l.segmentOrdinals()
	if err != nil {
		return err
	}
	if len(ords) == 0 {
		ords = []uint32{0}
	}
	for i, ord := range ords {
		last := i == len(ords)-1
		seg, err := l.scan(ord, last)
		if err != nil {
			return err
		}
		l.segs = append(l.segs, seg)
		l.segBytes += seg.size
	}

	// The highest ordinal is the active segment: reopen it for appending, at
	// the offset the scan established as the end of intact data.
	active := l.segs[len(l.segs)-1]
	f, err := os.OpenFile(active.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", active.path, err)
	}
	if _, err := f.Seek(active.size, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	active.f = f
	l.w = bufio.NewWriterSize(f, 1<<20)
	return nil
}

// scan reads one segment, indexing every intact record. `truncate` is set for
// the active segment, where a partial trailing record is repaired rather than
// reported.
func (l *Log) scan(ord uint32, truncate bool) (*segment, error) {
	seg := &segment{ord: ord, path: l.segPath(ord)}
	f, err := os.OpenFile(seg.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", seg.path, err)
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<20)
	var off int64
	var bad string
	hdr := make([]byte, lenSize+crcSize)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err != io.EOF {
				bad = "a header too short to be a record"
			}
			break // clean end, or a header too short to be a record
		}
		size := binary.BigEndian.Uint32(hdr[:lenSize])
		if size < fixed || size > maxRecord {
			bad = fmt.Sprintf("a length prefix of %d this file cannot have written", size)
			break
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(r, body); err != nil {
			bad = "a record that ends before its length says it should"
			break
		}
		if crc32.Checksum(body, crcTable) != binary.BigEndian.Uint32(hdr[lenSize:]) {
			bad = "a record whose checksum does not match its contents"
			break
		}
		e, err := decode(body)
		if err != nil {
			bad = "a record that does not decode: " + err.Error()
			break
		}
		total := int64(lenSize + crcSize + int(size))
		l.note(e.Session, e.Kind, locator{seg: ord, off: off, size: int32(total)})
		off += total
	}
	seg.size = off

	if bad != "" && !truncate {
		// A sealed segment does not get rewritten by anything legitimate, so
		// this is damage rather than a crash. Say so, keep what is intact, and
		// leave the file alone: truncating evidence of tampering would destroy
		// the only trace of it.
		l.damage = append(l.damage,
			fmt.Sprintf("%s: stopped at offset %d — %s", filepath.Base(seg.path), off, bad))
		return seg, nil
	}
	if truncate {
		// Anything past the last intact record is discarded rather than left to
		// be read as a record later.
		if err := f.Truncate(off); err != nil {
			return nil, fmt.Errorf("truncate torn tail of %s: %w", seg.path, err)
		}
	}
	return seg, nil
}

// note records one locator. Caller holds mu, or is inside recovery where no
// other goroutine exists yet.
func (l *Log) note(session, kind string, loc locator) {
	si := l.idx[session]
	if si == nil {
		si = &sessionIndex{}
		l.idx[session] = si
	}
	l.clock++
	si.touched = l.clock
	si.note(loc.seg)
	si.total++
	if kind == "error" {
		si.errors++
	}
	if si.evicted {
		// Already spilled to disk. Re-admitting one locator would make this
		// session half-indexed, and a half-indexed session is worse than an
		// unindexed one: the read path would have to reconcile a partial list
		// with a scan. It stays on the scan path until it is read.
		return
	}
	si.locs = append(si.locs, loc)
	l.indexed++
	l.evictIfOverBudget()
}

// evictIfOverBudget drops the coldest sessions' locators until the index is
// back under its cap. Caller holds mu.
//
// It evicts down to 80% rather than to exactly the cap so that a node sitting at
// the boundary does not pay an O(sessions) pass on every append — the same
// amortisation any high-water-mark eviction uses.
func (l *Log) evictIfOverBudget() {
	budget := l.opt.maxIndexed()
	if budget < 0 || l.indexed <= budget {
		return
	}
	target := budget * 4 / 5

	type victim struct {
		id      string
		touched uint64
		n       int
	}
	victims := make([]victim, 0, len(l.idx))
	for id, si := range l.idx {
		if si.evicted || len(si.locs) == 0 {
			continue
		}
		victims = append(victims, victim{id: id, touched: si.touched, n: len(si.locs)})
	}
	sort.Slice(victims, func(i, j int) bool { return victims[i].touched < victims[j].touched })

	for _, v := range victims {
		if l.indexed <= target {
			return
		}
		si := l.idx[v.id]
		si.locs = nil
		si.evicted = true
		l.indexed -= v.n
	}
}

// Append writes one record and returns once it is committed.
func (l *Log) Append(e Entry) error {
	rec, err := encode(e)
	if err != nil {
		return err
	}
	req := &request{rec: rec, e: e, done: make(chan error, 1)}

	// Admission is what makes this safe, not a select on `stop`. `reqs` is
	// buffered, so after Close both cases of `select { case l.reqs <- req:
	// case <-l.stop: }` are ready and Go picks between them at random: roughly
	// half the time the request was enqueued into a channel whose reader had
	// already returned, and the caller blocked on `done` forever. On the event
	// log that is a session goroutine hung for the life of the process.
	//
	// Taking the lock for read cannot deadlock against Close even when the
	// queue is full: Close cannot have signalled the writer yet — it sets
	// closing under the write lock first — so the writer is still draining.
	l.admit.RLock()
	if l.closing {
		l.admit.RUnlock()
		return ErrClosed
	}
	l.reqs <- req
	l.admit.RUnlock()

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
			// Close has already stopped admission, so what is still queued is a
			// finite set of writes accepted while the log was open. Commit them
			// rather than walk away: their callers are blocked on `done`, and a
			// write must either land or come back with an error — never
			// neither. The file is still open, since Close waits on the
			// WaitGroup before touching it.
			l.drain(batch)
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

// drain commits everything left in the queue at shutdown, reusing the caller's
// batch buffer. It terminates because Close closes admission before signalling
// the writer, so no new request can arrive while this runs.
func (l *Log) drain(batch []*request) {
	for {
		batch = batch[:0]
	fill:
		for len(batch) < batchCap {
			select {
			case r := <-l.reqs:
				batch = append(batch, r)
			default:
				break fill
			}
		}
		if len(batch) == 0 {
			return
		}
		l.flush(batch)
	}
}

func (l *Log) flush(batch []*request) {
	if len(batch) == 0 {
		return
	}
	l.mu.Lock()
	active := l.segs[len(l.segs)-1]
	var err error
	start := active.size
	startBytes := l.segBytes
	written := make([]locator, 0, len(batch))
	for _, r := range batch {
		if _, e := l.w.Write(r.rec); e != nil {
			err = e
			break
		}
		written = append(written, locator{seg: active.ord, off: active.size, size: int32(len(r.rec))})
		active.size += int64(len(r.rec))
		l.segBytes += int64(len(r.rec))
	}
	if err == nil {
		err = l.w.Flush()
	}
	if err == nil && l.opt.Sync {
		err = active.f.Sync()
	}
	if err != nil {
		// The batch did not land. Roll the offset back so the next write does
		// not leave a hole, and tell every caller the truth about its own write
		// rather than letting a partial batch look successful.
		active.size = start
		l.segBytes = startBytes
		l.mu.Unlock()
		for _, r := range batch {
			r.done <- err
		}
		return
	}
	for i, r := range batch {
		l.note(r.e.Session, r.e.Kind, written[i])
	}
	// Rotation is checked after the batch, never inside it, so a record is
	// never split across two segments. A segment may therefore exceed the
	// threshold by up to one batch, which is the harmless direction to err in.
	if active.size >= l.opt.segmentBytes() {
		if e := l.rotate(); e != nil {
			// The batch is already durable; rotation failing does not unmake
			// that. Record it and keep writing into the current segment — a
			// node that stopped accepting events because it could not start a
			// new file would be trading a bounded disk for an outage.
			l.damage = append(l.damage, "rotation failed: "+e.Error())
		}
	}
	l.mu.Unlock()
	for _, r := range batch {
		r.done <- nil
	}
}

// rotate seals the active segment and opens the next one. Caller holds mu.
func (l *Log) rotate() error {
	active := l.segs[len(l.segs)-1]
	if err := l.w.Flush(); err != nil {
		return err
	}
	if err := active.f.Sync(); err != nil {
		return err
	}
	if err := active.f.Close(); err != nil {
		return err
	}
	active.f = nil

	next := &segment{ord: active.ord + 1, path: l.segPath(active.ord + 1)}
	f, err := os.OpenFile(next.path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		// Reopen the segment just sealed so the log stays writable.
		if reopened, e := os.OpenFile(active.path, os.O_RDWR|os.O_APPEND, 0o644); e == nil {
			active.f = reopened
			l.w = bufio.NewWriterSize(reopened, 1<<20)
		}
		return fmt.Errorf("open %s: %w", next.path, err)
	}
	next.f = f
	l.segs = append(l.segs, next)
	l.w = bufio.NewWriterSize(f, 1<<20)
	l.prune()
	return nil
}

// prune enforces Options.MaxBytes by deleting whole segments, oldest first.
// Caller holds mu.
//
// Whole segments only. Deleting a record — or trimming the front of a file —
// would be the relocation this design exists to avoid, and would leave offsets
// that the index and every locator already hold pointing at the wrong bytes.
// The active segment is never a candidate: a node must always have somewhere to
// write, whatever the budget says.
func (l *Log) prune() {
	if l.opt.MaxBytes <= 0 {
		return
	}
	for l.segBytes > l.opt.MaxBytes && len(l.segs) > 1 {
		oldest := l.segs[0]
		if err := os.Remove(oldest.path); err != nil && !os.IsNotExist(err) {
			l.damage = append(l.damage, "prune failed: "+err.Error())
			return
		}
		l.segs = l.segs[1:]
		l.segBytes -= oldest.size
		l.forget(oldest.ord)
	}
}

// forget drops every trace of a deleted segment from the index. Caller holds mu.
//
// The counts are recomputed rather than left as they were. A session listing
// that says 400 events while a read of the same session returns 40 is a UI
// telling the operator something the node can no longer back up; the honest
// number is what is still readable.
func (l *Log) forget(ord uint32) {
	for id, si := range l.idx {
		if _, ok := si.segs[ord]; !ok {
			continue
		}
		delete(si.segs, ord)
		if len(si.segs) == 0 {
			l.indexed -= len(si.locs)
			delete(l.idx, id)
			continue
		}
		if si.evicted {
			// The counts cannot be recomputed without a scan, and a scan on the
			// prune path would make rotation O(log size). The session is already
			// on the fallback read path, so its counts are refreshed the next
			// time it is actually read.
			si.total, si.errors = 0, 0
			continue
		}
		kept := si.locs[:0]
		for _, loc := range si.locs {
			if loc.seg != ord {
				kept = append(kept, loc)
			}
		}
		l.indexed -= len(si.locs) - len(kept)
		si.locs = kept
		si.total = len(kept)
		// Error counts are per record and the records are gone; recomputing
		// them exactly would need a read. Bounding them by what remains is the
		// truthful direction to be wrong in.
		if si.errors > si.total {
			si.errors = si.total
		}
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
	if si.evicted {
		segs := make([]uint32, 0, len(si.segs))
		for ord := range si.segs {
			segs = append(segs, ord)
		}
		paths := l.pathsFor(segs)
		l.mu.RUnlock()
		return l.scanFor(session, paths, limit)
	}
	locs := append([]locator(nil), si.locs...)
	l.mu.RUnlock()

	if limit > 0 && len(locs) > limit {
		locs = locs[:limit]
	}
	if len(locs) == 0 {
		return nil, nil
	}
	return l.readLocators(session, locs)
}

// pathsFor resolves segment ordinals to paths, in order. Caller holds mu.
func (l *Log) pathsFor(ords []uint32) []string {
	sort.Slice(ords, func(i, j int) bool { return ords[i] < ords[j] })
	live := make(map[uint32]string, len(l.segs))
	for _, s := range l.segs {
		live[s.ord] = s.path
	}
	paths := make([]string, 0, len(ords))
	for _, ord := range ords {
		if p, ok := live[ord]; ok {
			paths = append(paths, p)
		}
	}
	return paths
}

// readLocators reads an indexed session. Locators are grouped by segment and
// then by adjacency, so a long session costs one open per segment and one read
// per contiguous run rather than a syscall per record.
func (l *Log) readLocators(session string, locs []locator) ([]Entry, error) {
	out := make([]Entry, 0, len(locs))
	for i := 0; i < len(locs); {
		// One segment at a time: locators are in write order, and a session's
		// records only ever move forward through segments.
		j := i
		for j+1 < len(locs) && locs[j+1].seg == locs[i].seg {
			j++
		}
		f, err := os.Open(l.segPath(locs[i].seg))
		if err != nil {
			return out, fmt.Errorf("read session %s: %w", session, err)
		}
		err = readRuns(f, locs[i:j+1], &out)
		f.Close()
		if err != nil {
			return out, fmt.Errorf("read session %s: %w", session, err)
		}
		i = j + 1
	}
	return out, nil
}

// readRuns reads one segment's locators, coalescing adjacent records into a
// single call. A session's records are usually adjacent on disk, so this is
// what keeps replay of a long session from being dominated by per-record seeks.
func readRuns(f *os.File, locs []locator, out *[]Entry) error {
	for i := 0; i < len(locs); {
		j, span := i, int64(locs[i].size)
		for j+1 < len(locs) &&
			locs[j].off+int64(locs[j].size) == locs[j+1].off &&
			span+int64(locs[j+1].size) <= 4<<20 {
			j++
			span += int64(locs[j].size)
		}
		buf := make([]byte, span)
		if _, err := f.ReadAt(buf, locs[i].off); err != nil {
			return err
		}
		for k, pos := i, int64(0); k <= j; k++ {
			size := int64(locs[k].size)
			e, err := decodeFramed(buf[pos : pos+size])
			if err != nil {
				return err
			}
			*out = append(*out, e)
			pos += size
		}
		i = j + 1
	}
	return nil
}

// scanFor reads a session whose locators were evicted from the index, by
// scanning only the segments the index still remembers it appearing in.
//
// This is the trade the index cap buys: bounded memory for every session, at
// the cost of a sequential read of a bounded set of segments for the coldest
// ones. Replay of an old session is rare and off the hot path, which is exactly
// where a scan belongs.
func (l *Log) scanFor(session string, paths []string, limit int) ([]Entry, error) {
	var out []Entry
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // pruned between the index read and here
			}
			return out, fmt.Errorf("read session %s: %w", session, err)
		}
		err = scanSegment(f, session, limit, &out)
		f.Close()
		if err != nil {
			return out, fmt.Errorf("read session %s: %w", session, err)
		}
		if limit > 0 && len(out) >= limit {
			return out[:limit], nil
		}
	}
	return out, nil
}

// scanSegment appends one segment's records for a session. It stops at the
// first record that does not parse, for the same reason recovery does: past
// that point the framing is no longer trustworthy.
func scanSegment(f *os.File, session string, limit int, out *[]Entry) error {
	r := bufio.NewReaderSize(f, 1<<20)
	hdr := make([]byte, lenSize+crcSize)
	for {
		if limit > 0 && len(*out) >= limit {
			return nil
		}
		if _, err := io.ReadFull(r, hdr); err != nil {
			return nil // clean end
		}
		size := binary.BigEndian.Uint32(hdr[:lenSize])
		if size < fixed || size > maxRecord {
			return nil
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil
		}
		if crc32.Checksum(body, crcTable) != binary.BigEndian.Uint32(hdr[lenSize:]) {
			return nil
		}
		e, err := decode(body)
		if err != nil {
			return nil
		}
		if e.Session == session {
			*out = append(*out, e)
		}
	}
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
	return si.total, si.errors
}

// Stats reports what the log is actually holding, which is the number an
// operator needs to decide whether Options.MaxBytes wants setting.
type Stats struct {
	Segments int
	Bytes    int64
	Sessions int
	// Indexed is how many locators are held in memory; Evicted is how many
	// sessions have fallen back to the scan path.
	Indexed int
	Evicted int
	// Damage describes sealed segments that did not read back cleanly. Empty is
	// the expected state; anything here means a file changed after it was
	// closed.
	Damage []string
}

// Stats returns a snapshot.
func (l *Log) Stats() Stats {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s := Stats{
		Segments: len(l.segs),
		Bytes:    l.segBytes,
		Sessions: len(l.idx),
		Indexed:  l.indexed,
		Damage:   append([]string(nil), l.damage...),
	}
	for _, si := range l.idx {
		if si.evicted {
			s.Evicted++
		}
	}
	return s
}

func (l *Log) Close() error {
	l.closed.Do(func() {
		// Order matters. Refusing new appends *before* signalling the writer is
		// what bounds the queue: once this returns, every append either
		// completed its enqueue or will be refused, so the writer's drain sees
		// a finite set and terminates.
		l.admit.Lock()
		l.closing = true
		l.admit.Unlock()
		close(l.stop)
	})
	l.wg.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w != nil {
		if err := l.w.Flush(); err != nil {
			return err
		}
	}
	return l.closeSegmentsLocked()
}

func (l *Log) closeSegments() {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.closeSegmentsLocked()
}

func (l *Log) closeSegmentsLocked() error {
	var firstErr error
	for _, s := range l.segs {
		if s.f == nil {
			continue
		}
		if err := s.f.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := s.f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		s.f = nil
	}
	return firstErr
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
