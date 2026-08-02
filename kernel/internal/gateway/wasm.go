package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/spec"
	"aura/kernel/internal/wasmrt"
)

// Wasm skills (Phase 3, ROADMAP.md): what makes C1 rule 3 — "permissions are
// enforced, not merely declared" — true for `format: wasm`, the same way the
// Effect Checkpoint (C4) made the motor-gate invariant true rather than
// documented. A wasm skill never dials in over /ws/skill the way a `source`
// skill does: it is hosted *inside* this process by wasmrt, and this
// endpoint is the only way one is ever registered — `aura run` (for an
// installed wasm package) POSTs here instead of spawning a process.
//
// Once registered, a wasm skill is an ordinary registry.Live. Session.forward,
// sealEffect, policy, the ledger — none of it knows or cares that this
// particular Live's Send runs a sandboxed wasm module instead of writing to
// a socket.

// wasmInvokeTimeout bounds one delivery's execution. Not yet configurable
// per skill (no C1 field for it exists) — a fixed, generous default so a
// runaway guest is a bounded failure (see wasmrt.Module.Invoke) rather than
// an unbounded one, not a throughput tuning knob.
const wasmInvokeTimeout = 30 * time.Second

// registerWasmSkillRequest is POST /v1/skills/wasm's body.
type registerWasmSkillRequest struct {
	Manifest registry.Manifest `json:"manifest"`
	WasmB64  string            `json:"wasm_b64"`
}

// registerWasmSkill compiles the module, registers it as a live skill, and
// persists its manifest — the wasm-hosted equivalent of skillWS's
// registration handshake, minus the socket.
func (g *Gateway) registerWasmSkill(w http.ResponseWriter, r *http.Request) {
	if g.Wasm == nil {
		writeJSON(w, 404, map[string]string{"error": "this node has no wasm runtime configured"})
		return
	}
	var req registerWasmSkillRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}
	m := req.Manifest
	if err := m.Validate(); err != nil {
		writeJSON(w, 422, map[string]string{"error": "manifest rejected: " + err.Error()})
		return
	}
	if m.Format != spec.FormatWasm {
		writeJSON(w, 422, map[string]string{"error": fmt.Sprintf("manifest declares format %q, not %q", m.Format, spec.FormatWasm)})
		return
	}
	// v1's guest contract (ROADMAP.md, Phase 3) is synchronous single
	// request/response: one ingress port in, one egress port out. A
	// streaming or multi-port wasm skill is future work, not something this
	// endpoint silently mishandles.
	if len(m.Ports.Ingress) != 1 || len(m.Ports.Egress) != 1 {
		writeJSON(w, 422, map[string]string{
			"error": fmt.Sprintf("wasm skills declare exactly one ingress and one egress port "+
				"(got %d ingress, %d egress)", len(m.Ports.Ingress), len(m.Ports.Egress)),
		})
		return
	}
	wasmBytes, err := base64.StdEncoding.DecodeString(req.WasmB64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "wasm_b64: " + err.Error()})
		return
	}
	perm, err := wasmrt.ParsePermissions(m.Permissions)
	if err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}

	module, err := g.Wasm.Compile(r.Context(), wasmBytes)
	if err != nil {
		writeJSON(w, 422, map[string]string{"error": "compile: " + err.Error()})
		return
	}

	ingressPort := m.Ports.Ingress[0].Name
	egressPort := m.Ports.Egress[0]

	connID := channel.NewID()
	live := &registry.Live{
		Manifest:  m,
		Connected: time.Now(),
		Send:      g.wasmSend(module, perm, ingressPort, egressPort.Name, egressPort.Schema),
	}
	g.Reg.Register(connID, live)
	if err := g.St.UpsertSkill(m.ID, m.Version, m); err != nil {
		g.Log.Error("persist wasm skill failed", "err", err)
	}
	g.Log.Info("wasm skill registered", "skill", m.ID, "type", m.Type, "capability", m.Capability)

	writeJSON(w, 201, map[string]string{"id": m.ID, "version": m.Version})
}

// wasmSend builds the Send closure a wasm-hosted Live uses. It mirrors what
// a well-behaved `source` skill's own SDK does on every emission — echo the
// `Node` (graph ref) it was addressed as, cause the reply on the envelope
// that triggered it — except the kernel does it on the guest's behalf,
// because the guest itself only ever sees a payload in and a payload out
// (see wasmrt.Module.Invoke's doc comment on the v1 guest contract).
//
// Fire-and-forget, like a WS skill's Send: Session.forward calls Send and
// moves on, and the reply arrives later via Manager.Dispatch — here, from
// the goroutine actually running the sandboxed module, there, from
// skillWS's read loop.
func (g *Gateway) wasmSend(module *wasmrt.Module, perm wasmrt.Permission,
	ingressPort, egressPort, egressSchema string) func([]byte, string) error {
	return func(raw []byte, _ string) error {
		var incoming channel.Envelope
		if err := json.Unmarshal(raw, &incoming); err != nil {
			return err
		}
		if incoming.Kind != channel.KindData {
			return nil // v1's guest contract has nothing to do with cancel/config_update/etc.
		}
		if incoming.Port != ingressPort {
			return fmt.Errorf("delivery on port %q, but this wasm skill's declared ingress is %q",
				incoming.Port, ingressPort)
		}

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), wasmInvokeTimeout)
			defer cancel()
			stdout, stderr, exitCode, err := module.Invoke(ctx, incoming.Payload, perm)

			if err != nil {
				g.dispatchWasmError(incoming, egressPort, err.Error())
				return
			}
			if exitCode != 0 {
				g.dispatchWasmError(incoming, egressPort, string(stderr))
				return
			}

			data := channel.Envelope{
				V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: incoming.ID,
				Session: incoming.Session, Node: incoming.Node, Port: egressPort,
				Seq: 1, Idem: incoming.ID + ":wasm-reply",
				Schema: egressSchema, Kind: channel.KindData, Payload: stdout,
			}
			if err := g.Mgr.Dispatch(data); err != nil {
				g.Log.Error("wasm reply dispatch failed", "err", err)
				return
			}
			done := channel.Envelope{
				V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: incoming.ID,
				Session: incoming.Session, Node: incoming.Node, Port: egressPort,
				Kind: channel.KindDone,
			}
			if err := g.Mgr.Dispatch(done); err != nil {
				g.Log.Error("wasm done dispatch failed", "err", err)
			}
		}()
		return nil
	}
}

// dispatchWasmError routes an error the same way a `source` skill's own
// error emission would be: from its declared *egress* port (not the
// ingress port the failing delivery arrived on), through the ordinary
// routing table — the wrong port here means the envelope matches no edge
// and is silently dropped by Route's "no route" path instead of ever
// reaching the client.
func (g *Gateway) dispatchWasmError(incoming channel.Envelope, egressPort, detail string) {
	payload, _ := json.Marshal(map[string]string{"state": "error", "detail": detail})
	errEnv := channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: incoming.ID,
		Session: incoming.Session, Node: incoming.Node, Port: egressPort,
		Kind: channel.KindError, Schema: "std/status@1", Payload: payload,
	}
	if err := g.Mgr.Dispatch(errEnv); err != nil {
		g.Log.Error("wasm error dispatch failed", "err", err)
	}
}
