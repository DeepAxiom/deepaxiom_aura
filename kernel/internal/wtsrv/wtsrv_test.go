package wtsrv

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"aura/kernel/internal/channel"
)

// These tests drive a real QUIC listener with a real QUIC client. Nothing here
// is mocked, because the claim being made is about the transport itself: that
// the three QoS classes reach the wire as three different primitives, and that
// a frame on one lane cannot be blocked by a frame on another.

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// serve starts a node on a free UDP port and returns its address.
func serve(t *testing.T, routes map[string]Handler) (*Server, string) {
	t.Helper()
	s := &Server{
		Addr:    "127.0.0.1:0",
		DataDir: t.TempDir(),
		Log:     testLogger(),
		Routes:  routes,
	}
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, s.LocalAddr()
}

// dial connects a client and opens the control stream, the way a real peer
// does.
func dial(t *testing.T, addr, path string) (*webtransport.Session, *webtransport.Stream) {
	t.Helper()
	// The certificate is self-signed and freshly minted per test; skipping
	// verification here is testing the transport, not the trust model.
	d := &webtransport.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{http3.NextProtoH3}},
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	t.Cleanup(func() { _ = d.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, sess, err := d.Dial(ctx, fmt.Sprintf("https://%s%s", addr, path), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ctrl, err := sess.OpenStream()
	if err != nil {
		t.Fatalf("open control stream: %v", err)
	}
	// A stream only exists for the peer once something is written on it.
	if err := writeFrame(ctrl, []byte(`{"kind":"hello"}`)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	t.Cleanup(func() { _ = sess.CloseWithError(0, "") })
	return sess, ctrl
}

func TestEchoOverTheReliableLane(t *testing.T) {
	got := make(chan []byte, 8)
	_, addr := serve(t, map[string]Handler{
		"/ws/skill": func(_ context.Context, s *Session, _ *http.Request) {
			for {
				raw, err := s.Recv()
				if err != nil {
					return
				}
				got <- raw
				if err := s.Send(raw, channel.QoSReliable); err != nil {
					return
				}
			}
		},
	})

	_, ctrl := dial(t, addr, "/ws/skill")
	select {
	case raw := <-got:
		if string(raw) != `{"kind":"hello"}` {
			t.Fatalf("server saw %s", raw)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server never received the hello")
	}
	reply, err := readFrame(ctrl)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if string(reply) != `{"kind":"hello"}` {
		t.Errorf("reply = %s", reply)
	}
}

// C3 requires FIFO per edge. `reliable` is the lane that promises it, and one
// ordered QUIC stream is what delivers it.
func TestReliableLaneIsFIFO(t *testing.T) {
	const n = 50
	_, addr := serve(t, map[string]Handler{
		"/ws/skill": func(_ context.Context, s *Session, _ *http.Request) {
			if _, err := s.Recv(); err != nil {
				return
			}
			for i := 0; i < n; i++ {
				if err := s.Send([]byte(fmt.Sprintf(`{"seq":%d}`, i)), channel.QoSReliable); err != nil {
					return
				}
			}
			<-s.Context()
		},
	})

	_, ctrl := dial(t, addr, "/ws/skill")
	for i := 0; i < n; i++ {
		raw, err := readFrame(ctrl)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		var env struct {
			Seq int `json:"seq"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if env.Seq != i {
			t.Fatalf("reliable lane delivered %d at position %d — FIFO broken", env.Seq, i)
		}
	}
}

// The whole point of the change: a realtime frame travels as a datagram, which
// is a primitive TCP does not have and cannot emulate.
func TestRealtimeFrameArrivesAsADatagram(t *testing.T) {
	_, addr := serve(t, map[string]Handler{
		"/v1/stream": func(_ context.Context, s *Session, _ *http.Request) {
			if _, err := s.Recv(); err != nil {
				return
			}
			for i := 0; i < 5; i++ {
				_ = s.Send([]byte(fmt.Sprintf(`{"frame":%d}`, i)), channel.QoSRealtime)
				time.Sleep(20 * time.Millisecond)
			}
			<-s.Context()
		},
	})

	sess, _ := dial(t, addr, "/v1/stream")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := sess.ReceiveDatagram(ctx)
	if err != nil {
		t.Fatalf("no datagram arrived: %v", err)
	}
	var env struct {
		Frame int `json:"frame"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("datagram payload: %s", raw)
	}
}

// A realtime frame too large for a datagram must still not block the reliable
// lane. It falls back to a stream of its own, which is independent.
func TestOversizedRealtimeFrameFallsBackToItsOwnStream(t *testing.T) {
	big := make([]byte, 64<<10) // far beyond any datagram
	for i := range big {
		big[i] = 'a'
	}
	payload, _ := json.Marshal(map[string]string{"audio": string(big)})

	_, addr := serve(t, map[string]Handler{
		"/v1/stream": func(_ context.Context, s *Session, _ *http.Request) {
			if _, err := s.Recv(); err != nil {
				return
			}
			_ = s.Send(payload, channel.QoSRealtime)
			<-s.Context()
		},
	})

	sess, _ := dial(t, addr, "/v1/stream")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	str, err := sess.AcceptUniStream(ctx)
	if err != nil {
		t.Fatalf("no fallback stream: %v", err)
	}
	raw, err := readFrame(str)
	if err != nil {
		t.Fatalf("read fallback stream: %v", err)
	}
	if len(raw) != len(payload) {
		t.Errorf("fallback delivered %d bytes, want %d", len(raw), len(payload))
	}
}

// `bulk` had no defined behaviour because over one TCP socket there was nothing
// to define. Here it is a stream of its own, so a large transfer cannot delay
// the control lane behind it.
func TestBulkTravelsOnItsOwnStreamAndDoesNotDelayReliable(t *testing.T) {
	blob := make([]byte, 512<<10)
	_, addr := serve(t, map[string]Handler{
		"/v1/stream": func(_ context.Context, s *Session, _ *http.Request) {
			if _, err := s.Recv(); err != nil {
				return
			}
			payload, _ := json.Marshal(map[string]string{"blob": string(blob)})
			_ = s.Send(payload, channel.QoSBulk)
			// Sent immediately after the bulk transfer starts. On one shared
			// queue this would wait behind half a megabyte.
			_ = s.Send([]byte(`{"kind":"done"}`), channel.QoSReliable)
			<-s.Context()
		},
	})

	_, ctrl := dial(t, addr, "/v1/stream")
	start := time.Now()
	raw, err := readFrame(ctrl)
	if err != nil {
		t.Fatalf("control lane: %v", err)
	}
	elapsed := time.Since(start)
	if string(raw) != `{"kind":"done"}` {
		t.Fatalf("control lane carried %s", raw)
	}
	// Generous, because CI machines are not fast — the point is that it did not
	// have to wait for the whole blob.
	if elapsed > 5*time.Second {
		t.Errorf("the terminal envelope waited %v behind the bulk transfer", elapsed)
	}
}

// A slow consumer must not stall a producer of audio. Same contract the
// WebSocket lane has; here it is about consumer speed only, since the network
// can no longer contribute a stall.
func TestRealtimeNeverBlocksTheProducer(t *testing.T) {
	sent := make(chan struct{})
	_, addr := serve(t, map[string]Handler{
		"/v1/stream": func(_ context.Context, s *Session, _ *http.Request) {
			if _, err := s.Recv(); err != nil {
				return
			}
			for i := 0; i < 500; i++ { // far more than the lane depth
				if err := s.Send([]byte(fmt.Sprintf(`{"f":%d}`, i)), channel.QoSRealtime); err != nil {
					return
				}
			}
			close(sent)
			<-s.Context()
		},
	})

	dial(t, addr, "/v1/stream") // deliberately never reads
	select {
	case <-sent:
	case <-time.After(15 * time.Second):
		t.Fatal("the producer blocked on a consumer that never read")
	}
}

func TestCertificateIsReusedAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	a, err := loadOrCreateCert(dir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := loadOrCreateCert(dir)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if fingerprint(a.Certificate[0]) != fingerprint(b.Certificate[0]) {
		t.Error("a restart minted a new certificate; a pinned client would break on every restart")
	}
}

// WebTransport only lets a browser accept a self-signed certificate if it is
// short-lived, so this bound is load-bearing rather than cosmetic.
func TestCertificateIsShortLivedEnoughForBrowsers(t *testing.T) {
	cert, err := newCert()
	if err != nil {
		t.Fatalf("newCert: %v", err)
	}
	life := time.Until(cert.Leaf.NotAfter)
	if life > 14*24*time.Hour {
		t.Errorf("certificate valid for %v; browsers refuse a self-signed one beyond 14 days", life)
	}
	if life < 24*time.Hour {
		t.Errorf("certificate valid for only %v; it would expire faster than a node runs", life)
	}
}

func TestFingerprintIsTheUsualColonHex(t *testing.T) {
	fp := fingerprint([]byte("x"))
	if len(fp) != 32*3-1 {
		t.Fatalf("fingerprint has length %d: %s", len(fp), fp)
	}
	for i := 2; i < len(fp); i += 3 {
		if fp[i] != ':' {
			t.Fatalf("expected colon separators: %s", fp)
		}
	}
}

func TestFrameRoundTripAndLimit(t *testing.T) {
	var buf syncBuf
	if err := writeFrame(&buf, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readFrame(&buf)
	if err != nil || string(got) != "hello" {
		t.Fatalf("round trip: %q %v", got, err)
	}
	// An oversized length prefix is a peer-controlled allocation; it has to be
	// refused rather than honoured.
	var big syncBuf
	big.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	if _, err := readFrame(&big); err == nil {
		t.Error("a 4 GiB length prefix must be refused")
	}
}

type syncBuf struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuf) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}
