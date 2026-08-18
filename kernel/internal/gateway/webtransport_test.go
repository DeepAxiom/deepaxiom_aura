package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/wtsrv"
)

// The claim these tests check is the one that matters after the refactor: a
// skill that arrives over QUIC is indistinguishable, to everything above the
// transport, from one that arrived over a WebSocket. Same registration
// handshake, same admission, same registry entry, same dispatch — because it is
// literally the same code path, reached through the `wire` interface.
//
// If these ever diverge, the C3 promise that envelopes travel over whatever
// carries them stops being true, and a graph starts caring how a skill
// connected.

func wtNode(t *testing.T, g *Gateway) string {
	t.Helper()
	s := &wtsrv.Server{
		Addr:    "127.0.0.1:0",
		DataDir: t.TempDir(),
		Log:     g.Log,
		Routes: map[string]wtsrv.Handler{
			"/ws/skill": func(_ context.Context, sess *wtsrv.Session, _ *http.Request) {
				g.ServeSkill(sess, Principal{Scope: ScopeOperator})
			},
		},
	}
	if err := s.Start(); err != nil {
		t.Fatalf("start webtransport: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.LocalAddr()
}

type wtClient struct {
	sess *webtransport.Session
	ctrl *webtransport.Stream
}

func dialWT(t *testing.T, addr string) *wtClient {
	t.Helper()
	tr := &webtransport.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{http3.NextProtoH3},
		},
		QUICConfig: &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	t.Cleanup(func() { _ = tr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, sess, err := tr.Dial(ctx, fmt.Sprintf("https://%s/ws/skill", addr), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ctrl, err := sess.OpenStream()
	if err != nil {
		t.Fatalf("open control stream: %v", err)
	}
	t.Cleanup(func() { _ = sess.CloseWithError(0, "") })
	return &wtClient{sess: sess, ctrl: ctrl}
}

// send writes one envelope on the control (reliable) lane, framed the way the
// server frames it.
func (c *wtClient) send(t *testing.T, env channel.Envelope) {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var hdr [4]byte
	hdr[0] = byte(len(raw) >> 24)
	hdr[1] = byte(len(raw) >> 16)
	hdr[2] = byte(len(raw) >> 8)
	hdr[3] = byte(len(raw))
	if _, err := c.ctrl.Write(hdr[:]); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := c.ctrl.Write(raw); err != nil {
		t.Fatalf("write body: %v", err)
	}
}

func (c *wtClient) recv(t *testing.T) channel.Envelope {
	t.Helper()
	_ = c.ctrl.SetReadDeadline(time.Now().Add(15 * time.Second))
	var hdr [4]byte
	if _, err := io.ReadFull(c.ctrl, hdr[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	n := int(hdr[0])<<24 | int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.ctrl, buf); err != nil {
		t.Fatalf("read body: %v", err)
	}
	var env channel.Envelope
	if err := json.Unmarshal(buf, &env); err != nil {
		t.Fatalf("unmarshal %s: %v", buf, err)
	}
	return env
}

func wtManifest() registry.Manifest {
	m := registry.Manifest{
		ID: "example/logical/over-quic", Version: "1.0.0", Protocol: "1",
		Name: "over quic", Description: "a skill that arrived over WebTransport",
		Capability: "logical.overquic", Type: "logical", Format: "source",
	}
	m.Ports.Ingress = []registry.Port{{Name: "text_in", Schema: "std/text@1"}}
	m.Ports.Egress = []registry.Port{{Name: "text_out", Schema: "std/text@1"}}
	return m
}

func TestSkillRegistersOverWebTransport(t *testing.T) {
	g, _ := testGateway(t)
	addr := wtNode(t, g)
	c := dialWT(t, addr)

	payload, _ := json.Marshal(wtManifest())
	c.send(t, channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(),
		Kind: channel.KindRegister, Payload: payload,
	})

	ack := c.recv(t)
	if ack.Kind != channel.KindStatus {
		t.Fatalf("registration answered %s, want status: %s", ack.Kind, ack.Payload)
	}
	var body struct {
		State string `json:"state"`
		Skill string `json:"skill"`
	}
	if err := json.Unmarshal(ack.Payload, &body); err != nil {
		t.Fatalf("ack payload: %v", err)
	}
	if body.State != "registered" || body.Skill != "example/logical/over-quic" {
		t.Fatalf("ack = %+v", body)
	}

	// The registry must not be able to tell which transport carried it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range g.Reg.Catalog() {
			if m.Capability == "logical.overquic" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the skill never appeared in the live catalogue")
}

// A malformed manifest is refused the same way over either transport. The
// handshake is one implementation, so this is really checking that the refusal
// still reaches a QUIC peer rather than being written into a void.
func TestBadManifestIsRefusedOverWebTransport(t *testing.T) {
	g, _ := testGateway(t)
	addr := wtNode(t, g)
	c := dialWT(t, addr)

	bad := wtManifest()
	bad.Capability = "logical.mismatch"
	bad.Type = "motor" // capability must carry the type prefix
	payload, _ := json.Marshal(bad)
	c.send(t, channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(),
		Kind: channel.KindRegister, Payload: payload,
	})

	resp := c.recv(t)
	if resp.Kind != channel.KindError {
		t.Fatalf("a rejected manifest answered %s, want error", resp.Kind)
	}
}

// The first frame has to be a register envelope, over QUIC as over TCP.
func TestFirstFrameMustRegisterOverWebTransport(t *testing.T) {
	g, _ := testGateway(t)
	addr := wtNode(t, g)
	c := dialWT(t, addr)

	c.send(t, channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(),
		Kind: channel.KindData, Schema: "std/text@1", Payload: []byte(`{"text":"hi"}`),
	})
	resp := c.recv(t)
	if resp.Kind != channel.KindError {
		t.Fatalf("a data-first connection answered %s, want error", resp.Kind)
	}
}
