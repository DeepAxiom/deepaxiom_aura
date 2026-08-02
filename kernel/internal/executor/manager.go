package executor

import (
	"fmt"
	"log/slog"
	"sync"

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
}

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

// Start instantiates a session for a stored graph and wires the client sender.
func (m *Manager) Start(sessionID, graphID string,
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
	sess, err := NewSession(sessionID, g, m.reg, m.st, m.mode, m.policy, m.ldg, sendClient, m.log)
	if err != nil {
		return nil, err
	}
	if err := m.st.StartSession(sessionID, graphID); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.sessions[sessionID] = sess
	m.mu.Unlock()
	m.log.Info("session started", "session", sessionID, "graph", graphID)
	return sess, nil
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
	sess.Route(env)
	return nil
}
