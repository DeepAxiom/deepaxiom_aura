// Package store is the kernel's embedded state: SQLite, zero external services.
//
// It holds ONLY kernel state (skills, graphs, sessions, causal event log).
// Application data never lives here — that is what projections are for.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no CGO, single-binary friendly
)

type Store struct {
	db *sql.DB
}

func Open(dataDir string) (*Store, error) {
	dsn := filepath.Join(dataDir, "kernel.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

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
`)
	return err
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
	cutoff := time.Now().Add(-window).UnixMilli()
	if _, err := s.db.Exec(`DELETE FROM ingress_seen WHERE ts < ?`, cutoff); err != nil {
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

func (s *Store) SaveGraph(graphID string, ir []byte) error {
	_, err := s.db.Exec(`
INSERT INTO graphs (graph_id, ir, created) VALUES (?,?,?)
ON CONFLICT(graph_id) DO UPDATE SET ir=excluded.ir`,
		graphID, string(ir), time.Now().UnixMilli())
	return err
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
func (s *Store) ListSessions(limit int) ([]SessionMeta, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
SELECT s.session_id, s.graph_id, s.started, COALESCE(s.ended, 0),
       COUNT(e.rowid_),
       SUM(CASE WHEN json_extract(e.envelope, '$.kind') = 'error' THEN 1 ELSE 0 END)
FROM sessions s LEFT JOIN events e ON e.session = s.session_id
GROUP BY s.session_id ORDER BY s.started DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionMeta
	for rows.Next() {
		var m SessionMeta
		var errs sql.NullInt64
		if err := rows.Scan(&m.SessionID, &m.GraphID, &m.Started, &m.Ended,
			&m.Events, &errs); err != nil {
			return nil, err
		}
		m.Errors = int(errs.Int64)
		out = append(out, m)
	}
	return out, rows.Err()
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
	_, err := s.db.Exec(`
INSERT INTO sessions (session_id, graph_id, started) VALUES (?,?,?)
ON CONFLICT(session_id) DO UPDATE SET ended=NULL`,
		sessionID, graphID, time.Now().UnixMilli())
	return err
}

func (s *Store) EndSession(sessionID string) error {
	_, err := s.db.Exec(`UPDATE sessions SET ended=? WHERE session_id=?`, time.Now().UnixMilli(), sessionID)
	return err
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
func (s *Store) AppendEvent(session, msgID, causeID string, envelope []byte) error {
	_, err := s.db.Exec(`INSERT INTO events (session, msg_id, cause_id, envelope, ts) VALUES (?,?,?,?,?)`,
		session, msgID, causeID, string(envelope), time.Now().UnixMilli())
	return err
}

// SessionEvents returns the raw envelopes of a session in causal-log order,
// together with their persist timestamps (unix millis, index-aligned).
func (s *Store) SessionEvents(session string, limit int) ([]json.RawMessage, []int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.Query(`SELECT envelope, ts FROM events WHERE session=? ORDER BY rowid_ LIMIT ?`, session, limit)
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
	_, err := s.db.Exec(
		`INSERT INTO ledger_entries (seq, hash, session, entry, ts) VALUES (?,?,?,?,?)`,
		seq, hash, session, string(entry), time.Now().UnixMilli())
	return err
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
}

// SaveCheckpoint persists one signed checkpoint.
func (s *Store) SaveCheckpoint(cp CheckpointRow) error {
	_, err := s.db.Exec(
		`INSERT INTO ledger_checkpoints (seq, head_hash, pubkey, signature, ts) VALUES (?,?,?,?,?)`,
		cp.Seq, cp.HeadHash, cp.Pubkey, cp.Signature, cp.TS)
	return err
}

// LedgerCheckpoints returns every checkpoint in seq order — what `aura
// verify` walks to check every signature, not just the latest.
func (s *Store) LedgerCheckpoints() ([]CheckpointRow, error) {
	rows, err := s.db.Query(
		`SELECT seq, head_hash, pubkey, signature, ts FROM ledger_checkpoints ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckpointRow
	for rows.Next() {
		var cp CheckpointRow
		if err := rows.Scan(&cp.Seq, &cp.HeadHash, &cp.Pubkey, &cp.Signature, &cp.TS); err != nil {
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
		`SELECT seq, head_hash, pubkey, signature, ts FROM ledger_checkpoints ORDER BY seq DESC LIMIT 1`,
	).Scan(&cp.Seq, &cp.HeadHash, &cp.Pubkey, &cp.Signature, &cp.TS)
	if err == sql.ErrNoRows {
		return CheckpointRow{}, false, nil
	}
	return cp, err == nil, err
}
