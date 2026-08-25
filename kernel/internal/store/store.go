// Package store is the kernel's embedded state: SQLite, zero external services.
//
// It holds ONLY kernel state (skills, graphs, sessions, causal event log).
// Application data never lives here — that is what projections are for.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no CGO, single-binary friendly

	"aura/kernel/internal/seglog"
)

type Store struct {
	db *sql.DB
	// evlog carries the causal event log. It is an append-only segment file
	// rather than a table because that is what the data is: written once per
	// envelope, never updated, never deleted, read back as one session in
	// order. Profiling put ~54% of a loaded node's CPU inside SQLite's file
	// I/O; on the same workload at matched durability this sustained 1.5M
	// events/sec against SQLite's 27k. See internal/seglog.
	//
	// The effect ledger deliberately stays in SQLite: a bug here loses replay
	// history, a bug there loses evidence.
	evlog *seglog.Log
	// w batches the two writes that sit on the delivery path — the causal
	// event log and the effect ledger — into shared transactions. See
	// groupcommit.go for why this costs no durability and no ordering.
	w *writer
}

// Open the embedded state. The pragma set is on the hot path, so it is worth
// saying why each one is there.
//
// `journal_mode=WAL` lets a reader (a replay, `aura why`, the UI polling the
// ledger) run while the executor keeps appending, which a rollback journal
// would serialise.
//
// `synchronous=NORMAL` is the one that matters for latency. AppendEvent is one
// INSERT per envelope — one implicit transaction — and it is called
// *synchronously on the delivery path* (see executor.Session.deliver), so with
// SQLite's default of FULL every streamed token pays an fsync before the next
// hop runs. Measured on one developer machine that is ~2.8 ms per envelope, a
// ceiling of ~356 envelopes/sec through the whole kernel; NORMAL puts it at
// ~0.05 ms and ~21k/sec. In WAL mode NORMAL is still crash-safe for the thing
// this log is for: a process that dies — panic, SIGKILL, a dropped session —
// loses nothing, because the WAL is already written. What NORMAL gives up is
// durability across a *power* loss, where the last transactions before the cut
// may be missing. The log stays internally consistent either way; it is never
// left torn or corrupt.
//
// That trade is deliberate and it is not free: the effect ledger's guarantee is
// that entries cannot be *altered*, which the hash chain enforces regardless of
// pragma — but an operator who needs the last effect before a power cut to have
// survived it wants FULL, and can say so. Hence the override rather than a
// hardcoded value.
//
// AURA_SQLITE_SYNCHRONOUS overrides it (FULL, NORMAL, OFF). OFF is faster still
// on paper and is not worth it: batching the appends buys the same win without
// risking a corrupt database.
func Open(dataDir string) (*Store, error) { return OpenWith(dataDir, Options{}) }

// Options tunes the parts of the store an operator may reasonably want to bound.
// The zero value is the default configuration, which is what Open uses.
type Options struct {
	// EventLogMaxBytes caps the causal event log's total size on disk, reclaimed
	// by deleting whole segments oldest-first. Zero keeps everything.
	//
	// It is a store option rather than a seglog constant because it is a
	// question only the operator can answer: how much replay history is this
	// node's disk worth. See seglog.Options.MaxBytes for what "oldest-first"
	// costs — `aura why` and `aura replay` stop being able to answer about
	// sessions whose segments were reclaimed.
	EventLogMaxBytes int64
	// EventLogSegmentBytes is the rotation threshold. Zero means the seglog
	// default. Exposed mainly so tests can rotate without writing 128 MiB.
	EventLogSegmentBytes int64
}

// OpenWith opens the embedded state with explicit options.
func OpenWith(dataDir string, opt Options) (*Store, error) {
	sync := os.Getenv("AURA_SQLITE_SYNCHRONOUS")
	switch strings.ToUpper(sync) {
	case "FULL", "NORMAL", "OFF", "EXTRA":
		sync = strings.ToUpper(sync)
	default:
		sync = "NORMAL"
	}
	// wal_autocheckpoint is the other half of the fsync story, and profiling is
	// what found it. With synchronous=NORMAL a commit does not fsync — but a WAL
	// *checkpoint* does, and SQLite checkpoints every 1000 pages by default. On a
	// node under load the WAL refills constantly, so the node was spending 38% of
	// all CPU in FlushFileBuffers and another 25% in the reads and writes that
	// copy the WAL back into the main database. Raising the threshold makes
	// checkpoints rarer and larger.
	//
	// It costs disk, not durability: WAL content is written either way, and
	// checkpointing only moves it into the main file. A bigger WAL means a longer
	// recovery scan after an unclean shutdown, which is the actual trade.
	checkpoint := os.Getenv("AURA_SQLITE_WAL_AUTOCHECKPOINT")
	if _, err := strconv.Atoi(checkpoint); err != nil {
		checkpoint = "20000"
	}
	// The store creates its own directory rather than assuming a caller did.
	// `aura up` happens to load the node identity first, which creates it as a
	// side effect — so every command that opens a store *without* doing that
	// (issuing a token, reading a ledger) failed on a fresh data directory with
	// SQLite's "out of memory", which is what it reports for a path it cannot
	// open. A component that needs a directory makes it.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dataDir, err)
	}
	dsn := filepath.Join(dataDir, "kernel.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(" + sync + ")" +
		"&_pragma=wal_autocheckpoint(" + checkpoint + ")"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	s.w = newWriter(db)

	// synchronous=FULL means the operator asked for durability across a power
	// cut; the event log honours the same request rather than quietly keeping
	// the weaker guarantee.
	evlog, err := seglog.OpenWith(dataDir, seglog.Options{
		Sync:         sync == "FULL" || sync == "EXTRA",
		SegmentBytes: opt.EventLogSegmentBytes,
		MaxBytes:     opt.EventLogMaxBytes,
	})
	if err != nil {
		s.w.Close()
		db.Close()
		return nil, err
	}
	s.evlog = evlog
	return s, nil
}

// EventLogStats reports what the causal event log is holding on disk and in
// memory. Surfaced because "how big has my history got" is the question that
// decides whether EventLogMaxBytes wants setting, and a node that cannot answer
// it leaves the operator guessing.
func (s *Store) EventLogStats() seglog.Stats {
	if s.evlog == nil {
		return seglog.Stats{}
	}
	return s.evlog.Stats()
}

