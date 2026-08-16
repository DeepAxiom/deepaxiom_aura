package executor

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"

	"aura/kernel/internal/approver"
	"aura/kernel/internal/channel"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/store"
)

// Manager owns all live sessions on this node (single-writer per session).
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	reg      *registry.Registry
	st       *store.Store
	log      *slog.Logger
	// mode is the node's security mode; it decides how an ungated edge into a
	// motor skill is treated and whether a graph may waive a gate at all
	// (see applyPolicy).
	mode string
	// policy is the node's authorization document, shared by every session:
	// one document in force per node is what makes "which policy allowed this"
	// answerable.
	policy *Policy
	// ledger is the node's effect ledger (C4), shared by every session for the
	// same reason policy is: attestation is a property of the node's history,
	// not of any one conversation, so checkpoints have to cover everything the
	// node ever sealed, not one session's slice of it.
	ldg *ledger.Ledger
	// maxSessions caps live sessions, 0 = unlimited. A public ingress route
	// opens a session per delivery, so without a cap an unauthenticated flood
	// is an out-of-memory condition rather than a rejected request.
	maxSessions int
	// approvers is the node's enrolled operator roster (C4 v1.3). Shared by
	// every session, for the same reason policy and the ledger are: who may
	// approve is a property of the node, and a per-session roster would mean
	// whoever opened the session got to say who could authorize its writes.
	approvers *approver.Registry
}

// SetApprovers installs the enrolled operator roster. Separate from NewManager
// on purpose: the roster is loaded from the same store the manager already has,
// so threading it through the constructor would only widen a signature every
// test calls, to pass a value nearly none of them exercise.
func (m *Manager) SetApprovers(r *approver.Registry) { m.approvers = r }

// Approvers is the roster in force, for the control plane and startup checks.
func (m *Manager) Approvers() *approver.Registry { return m.approvers }

func NewManager(reg *registry.Registry, st *store.Store, mode string,
	pol *Policy, ldg *ledger.Ledger, log *slog.Logger) *Manager {
	if pol == nil {
		pol = DefaultPolicy()
	}
	return &Manager{sessions: map[string]*Session{}, reg: reg, st: st,
		mode: mode, policy: pol, ldg: ldg, log: log}
}

// SetMaxSessions caps concurrent live sessions. Zero means unlimited.
func (m *Manager) SetMaxSessions(n int) { m.maxSessions = n }

// Policy returns the document in force, for the startup banner and health.
func (m *Manager) Policy() *Policy { return m.policy }

// LiveSessions reports how many sessions are currently held.
func (m *Manager) LiveSessions() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// Start instantiates a session for a stored graph and wires the client
// sender. undoOf is empty for an ordinary session; non-empty marks this as
// an ephemeral undo session (Phase 2, `aura undo`) — see NewSession.
func (m *Manager) Start(sessionID, graphID, undoOf string,
	sendClient func(raw []byte, qos string) error) (*Session, error) {
	irRaw, err := m.st.LoadGraph(graphID)
	if err != nil {
		return nil, err
	}
	g, err := ParseGraph(irRaw)
	if err != nil {
		return nil, fmt.Errorf("stored graph %q is invalid: %w", graphID, err)
	}
	// Checked before the session is built so a node under flood spends nothing
	// resolving skills for work it is about to refuse.
	if m.maxSessions > 0 {
		m.mu.RLock()
		live := len(m.sessions)
		m.mu.RUnlock()
		if live >= m.maxSessions {
			return nil, fmt.Errorf("node is at its session limit (%d live); "+
				"retry shortly or raise --max-sessions", m.maxSessions)
		}
	}
	sess, err := NewSession(sessionID, g, m.reg, m.st, m.mode, m.policy, m.ldg, undoOf, sendClient, m.log)
	if err != nil {
		return nil, err
	}
	sess.approvers = m.approvers
	if err := m.st.StartSession(sessionID, graphID); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.sessions[sessionID] = sess
	m.mu.Unlock()
	m.log.Info("session started", "session", sessionID, "graph", graphID)
	return sess, nil
}

// Get returns a live session by id. Used by borders that drive a session
// across several HTTP requests (the OpenEnv episode loop) rather than holding
// one socket open the way a WebSocket client does.
func (m *Manager) Get(sessionID string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[sessionID]
	return sess, ok
}

func (m *Manager) End(sessionID string) {
	m.mu.Lock()
	delete(m.sessions, sessionID)
	m.mu.Unlock()
	_ = m.st.EndSession(sessionID)
	m.log.Info("session ended", "session", sessionID)
}

// Dispatch routes a skill-emitted envelope to its session.
func (m *Manager) Dispatch(env channel.Envelope) error {
	m.mu.RLock()
	sess, ok := m.sessions[env.Session]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown session %q", env.Session)
	}
	return m.route(sess, env)
}

// route runs one session's routing with a blast radius of one session.
//
// Before this, a panic anywhere in routing — a malformed payload hitting an
// unchecked type assertion, a nil map in a skill's reply, a bug in a code path
// nobody exercised — unwound to the top of the goroutine and took the whole
// node with it. Every other session, every open connection and the ledger's
// in-memory chain state all died with it, because of one bad envelope on one
// connection.
//
// That is the property the BEAM gives away for free and it is the one real
// argument for putting this tier on another runtime. It is also, at this scale,
// twenty lines: routing is synchronous end to end (Session.Route spawns
// nothing), so a single recover here contains everything a session does.
//
// Deliberately narrow. Recovering does *not* pretend the work succeeded: the
// panic is logged with its session and stack and returned as an error, so the
// caller fails that delivery the same way it would fail any other. Nothing is
// silently swallowed — the difference is only that the other thousand sessions
// keep running.
func (m *Manager) route(sess *Session, env channel.Envelope) (err error) {
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("panic while routing; this session failed, the node did not",
				"session", env.Session, "envelope", env.ID, "kind", env.Kind,
				"panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("internal error routing envelope %s in session %s",
				env.ID, env.Session)
		}
	}()
	sess.Route(env)
	return nil
}