// Close shuts the two write paths down before the database underneath them.
//
// The order is a correctness requirement, not tidiness. Both writer.Close and
// seglog.Close stop admitting, commit what they already accepted, and only then
// return — so by the time db.Close runs there is no transaction still open
// against it, and no caller is left blocked on a write that will never be
// answered. Closing the database first would abort an in-flight ledger commit
// and report success to the operator running the shutdown.
func (s *Store) Close() error {
	if s.w != nil {
		s.w.Close()
	}
	if s.evlog != nil {
		if err := s.evlog.Close(); err != nil {
			s.db.Close()
			return err
		}
	}
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS skills (
  id         TEXT NOT NULL,
  version    TEXT NOT NULL,
  manifest   TEXT NOT NULL,
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL,
  PRIMARY KEY (id, version)
);
CREATE TABLE IF NOT EXISTS graphs (
  graph_id TEXT PRIMARY KEY,
  ir       TEXT NOT NULL,
  created  INTEGER NOT NULL
);
-- Every version of a graph that was ever registered, append-only.
--
-- The digest is the SHA-256 of the IR, which is what makes a revision a fact
-- rather than a timestamp: two revisions with the same digest are the same
-- graph, and re-registering an unchanged one adds nothing. That matters
-- because "aura up" re-seeds its example graphs on every launch, and a history
-- that grew a row per restart would be noise nobody reads.
--
-- Nothing here is ever updated or deleted. Restoring an old version registers
-- it again, which appends; the history is not rewritten to make the past look
-- like the present.
CREATE TABLE IF NOT EXISTS graph_revisions (
  graph_id TEXT    NOT NULL,
  n        INTEGER NOT NULL,
  ir       TEXT    NOT NULL,
  digest   TEXT    NOT NULL,
  created  INTEGER NOT NULL,
  PRIMARY KEY (graph_id, n)
);
CREATE INDEX IF NOT EXISTS graph_revisions_by_graph ON graph_revisions(graph_id, n DESC);
CREATE TABLE IF NOT EXISTS sessions (
  session_id TEXT PRIMARY KEY,
  graph_id   TEXT NOT NULL,
  started    INTEGER NOT NULL,
  ended      INTEGER
);
CREATE TABLE IF NOT EXISTS projections (
  name   TEXT PRIMARY KEY,
  config TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS ingress (
  name   TEXT PRIMARY KEY,
  config TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS skill_config (
  skill_id TEXT PRIMARY KEY,
  values_  TEXT NOT NULL,
  updated  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  rowid_    INTEGER PRIMARY KEY AUTOINCREMENT,
  session   TEXT NOT NULL,
  msg_id    TEXT NOT NULL,
  cause_id  TEXT,
  envelope  TEXT NOT NULL,
  ts        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_session ON events(session, rowid_);
CREATE INDEX IF NOT EXISTS idx_events_msg ON events(msg_id);
CREATE TABLE IF NOT EXISTS ingress_seen (
  route  TEXT NOT NULL,
  digest TEXT NOT NULL,
  ts     INTEGER NOT NULL,
  PRIMARY KEY (route, digest)
);
CREATE INDEX IF NOT EXISTS idx_ingress_seen_ts ON ingress_seen(ts);
CREATE TABLE IF NOT EXISTS ledger_entries (
  seq     INTEGER PRIMARY KEY,
  hash    TEXT NOT NULL UNIQUE,
  session TEXT NOT NULL,
  entry   TEXT NOT NULL,
  ts      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS ledger_checkpoints (
  seq       INTEGER PRIMARY KEY,
  head_hash TEXT NOT NULL,
  pubkey    TEXT NOT NULL,
  signature TEXT NOT NULL,
  ts        INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS ledger_successions (
  seq         INTEGER PRIMARY KEY,
  from_pubkey TEXT NOT NULL,
  to_pubkey   TEXT NOT NULL,
  from_sig    TEXT NOT NULL,
  to_sig      TEXT NOT NULL,
  reason      TEXT NOT NULL DEFAULT '',
  ts          INTEGER NOT NULL
);
-- A key hands over exactly once. Two successions out of one key would fork the
-- chain of custody into two histories that both look valid, which is precisely
-- what an attacker holding a stolen key would try to write. The constraint is
-- here rather than in Go because it has to hold against anything that opens
-- this file, not only against the kernel.
CREATE UNIQUE INDEX IF NOT EXISTS idx_succession_from ON ledger_successions(from_pubkey);
CREATE UNIQUE INDEX IF NOT EXISTS idx_succession_to ON ledger_successions(to_pubkey);
CREATE TABLE IF NOT EXISTS inference_attestations (
  hash   TEXT PRIMARY KEY,
  record TEXT NOT NULL,
  ts     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS ledger_witnesses (
  seq         INTEGER NOT NULL,
  witness_key TEXT NOT NULL,
  merkle_root TEXT NOT NULL,
  signature   TEXT NOT NULL,
  witness_url TEXT,
  ts          INTEGER NOT NULL,
  PRIMARY KEY (seq, witness_key)
);
CREATE TABLE IF NOT EXISTS witnessed_heads (
  node        TEXT PRIMARY KEY,
  seq         INTEGER NOT NULL,
  merkle_root TEXT NOT NULL,
  pubkey      TEXT NOT NULL,
  ts          INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS witness_log (
  seq         INTEGER PRIMARY KEY,
  node        TEXT NOT NULL,
  node_seq    INTEGER NOT NULL,
  merkle_root TEXT NOT NULL,
  node_pubkey TEXT NOT NULL,
  signature   TEXT NOT NULL,
  ts          INTEGER NOT NULL,
  entry       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_witness_log_node ON witness_log(node, seq);
CREATE TABLE IF NOT EXISTS seen_witness_heads (
  witness_key TEXT PRIMARY KEY,
  witness_url TEXT,
  size        INTEGER NOT NULL,
  root        TEXT NOT NULL,
  ts          INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS operators (
  id      TEXT PRIMARY KEY,
  pubkey  TEXT NOT NULL,
  name    TEXT,
  added   INTEGER NOT NULL,
  revoked INTEGER
);
CREATE TABLE IF NOT EXISTS node_secrets (
  name       TEXT PRIMARY KEY,
  ciphertext TEXT NOT NULL,
  updated    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS node_tokens (
  id         TEXT PRIMARY KEY,
  hash       TEXT NOT NULL UNIQUE,
  scope      TEXT NOT NULL,
  capability TEXT,
  label      TEXT,
  created    INTEGER NOT NULL,
  revoked    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_node_tokens_hash ON node_tokens(hash);
`)
	if err != nil {
		return err
	}
	return s.addColumns()
}

// addColumns applies additive column migrations to tables that already exist
// in the field.
//
// `CREATE TABLE IF NOT EXISTS` is a no-op against a data directory written by
// an older binary, so a column added to the schema above would never appear
// there — the node would start and then fail on the first read. SQLite has no
// `ADD COLUMN IF NOT EXISTS`, and the canonical way to ask is to try it and
// recognise the one error that means "already there".
//
// Each migration must stay additive and nullable/defaulted, for the same
// reason C1-C4 are additive: an older binary pointed at a newer data
// directory has to keep working, and a column it does not know about is the
// only kind it can ignore safely.
func (s *Store) addColumns() error {
	migrations := []struct{ table, column, ddl string }{
		// C4 v1.2 — the RFC 6962 tree head a checkpoint commits to, alongside
		// the linear head_hash it has always carried.
		{"ledger_checkpoints", "merkle_root", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, m := range migrations {
		_, err := s.db.Exec(fmt.Sprintf(
			`ALTER TABLE %s ADD COLUMN %s %s`, m.table, m.column, m.ddl))
		if err != nil && !isDuplicateColumn(err) {
			return fmt.Errorf("migrate %s.%s: %w", m.table, m.column, err)
		}
	}
	return nil
}

// isDuplicateColumn recognises the "already migrated" case. Matching on the
// message is unpleasant but it is what the driver gives us: modernc's sqlite
// reports this as a generic error, and re-running a completed migration must
// not be fatal.
func isDuplicateColumn(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
}

// SeenIngressDelivery records a delivery digest and reports whether it had
// already been seen inside the window.
//
// This is what makes a captured webhook unreplayable. HMAC proves a body was
// signed by someone holding the secret; it says nothing about *when*, so a
// recorded delivery could be posted back forever and each copy would verify.
// The digest is a deterministic function of the delivery, so a second arrival
// collides with the first.
//
// It lives in the store rather than in memory because a replay that works
// after a restart is still a replay, and because a node with a public ingress
// route is exactly the node most likely to be restarted by a supervisor.
//
// Rows outside the window are dropped on the way past: the table stays
// proportional to traffic within the window rather than to traffic ever.
func (s *Store) SeenIngressDelivery(route, digest string, window time.Duration) (bool, error) {
	// `<=`, not `<`: a row sitting exactly on the cutoff is *not* inside the
	// window — remembered means `now - ts < window`, so forgotten is
	// `ts <= now - window`. The strict form also made the boundary depend on
	// wall-clock luck, since ts is only millisecond-resolution: with a zero
	// window (and, once appends stopped paying an fsync each, with any window
	// at all) the write and the check land in the same millisecond and the row
	// survived a cutoff it was supposed to fall on.
	cutoff := time.Now().Add(-window).UnixMilli()
	if _, err := s.db.Exec(`DELETE FROM ingress_seen WHERE ts <= ?`, cutoff); err != nil {
		return false, err
	}
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO ingress_seen (route, digest, ts) VALUES (?,?,?)`,
		route, digest, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 0, nil // nothing inserted means the digest was already there
}

func (s *Store) UpsertSkill(id, version string, manifest any) error {
	b, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	_, err = s.db.Exec(`
INSERT INTO skills (id, version, manifest, first_seen, last_seen) VALUES (?,?,?,?,?)
ON CONFLICT(id, version) DO UPDATE SET manifest=excluded.manifest, last_seen=excluded.last_seen`,
		id, version, string(b), now, now)
	return err
}

// GraphRevision is one registered version of a graph.
type GraphRevision struct {
	N       int    `json:"n"`
	Digest  string `json:"digest"`
	Created int64  `json:"created"`
	Bytes   int    `json:"bytes"`
	IR      []byte `json:"ir,omitempty"`
}

// SaveGraph stores a graph and appends a revision when its content changed.
//
// Deduplicated by digest against the newest revision, not against every one:
// registering A, then B, then A again is three things that happened and the
// history should say so. Only a re-register of what is already current is
// silence, which is the case that would otherwise fire on every node restart.
func (s *Store) SaveGraph(graphID string, ir []byte) error {
	sum := sha256.Sum256(ir)
	digest := hex.EncodeToString(sum[:])
	now := time.Now().UnixMilli()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var lastDigest string
	var lastN int
	err = tx.QueryRow(
		`SELECT digest, n FROM graph_revisions WHERE graph_id = ? ORDER BY n DESC LIMIT 1`,
		graphID).Scan(&lastDigest, &lastN)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	if lastDigest != digest {
		if _, err := tx.Exec(
			`INSERT INTO graph_revisions (graph_id, n, ir, digest, created) VALUES (?,?,?,?,?)`,
			graphID, lastN+1, string(ir), digest, now); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`
INSERT INTO graphs (graph_id, ir, created) VALUES (?,?,?)
ON CONFLICT(graph_id) DO UPDATE SET ir=excluded.ir`,
		graphID, string(ir), now); err != nil {
		return err
	}
	return tx.Commit()
}

// GraphRevisions lists a graph's history, newest first, without the IR bodies.
func (s *Store) GraphRevisions(graphID string) ([]GraphRevision, error) {
	rows, err := s.db.Query(
		`SELECT n, digest, created, LENGTH(ir) FROM graph_revisions WHERE graph_id = ? ORDER BY n DESC`,
		graphID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GraphRevision{}
	for rows.Next() {
		var r GraphRevision
		if err := rows.Scan(&r.N, &r.Digest, &r.Created, &r.Bytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GraphRevision returns one version's IR.
func (s *Store) GraphRevision(graphID string, n int) ([]byte, error) {
	var ir string
	err := s.db.QueryRow(
		`SELECT ir FROM graph_revisions WHERE graph_id = ? AND n = ?`, graphID, n).Scan(&ir)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("graph %q has no revision %d", graphID, n)
	}
	return []byte(ir), err
}

func (s *Store) LoadGraph(graphID string) ([]byte, error) {
	var ir string
	err := s.db.QueryRow(`SELECT ir FROM graphs WHERE graph_id = ?`, graphID).Scan(&ir)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("graph %q not found", graphID)
	}
	return []byte(ir), err
}

func (s *Store) ListGraphs() ([]string, error) {
	rows, err := s.db.Query(`SELECT graph_id FROM graphs ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SessionMeta summarizes one session for listings and `aura why`.
type SessionMeta struct {
	SessionID string `json:"session_id"`
	GraphID   string `json:"graph_id"`
	Started   int64  `json:"started"`
	Ended     int64  `json:"ended,omitempty"`
	Events    int    `json:"events"`
	Errors    int    `json:"errors"`
}

// ListSessions returns recent sessions, newest first.
//
// The per-session counts come from the event log's index rather than a JOIN:
// the log knows how many records a session has and how many are errors without
// touching a byte of disk, and the old query paid a full scan plus a
// json_extract per row to learn the same thing.
func (s *Store) ListSessions(limit int) ([]SessionMeta, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
SELECT session_id, graph_id, started, COALESCE(ended, 0)
FROM sessions ORDER BY started DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionMeta
	for rows.Next() {
		var m SessionMeta
		if err := rows.Scan(&m.SessionID, &m.GraphID, &m.Started, &m.Ended); err != nil {
			return nil, err
		}
		m.Events, m.Errors = s.evlog.Counts(m.SessionID)
		if m.Events == 0 {
			// A session from before the log existed; its counts are still in
			// the table it was written to.
			m.Events, m.Errors = s.legacyCounts(m.SessionID)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// legacyCounts reads the pre-seglog events table for one session.
func (s *Store) legacyCounts(session string) (events, errs int) {
	var e, x sql.NullInt64
	_ = s.db.QueryRow(`
SELECT COUNT(rowid_),
       SUM(CASE WHEN json_extract(envelope, '$.kind') = 'error' THEN 1 ELSE 0 END)
FROM events WHERE session = ?`, session).Scan(&e, &x)
	return int(e.Int64), int(x.Int64)
}

// GetSession returns one session's metadata.
func (s *Store) GetSession(sessionID string) (SessionMeta, error) {
	list, err := s.ListSessions(1000)
	if err != nil {
		return SessionMeta{}, err
	}
	for _, m := range list {
		if m.SessionID == sessionID {
			return m, nil
		}
	}
	return SessionMeta{}, fmt.Errorf("session %q not found", sessionID)
}

// StartSession records a session's start. On a session id that already
// exists — a resume (Phase 2): the client reconnected, or the kernel process
// itself restarted — it also clears `ended`, so a session that picks back up
// stops looking permanently ended to `aura why` and `GET /v1/sessions`.
func (s *Store) StartSession(sessionID, graphID string) error {
	// Batched like the event log: a node accepting a burst of connections
	// otherwise pays one transaction per session *at accept time*, which is
	// exactly when it can least afford to serialize.
	return s.w.Submit(`
INSERT INTO sessions (session_id, graph_id, started) VALUES (?,?,?)
ON CONFLICT(session_id) DO UPDATE SET ended=NULL`,
		sessionID, graphID, time.Now().UnixMilli())
}

func (s *Store) EndSession(sessionID string) error {
	return s.w.Submit(`UPDATE sessions SET ended=? WHERE session_id=?`,
		time.Now().UnixMilli(), sessionID)
}

func (s *Store) SaveProjection(name string, config []byte) error {
	_, err := s.db.Exec(`
INSERT INTO projections (name, config) VALUES (?,?)
ON CONFLICT(name) DO UPDATE SET config=excluded.config`, name, string(config))
	return err
}

// LoadProjections returns every persisted projection config.
func (s *Store) LoadProjections() (map[string][]byte, error) {
	rows, err := s.db.Query(`SELECT name, config FROM projections ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var name, cfg string
		if err := rows.Scan(&name, &cfg); err != nil {
			return nil, err
		}
		out[name] = []byte(cfg)
	}
	return out, rows.Err()
}

// SaveIngress persists one inbound webhook route.
func (s *Store) SaveIngress(name string, config []byte) error {
	_, err := s.db.Exec(`
INSERT INTO ingress (name, config) VALUES (?,?)
ON CONFLICT(name) DO UPDATE SET config=excluded.config`, name, string(config))
	return err
}

// LoadIngress returns every persisted inbound route.
func (s *Store) LoadIngress() (map[string][]byte, error) {
	rows, err := s.db.Query(`SELECT name, config FROM ingress ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var name, cfg string
		if err := rows.Scan(&name, &cfg); err != nil {
			return nil, err
		}
		out[name] = []byte(cfg)
	}
	return out, rows.Err()
}

// DeleteIngress removes a route. An inbound URL you cannot revoke is a
// security problem, so unlike projections this is deletable.
func (s *Store) DeleteIngress(name string) error {
	_, err := s.db.Exec(`DELETE FROM ingress WHERE name = ?`, name)
	return err
}

// LoadSkillManifest returns the most recently seen manifest for a skill id,
// across whichever version last connected — used to resolve a skill's
// declared config schema even while it is offline.
func (s *Store) LoadSkillManifest(skillID string) (json.RawMessage, error) {
	var manifest string
	err := s.db.QueryRow(
		`SELECT manifest FROM skills WHERE id = ? ORDER BY last_seen DESC LIMIT 1`, skillID,
	).Scan(&manifest)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("skill %q never seen by this node", skillID)
	}
	return json.RawMessage(manifest), err
}

// SaveSkillConfig persists runtime config overrides for a skill id (not
// version — a user's tuning survives a package upgrade). Replaces the
// whole value set; callers merge first if they want a partial update.
func (s *Store) SaveSkillConfig(skillID string, values map[string]any) error {
	b, err := json.Marshal(values)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
INSERT INTO skill_config (skill_id, values_, updated) VALUES (?,?,?)
ON CONFLICT(skill_id) DO UPDATE SET values_=excluded.values_, updated=excluded.updated`,
		skillID, string(b), time.Now().UnixMilli())
	return err
}

// LoadSkillConfig returns the stored override values for a skill id, or an
// empty map if none were ever set.
func (s *Store) LoadSkillConfig(skillID string) (map[string]any, error) {
	var raw string
	err := s.db.QueryRow(`SELECT values_ FROM skill_config WHERE skill_id = ?`, skillID).Scan(&raw)
	if err == sql.ErrNoRows {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AppendEvent persists an envelope into the causal event log (C3 rule 7).
//
// kind is passed rather than parsed back out of the envelope: the caller always
// has it, and re-decoding JSON on the hottest write path in the kernel to
// recover a field it just serialised is work for nothing.
func (s *Store) AppendEvent(session, msgID, causeID, kind string, envelope []byte) error {
	return s.evlog.Append(seglog.Entry{
		Session: session, MsgID: msgID, CauseID: causeID, Kind: kind,
		TS: time.Now().UnixMilli(), Envelope: envelope,
	})
}

// SessionEvents returns the raw envelopes of a session in causal-log order,
// together with their persist timestamps (unix millis, index-aligned).
func (s *Store) SessionEvents(session string, limit int) ([]json.RawMessage, []int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	entries, err := s.evlog.Session(session, limit)
	if err != nil {
		return nil, nil, err
	}
	if len(entries) == 0 {
		// Nothing in the log for this session, which for a data directory
		// written by an older binary means its history is still in the events
		// table. Reading it keeps `aura why` and `aura replay` working across
		// the upgrade instead of silently answering "no history" — the table is
		// never written to again, so it only shrinks in relevance.
		return s.legacySessionEvents(session, limit)
	}
	out := make([]json.RawMessage, 0, len(entries))
	times := make([]int64, 0, len(entries))
	for _, e := range entries {
		out = append(out, json.RawMessage(e.Envelope))
		times = append(times, e.TS)
	}
	return out, times, nil
}

// legacySessionEvents reads the pre-seglog events table.
func (s *Store) legacySessionEvents(session string, limit int) ([]json.RawMessage, []int64, error) {
	rows, err := s.db.Query(`SELECT envelope, ts FROM events WHERE session=? ORDER BY rowid_ LIMIT ?`,
		session, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	var times []int64
	for rows.Next() {
		var e string
		var ts int64
		if err := rows.Scan(&e, &ts); err != nil {
			return nil, nil, err
		}
		out = append(out, json.RawMessage(e))
		times = append(times, ts)
	}
	return out, times, rows.Err()
}

// The effect ledger (C4). This file only persists what
// kernel/internal/ledger hands it — the hash chain, the checkpoint cadence
// and every verification rule live there. Keeping the split matches how
// `events` already works: the store does not know what a causal chain is
// either, it just keeps rows in the order they arrived.

// AppendLedgerEntry persists one sealed effect. seq and hash are supplied by
// the caller rather than assigned here, because the chain is a property of
// the *sequence* of entries — something only the single in-process writer
// that computed each hash from the one before it can be trusted to assign.
// A UNIQUE constraint on hash makes a duplicate append (a bug, not a user
// action) fail loudly instead of silently forking the chain.
func (s *Store) AppendLedgerEntry(seq uint64, hash, session string, entry []byte) error {
	return s.w.Submit(
		`INSERT INTO ledger_entries (seq, hash, session, entry, ts) VALUES (?,?,?,?,?)`,
		seq, hash, session, string(entry), time.Now().UnixMilli())
}

// LedgerHead returns the most recently sealed entry's seq and hash, or
// (0, "", nil) for a ledger that has never sealed anything — the case the
// ledger package treats as its genesis.
func (s *Store) LedgerHead() (seq uint64, hash string, err error) {
	err = s.db.QueryRow(`SELECT seq, hash FROM ledger_entries ORDER BY seq DESC LIMIT 1`).
		Scan(&seq, &hash)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	return seq, hash, err
}

// LedgerEntryCount reports how many effects this node has ever sealed —
// cheap enough for /healthz to include on every request.
func (s *Store) LedgerEntryCount() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM ledger_entries`).Scan(&n)
	return n, err
}

// LedgerEntriesBySession returns one session's sealed entries in seq order —
// what `aura undo <session>` walks backwards. seq is the node's own global
// counter (C4), not per-session, but filtering by the session column still
// yields that session's entries in the order they were sealed, which is what
// "reverse causal order" needs.
func (s *Store) LedgerEntriesBySession(session string) ([]json.RawMessage, error) {
	rows, err := s.db.Query(
		`SELECT entry FROM ledger_entries WHERE session = ? ORDER BY seq`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(e))
	}
	return out, rows.Err()
}

// LedgerEntryByHash looks up one sealed entry by its own Hash() — the receipt
// a client holds after an effect is delivered. Used by `aura undo <receipt>`
// (single-effect mode) and, internally, to validate an undo request before a
// session for it is ever built.
func (s *Store) LedgerEntryByHash(hash string) (json.RawMessage, error) {
	var entry string
	err := s.db.QueryRow(`SELECT entry FROM ledger_entries WHERE hash = ?`, hash).Scan(&entry)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no ledger entry with receipt %q", hash)
	}
	return json.RawMessage(entry), err
}

// LedgerFindByCompensates reports whether any entry already *delivered* an
// undo of the entry whose hash is receiptHash — the idempotency check that
// keeps a successful undo a one-time action. Scoped to outcome=delivered
// deliberately: a gated undo a human denied also carries `compensates` (C4
// records the refused proposal, same as any other gate — see
// executor.sealDenial), and a denial must not permanently block a later,
// approved retry of the same undo. Uses SQLite's JSON1 extension the same
// way ListSessions already does for `$.kind`, so no schema change (a
// dedicated column) is needed for a field most entries will never carry.
func (s *Store) LedgerFindByCompensates(receiptHash string) (entry json.RawMessage, found bool, err error) {
	var e string
	err = s.db.QueryRow(
		`SELECT entry FROM ledger_entries
		 WHERE json_extract(entry, '$.compensates') = ?
		   AND json_extract(entry, '$.outcome') = 'delivered'
		 LIMIT 1`,
		receiptHash).Scan(&e)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return json.RawMessage(e), true, nil
}

// LedgerEntries returns entries in seq order starting at fromSeq (inclusive).
// limit<=0 means unbounded — what `aura verify` needs, since recomputing a
// hash chain requires every link; the paginated HTTP listing passes a real
// limit instead.
func (s *Store) LedgerEntries(fromSeq uint64, limit int) ([]json.RawMessage, error) {
	q := `SELECT entry FROM ledger_entries WHERE seq >= ? ORDER BY seq`
	args := []any{fromSeq}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(e))
	}
	return out, rows.Err()
}

// CheckpointRow is one signed checkpoint over the ledger's head.
type CheckpointRow struct {
	Seq       uint64 `json:"seq"`       // the last entry seq this checkpoint covers
	HeadHash  string `json:"head_hash"` // that entry's hash
	Pubkey    string `json:"pubkey"`    // base64 Ed25519 public key that signed it
	Signature string `json:"signature"` // base64 Ed25519 signature
	TS        int64  `json:"ts"`
	// MerkleRoot is the RFC 6962 tree head over the first Seq entries (C4
	// v1.2, additive). Empty on any checkpoint written before the tree
	// existed, which is why it is a plain string and not a required column:
	// a node that has been running since v1.1 keeps verifying, and only the
	// checkpoints written from now on carry a root. Verification picks the
	// signing payload from this field's presence — see ledger.checkpointPayload.
	MerkleRoot string `json:"merkle_root,omitempty"`
}

const checkpointCols = `seq, head_hash, pubkey, signature, ts, COALESCE(merkle_root, '')`

// SaveCheckpoint persists one signed checkpoint.
func (s *Store) SaveCheckpoint(cp CheckpointRow) error {
	_, err := s.db.Exec(
		`INSERT INTO ledger_checkpoints (seq, head_hash, pubkey, signature, ts, merkle_root)
		 VALUES (?,?,?,?,?,?)`,
		cp.Seq, cp.HeadHash, cp.Pubkey, cp.Signature, cp.TS, cp.MerkleRoot)
	return err
}

// LedgerCheckpoints returns every checkpoint in seq order — what `aura
// verify` walks to check every signature, not just the latest.
func (s *Store) LedgerCheckpoints() ([]CheckpointRow, error) {
	rows, err := s.db.Query(`SELECT ` + checkpointCols + ` FROM ledger_checkpoints ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckpointRow
	for rows.Next() {
		var cp CheckpointRow
		if err := rows.Scan(&cp.Seq, &cp.HeadHash, &cp.Pubkey, &cp.Signature, &cp.TS, &cp.MerkleRoot); err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

// LastCheckpoint returns the most recent checkpoint, if any. The ledger
// package reads this once at startup so checkpoint cadence resumes correctly
// across a restart instead of appearing to have "never checkpointed" and
// sealing one immediately.
func (s *Store) LastCheckpoint() (cp CheckpointRow, ok bool, err error) {
	err = s.db.QueryRow(
		`SELECT `+checkpointCols+` FROM ledger_checkpoints ORDER BY seq DESC LIMIT 1`,
	).Scan(&cp.Seq, &cp.HeadHash, &cp.Pubkey, &cp.Signature, &cp.TS, &cp.MerkleRoot)
	if err == sql.ErrNoRows {
		return CheckpointRow{}, false, nil
	}
	return cp, err == nil, err
}

// Inference attestations (C5). Content-addressed, so identical inference
// conditions across many deliveries cost one row rather than one per effect.

// SaveAttestation stores one attestation under its content address. Writing
// the same hash twice is a no-op — by definition the bytes are identical, so
// there is nothing to update and an error would be noise.
func (s *Store) SaveAttestation(hash string, record []byte, ts int64) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO inference_attestations (hash, record, ts) VALUES (?,?,?)`,
		hash, string(record), ts)
	return err
}

// Attestation reads one back by content address.
func (s *Store) Attestation(hash string) (json.RawMessage, error) {
	var record string
	err := s.db.QueryRow(
		`SELECT record FROM inference_attestations WHERE hash = ?`, hash).Scan(&record)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no inference attestation with hash %q", hash)
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(record), nil
}

// External anchoring (C4 v1.2). Two tables, two directions:
//
//	ledger_witnesses  countersignatures OTHERS gave to THIS node's ledger
//	witnessed_heads   the last head this node vouched for, per other node
//
// They are separate because they answer different questions and have
// different trust properties: the first is evidence this node collected about
// itself (and could therefore drop), the second is this node's own memory of
// what it promised about someone else (and is what makes a fork detectable).

// WitnessRow is one third-party countersignature over this node's head.
type WitnessRow struct {
	Seq        uint64 `json:"seq"`
	WitnessKey string `json:"witness_key"`
	MerkleRoot string `json:"merkle_root"`
	Signature  string `json:"signature"`
	WitnessURL string `json:"witness_url,omitempty"`
	TS         int64  `json:"ts"`
}

// SaveWitness records a countersignature. Re-witnessing the same head with
// the same key is idempotent rather than an error: a node may legitimately
// re-present a head after a restart, and the second countersignature carries
// no more information than the first.
func (s *Store) SaveWitness(w WitnessRow) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO ledger_witnesses
		   (seq, witness_key, merkle_root, signature, witness_url, ts)
		 VALUES (?,?,?,?,?,?)`,
		w.Seq, w.WitnessKey, w.MerkleRoot, w.Signature, w.WitnessURL, w.TS)
	return err
}

// LedgerWitnesses returns every countersignature in seq order.
func (s *Store) LedgerWitnesses() ([]WitnessRow, error) {
	rows, err := s.db.Query(
		`SELECT seq, witness_key, merkle_root, signature, COALESCE(witness_url, ''), ts
		   FROM ledger_witnesses ORDER BY seq, witness_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WitnessRow
	for rows.Next() {
		var w WitnessRow
		if err := rows.Scan(&w.Seq, &w.WitnessKey, &w.MerkleRoot, &w.Signature, &w.WitnessURL, &w.TS); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WitnessedHead is the last head this node vouched for on behalf of another.
type WitnessedHead struct {
	Node       string `json:"node"`
	Seq        uint64 `json:"seq"`
	MerkleRoot string `json:"merkle_root"`
	Pubkey     string `json:"pubkey"`
	TS         int64  `json:"ts"`
}

// SaveWitnessedHead replaces what this node remembers about another node's
// history. Replace rather than append, deliberately: the value of the record
// is "the furthest point we vouched for", and the consistency proof at that
// point already transitively covers every earlier one.
func (s *Store) SaveWitnessedHead(h WitnessedHead) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO witnessed_heads (node, seq, merkle_root, pubkey, ts)
		 VALUES (?,?,?,?,?)`,
		h.Node, h.Seq, h.MerkleRoot, h.Pubkey, h.TS)
	return err
}

// WitnessedHeadCount reports how many distinct nodes this witness remembers —
// what an open witness checks against its capacity bound.
func (s *Store) WitnessedHeadCount() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM witnessed_heads`).Scan(&n)
	return n, err
}

// PruneWitnessedHeads forgets nodes not heard from since cutoff (unix millis),
// and reports how many were dropped.
func (s *Store) PruneWitnessedHeads(cutoff int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM witnessed_heads WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LastWitnessedHead returns what this node last vouched for about nodeID.
func (s *Store) LastWitnessedHead(nodeID string) (h WitnessedHead, ok bool, err error) {
	err = s.db.QueryRow(
		`SELECT node, seq, merkle_root, pubkey, ts FROM witnessed_heads WHERE node = ?`, nodeID,
	).Scan(&h.Node, &h.Seq, &h.MerkleRoot, &h.Pubkey, &h.TS)
	if err == sql.ErrNoRows {
		return WitnessedHead{}, false, nil
	}
	return h, err == nil, err
}

// ── the witness's own log (C4 v1.4) ─────────────────────────────────
//
// `witnessed_heads` is what this witness currently believes about each node —
// one row per node, replaced as it advances. Useful, and not evidence: a table
// that is replaced cannot be audited, because nothing in it records what it
// used to say.
//
// This is the other half, and the half that makes a witness accountable rather
// than merely trusted: **every countersignature it ever issues, appended, never
// updated, never deleted**, with a Merkle tree over it. A witness that can
// silently revise what it vouched for is a witness you have to believe. One
// whose own history is append-only and publicly checkable is one you can catch.
//
// This is Certificate Transparency's own arrangement applied to the witness
// rather than to the log it watches: CT's logs are themselves Merkle logs that
// monitors follow, precisely so that "who watches the watcher" has an answer
// that is not "nobody".

// WitnessLogRow is one countersignature this witness issued, as stored.
type WitnessLogRow struct {
	Seq        uint64 `json:"seq"`
	Node       string `json:"node"`
	NodeSeq    uint64 `json:"node_seq"`
	MerkleRoot string `json:"merkle_root"`
	NodePubkey string `json:"node_pubkey"`
	Signature  string `json:"signature"`
	TS         int64  `json:"ts"`
	// Entry is the canonical JSON the Merkle leaf is computed over — stored
	// rather than re-marshalled at read time, for the same reason ledger
	// entries are: the tree a witness publishes and the tree a monitor rebuilds
	// must not be able to diverge on an encoding detail.
	Entry []byte `json:"-"`
}

// AppendWitnessLog records one issued countersignature. The seq is the
// witness's own monotonic counter and is assigned by the caller under the same
// lock that computed the tree, so the log's order and its tree can never
// disagree.
func (s *Store) AppendWitnessLog(r WitnessLogRow) error {
	_, err := s.db.Exec(
		`INSERT INTO witness_log (seq, node, node_seq, merkle_root, node_pubkey, signature, ts, entry)
		 VALUES (?,?,?,?,?,?,?,?)`,
		r.Seq, r.Node, r.NodeSeq, r.MerkleRoot, r.NodePubkey, r.Signature, r.TS, string(r.Entry))
	return err
}

// WitnessLogEntries returns the witness's own log from seq (1-based, inclusive)
// in order. limit 0 means all.
func (s *Store) WitnessLogEntries(fromSeq uint64, limit int) ([]WitnessLogRow, error) {
	q := `SELECT seq, node, node_seq, merkle_root, node_pubkey, signature, ts, entry
	        FROM witness_log WHERE seq >= ? ORDER BY seq`
	args := []any{fromSeq}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WitnessLogRow
	for rows.Next() {
		var r WitnessLogRow
		var entry string
		if err := rows.Scan(&r.Seq, &r.Node, &r.NodeSeq, &r.MerkleRoot,
			&r.NodePubkey, &r.Signature, &r.TS, &entry); err != nil {
			return nil, err
		}
		r.Entry = []byte(entry)
		out = append(out, r)
	}
	return out, rows.Err()
}

// WitnessLogSize reports how many countersignatures this witness has issued.
func (s *Store) WitnessLogSize() (uint64, error) {
	var n uint64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM witness_log`).Scan(&n)
	return n, err
}

// ── monitoring somebody else's witness ──────────────────────────────

// SeenWitnessHead is the furthest point this machine has verified about a
// witness's own log — the state `aura witness audit` compares against.
//
// Keeping it is what turns auditing from a snapshot into a guarantee: a
// witness that shrinks, forks, or cannot prove its new history extends the
// history you already recorded is caught only if you recorded it.
type SeenWitnessHead struct {
	WitnessKey string `json:"witness_key"`
	WitnessURL string `json:"witness_url,omitempty"`
	Size       uint64 `json:"size"`
	Root       string `json:"root"`
	TS         int64  `json:"ts"`
}

// SaveSeenWitnessHead records the furthest verified point for a witness.
func (s *Store) SaveSeenWitnessHead(h SeenWitnessHead) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO seen_witness_heads (witness_key, witness_url, size, root, ts)
		 VALUES (?,?,?,?,?)`,
		h.WitnessKey, h.WitnessURL, h.Size, h.Root, h.TS)
	return err
}

// SeenWitnessHeadFor returns what this machine last verified about a witness.
func (s *Store) SeenWitnessHeadFor(witnessKey string) (h SeenWitnessHead, ok bool, err error) {
	var url sql.NullString
	err = s.db.QueryRow(
		`SELECT witness_key, witness_url, size, root, ts FROM seen_witness_heads WHERE witness_key = ?`,
		witnessKey,
	).Scan(&h.WitnessKey, &url, &h.Size, &h.Root, &h.TS)
	if err == sql.ErrNoRows {
		return SeenWitnessHead{}, false, nil
	}
	h.WitnessURL = url.String
	return h, err == nil, err
}

// SeenWitnessHeads lists every witness this machine follows.
func (s *Store) SeenWitnessHeads() ([]SeenWitnessHead, error) {
	rows, err := s.db.Query(
		`SELECT witness_key, witness_url, size, root, ts FROM seen_witness_heads ORDER BY witness_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeenWitnessHead
	for rows.Next() {
		var h SeenWitnessHead
		var url sql.NullString
		if err := rows.Scan(&h.WitnessKey, &url, &h.Size, &h.Root, &h.TS); err != nil {
			return nil, err
		}
		h.WitnessURL = url.String
		out = append(out, h)
	}
	return out, rows.Err()
}

// ── operators (C4 v1.3) ─────────────────────────────────────────────
//
// Who may answer a gate. Only public keys live here: the node must never hold
// an operator's private key, or it could manufacture the approvals it is
// supposed to be audited by.

// OperatorRow is one enrolled human.
type OperatorRow struct {
	ID     string `json:"id"`
	Pubkey string `json:"pubkey"`
	Name   string `json:"name,omitempty"`
	Added  int64  `json:"added"`
	// Revoked is unix millis at revocation, or 0 while enrolled. Revocation is
	// recorded rather than deleted so an entry approved before it stays
	// explicable: the roster answers "who may approve now", never "was this
	// past approval valid", which only the sealed signature answers.
	Revoked int64 `json:"revoked,omitempty"`
}

// SaveOperator enrolls or re-enrolls an operator. Re-enrolling clears a prior
// revocation, which is the only way back in — deliberately explicit rather
// than a separate un-revoke verb nobody would find.
func (s *Store) SaveOperator(o OperatorRow) error {
	_, err := s.db.Exec(
		`INSERT INTO operators (id, pubkey, name, added, revoked) VALUES (?,?,?,?,NULL)
		 ON CONFLICT(id) DO UPDATE SET pubkey=excluded.pubkey, name=excluded.name, revoked=NULL`,
		o.ID, o.Pubkey, o.Name, o.Added)
	return err
}

// RevokeOperator marks an operator as no longer able to approve. It reports
// whether a row was actually changed, so a caller can tell "revoked" from
// "there was nobody by that name".
func (s *Store) RevokeOperator(id string, ts int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE operators SET revoked = ? WHERE id = ? AND revoked IS NULL`, ts, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Operator returns one enrolled human, revoked or not — the caller decides
// what revocation means for what it is doing.
func (s *Store) Operator(id string) (o OperatorRow, ok bool, err error) {
	var revoked sql.NullInt64
	var name sql.NullString
	err = s.db.QueryRow(
		`SELECT id, pubkey, name, added, revoked FROM operators WHERE id = ?`, id,
	).Scan(&o.ID, &o.Pubkey, &name, &o.Added, &revoked)
	if err == sql.ErrNoRows {
		return OperatorRow{}, false, nil
	}
	o.Name, o.Revoked = name.String, revoked.Int64
	return o, err == nil, err
}

// Operators lists everyone ever enrolled, by id.
func (s *Store) Operators() ([]OperatorRow, error) {
	rows, err := s.db.Query(`SELECT id, pubkey, name, added, revoked FROM operators ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OperatorRow
	for rows.Next() {
		var o OperatorRow
		var revoked sql.NullInt64
		var name sql.NullString
		if err := rows.Scan(&o.ID, &o.Pubkey, &name, &o.Added, &revoked); err != nil {
			return nil, err
		}
		o.Name, o.Revoked = name.String, revoked.Int64
		out = append(out, o)
	}
	return out, rows.Err()
}

// ── node secrets (the credential broker) ────────────────────────────
//
// Stored encrypted; see internal/broker for the key derivation and for why a
// secret is only ever released against a sealed receipt.

// SaveSecret stores (or replaces) one encrypted secret.
// SuccessionRow records one node signing key handing over to its replacement
// (C4 v1.6). Seq is the last entry sealed under FromPubkey, so the ranges the
// two keys cover meet without a gap and without an overlap.
//
// Both signatures cover the same payload and neither is redundant. FromSig is
// authorization: without it, anybody who can write to this table could append
// a row naming their own key and inherit the chain. ToSig is proof of
// possession: without it, an operator could bind the ledger's future to a key
// nobody holds — a node that can no longer sign anything, discovered at the
// next checkpoint.
type SuccessionRow struct {
	Seq        uint64 `json:"seq"`
	FromPubkey string `json:"from_pubkey"`
	ToPubkey   string `json:"to_pubkey"`
	FromSig    string `json:"from_sig"`
	ToSig      string `json:"to_sig"`
	Reason     string `json:"reason,omitempty"`
	TS         int64  `json:"ts"`
}

const successionCols = `seq, from_pubkey, to_pubkey, from_sig, to_sig, reason, ts`

// LedgerSuccessions returns every recorded handover in seq order.
func (s *Store) LedgerSuccessions() ([]SuccessionRow, error) {
	rows, err := s.db.Query(`SELECT ` + successionCols + ` FROM ledger_successions ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SuccessionRow
	for rows.Next() {
		var r SuccessionRow
		if err := rows.Scan(&r.Seq, &r.FromPubkey, &r.ToPubkey,
			&r.FromSig, &r.ToSig, &r.Reason, &r.TS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CommitSuccession writes the handover and re-encrypts every stored secret
// under the incoming key, in one transaction.
//
// One transaction because they are one fact. The broker derives its
// secret-box key from the node's private key, so a succession recorded
// without the re-encryption leaves a node whose ledger says the new key is in
// force and whose secrets can only be opened by the old one — and a
// re-encryption committed without the succession leaves the opposite. Either
// half alone is worse than neither, and the window between two separate writes
// is exactly where a crash puts an operator who is already rotating because
// something went wrong.
//
// reseal receives each secret's name and current ciphertext and returns the
// ciphertext under the new key. It runs inside the transaction; returning an
// error rolls the whole thing back, leaving the node exactly as it was.
func (s *Store) CommitSuccession(rec SuccessionRow, reseal func(name, ciphertext string) (string, error)) (rekeyed int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.Exec(
		`INSERT INTO ledger_successions (`+successionCols+`) VALUES (?,?,?,?,?,?,?)`,
		rec.Seq, rec.FromPubkey, rec.ToPubkey, rec.FromSig, rec.ToSig, rec.Reason, rec.TS,
	); err != nil {
		return 0, fmt.Errorf("record succession: %w", err)
	}

	rows, err := tx.Query(`SELECT name, ciphertext FROM node_secrets ORDER BY name`)
	if err != nil {
		return 0, fmt.Errorf("read secrets: %w", err)
	}
	type secret struct{ name, ciphertext string }
	var secrets []secret
	for rows.Next() {
		var sec secret
		if err = rows.Scan(&sec.name, &sec.ciphertext); err != nil {
			rows.Close()
			return 0, err
		}
		secrets = append(secrets, sec)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	for _, sec := range secrets {
		var next string
		if next, err = reseal(sec.name, sec.ciphertext); err != nil {
			return 0, fmt.Errorf("re-encrypt secret %q: %w", sec.name, err)
		}
		if _, err = tx.Exec(
			`UPDATE node_secrets SET ciphertext = ?, updated = ? WHERE name = ?`,
			next, rec.TS, sec.name,
		); err != nil {
			return 0, fmt.Errorf("store re-encrypted secret %q: %w", sec.name, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return len(secrets), nil
}

func (s *Store) SaveSecret(name, ciphertext string, ts int64) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO node_secrets (name, ciphertext, updated) VALUES (?,?,?)`,
		name, ciphertext, ts)
	return err
}

// Secret returns one secret's ciphertext.
func (s *Store) Secret(name string) (ciphertext string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT ciphertext FROM node_secrets WHERE name = ?`, name).Scan(&ciphertext)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return ciphertext, err == nil, err
}

// SecretNames lists the stored secrets by name. Names only — the values are
// never enumerable, which is what keeps "list what is configured" a safe
// operation to expose on the control plane.
func (s *Store) SecretNames() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM node_secrets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DeleteSecret removes a secret, reporting whether one was there.
func (s *Store) DeleteSecret(name string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM node_secrets WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ── scoped node tokens ──────────────────────────────────────────────
//
// A node used to have exactly one credential, so every caller that could reach
// it held the operator's authority: a skill process, the CLI and the browser UI
// all presented the same bearer token. That made the credential broker's
// guarantee weaker than it reads — a receipt buys a secret, but a caller holding
// the node token could register a graph, waive its own gate and mint the receipt.
//
// These rows are the second kind of credential: narrow, per-skill, revocable,
// and bound at issue time to the one capability the holder is allowed to be.
//
// Only the hash is stored. A token readable out of the database would make the
// database a credential store, and an operator restoring a backup would be
// restoring live credentials rather than a record that they existed.

// TokenRow is one issued credential. The token itself is returned once, at
// issue, and never persisted.
type TokenRow struct {
	ID    string `json:"id"`
	Scope string `json:"scope"`
	// Capability is the C1 capability a `skill` token may register as. Empty for
	// scopes to which it does not apply.
	Capability string `json:"capability,omitempty"`
	Label      string `json:"label,omitempty"`
	Created    int64  `json:"created"`
	// Revoked is unix millis at revocation, or 0 while live. Recorded rather
	// than deleted for the same reason an operator revocation is: an entry
	// sealed while the token was valid stays explicable afterwards.
	Revoked int64 `json:"revoked,omitempty"`
}

// SaveToken records an issued token by hash.
func (s *Store) SaveToken(t TokenRow, hash string) error {
	_, err := s.db.Exec(
		`INSERT INTO node_tokens (id, hash, scope, capability, label, created, revoked)
		 VALUES (?,?,?,?,?,?,NULL)`,
		t.ID, hash, t.Scope, t.Capability, t.Label, t.Created)
	return err
}

// TokenByHash resolves a presented credential. The caller hashes; this never
// sees a token in the clear.
func (s *Store) TokenByHash(hash string) (TokenRow, bool, error) {
	var t TokenRow
	var capability, label sql.NullString
	var revoked sql.NullInt64
	err := s.db.QueryRow(
		`SELECT id, scope, capability, label, created, revoked FROM node_tokens WHERE hash = ?`,
		hash).Scan(&t.ID, &t.Scope, &capability, &label, &t.Created, &revoked)
	if err == sql.ErrNoRows {
		return TokenRow{}, false, nil
	}
	if err != nil {
		return TokenRow{}, false, err
	}
	t.Capability, t.Label, t.Revoked = capability.String, label.String, revoked.Int64
	return t, true, nil
}

// Tokens lists issued credentials, newest first. Hashes are never returned:
// a listing endpoint that leaked them would let a reader mount an offline
// search for the token that produced one.
func (s *Store) Tokens() ([]TokenRow, error) {
	rows, err := s.db.Query(
		`SELECT id, scope, capability, label, created, revoked FROM node_tokens
		 ORDER BY created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenRow
	for rows.Next() {
		var t TokenRow
		var capability, label sql.NullString
		var revoked sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Scope, &capability, &label, &t.Created, &revoked); err != nil {
			return nil, err
		}
		t.Capability, t.Label, t.Revoked = capability.String, label.String, revoked.Int64
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken stops a credential working, reporting whether one was live.
func (s *Store) RevokeToken(id string, ts int64) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE node_tokens SET revoked = ? WHERE id = ? AND revoked IS NULL`, ts, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// WriteStats reports what the group-commit batcher has done. See
// writer.Stats for why the mean batch size is the number that matters.
func (s *Store) WriteStats() WriteStats {
	if s.w == nil {
		return WriteStats{}
	}
	return s.w.Stats()
}
