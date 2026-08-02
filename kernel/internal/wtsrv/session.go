// Package wtsrv carries the channel protocol over WebTransport (HTTP/3, QUIC).
//
// # Why this exists
//
// C3 declares three QoS classes and the kernel has always honoured them — but
// only as far as a TCP socket lets anything honour them. `sendRealtime` in the
// WebSocket writer drops the oldest queued frame under pressure, which is the
// right behaviour for a *slow consumer* and does nothing at all for a *lossy
// network*: once a frame is handed to TCP, TCP retransmits it and delivers
// everything in order. One lost packet stalls every fresh frame queued behind
// it. A voice channel that must not be stalled by a slow consumer was still
// being stalled by a dropped packet, one layer down, where the application
// could not see it.
//
// QUIC is the layer where that is fixable, because it is the layer that owns
// loss. Streams are independent — a lost packet stalls its own stream and
// nothing else — and datagrams are not retransmitted at all. So the three QoS
// classes stop being a promise the transport quietly breaks and become three
// different transport primitives:
//
//	reliable  → one long-lived bidirectional stream, ordered.
//	            C3 requires FIFO per edge, and a single ordered stream is what
//	            gives it. Head-of-line blocking *within* this lane is correct:
//	            that is what "reliable" asked for.
//	realtime  → a datagram when the frame fits in one, otherwise a stream of
//	            its own that is closed immediately. Either way the frame cannot
//	            block, or be blocked by, any other frame. Staleness is then a
//	            question the receiver answers from `seq`, which is where it
//	            belongs — the network no longer forces an answer by stalling.
//	bulk      → its own unidirectional stream per transfer. C3 has named `bulk`
//	            without defining it since the beginning, for the honest reason
//	            that over one TCP socket there was nothing to define: a large
//	            transfer and a small one shared a queue either way. Here there
//	            is something to define, and this is it — a bulk transfer gets a
//	            stream nobody else is waiting on.
//
// # What this does not change
//
// The envelope, the executor, the ledger, the gate — none of it. This is a
// transport, and the point of the C3 contract is that the same envelopes travel
// over whatever carries them. A node speaks WebSocket and WebTransport at once
// and a graph cannot tell which one a skill arrived on.
package wtsrv

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"aura/kernel/internal/channel"
)

// maxFrame bounds one envelope on a stream. The same 8 MiB the HTTP surface
// uses, for the same reason: a peer is untrusted input and a length prefix it
// controls is an allocation it controls.
const maxFrame = 8 << 20

// realtimeLane is how many frames may queue on the realtime path before the
// oldest is dropped. Mirrors the WebSocket writer so the two transports behave
// the same under a slow consumer; the difference between them is loss, not
// backpressure.
const realtimeLane = 8

// Session is one WebTransport connection carrying envelopes.
//
// It is deliberately the same shape the WebSocket path already exposes — Send
// takes the QoS the edge declared, Recv hands back one whole envelope — so the
// gateway can host either without knowing which it has.
type Session struct {
	sess *webtransport.Session
	ctrl *webtransport.Stream // the reliable lane

	writeMu sync.Mutex // one writer per stream, as QUIC requires

	in     chan []byte
	closed chan struct{}
	once   sync.Once

	// rt is the realtime queue. It exists for the same reason the WebSocket
	// one does — a producer of audio must not block on a slow consumer — but
	// here it is only about consumer speed, because the network can no longer
	// contribute a stall.
	rt chan []byte

	log logf
}

type logf func(msg string, args ...any)

// newSession wires a session and starts its readers.
func newSession(sess *webtransport.Session, ctrl *webtransport.Stream, log logf) *Session {
	if log == nil {
		log = func(string, ...any) {}
	}
	s := &Session{
		sess: sess, ctrl: ctrl,
		in:     make(chan []byte, 64),
		closed: make(chan struct{}),
		rt:     make(chan []byte, realtimeLane),
		log:    log,
	}
	go s.readCtrl()
	go s.readUniStreams()
	go s.readDatagrams()
	go s.writeRealtime()
	return s
}

// Recv returns the next envelope, whichever lane carried it.
//
// Lanes are merged deliberately: ordering *between* QoS classes was never
// promised — C3 orders an edge, and an edge has one class — so a caller that
// had to drain three queues would be reimplementing this loop with more bugs.
func (s *Session) Recv() ([]byte, error) {
	select {
	case raw, ok := <-s.in:
		if !ok {
			return nil, io.EOF
		}
		return raw, nil
	case <-s.closed:
		return nil, io.EOF
	}
}

// Send routes one envelope onto the lane its edge declared.
func (s *Session) Send(raw []byte, qos string) error {
	select {
	case <-s.closed:
		return errors.New("session closed")
	default:
	}
	switch qos {
	case channel.QoSRealtime:
		return s.sendRealtime(raw)
	case channel.QoSBulk:
		return s.sendBulk(raw)
	default:
		return s.sendReliable(raw)
	}
}

// sendReliable writes to the one ordered stream. FIFO per edge (C3 rule 4)
// comes from the stream itself rather than from anything this code does.
func (s *Session) sendReliable(raw []byte) error {
	if len(raw) > maxFrame {
		return fmt.Errorf("envelope of %d bytes exceeds the %d-byte frame limit", len(raw), maxFrame)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeFrame(s.ctrl, raw)
}

// sendBulk opens a stream nobody else is waiting on.
//
// This is what `bulk` means here, and it is the first time it has meant
// anything: a large transfer no longer shares a queue with the control lane, so
// it cannot delay an approval request or a `done` behind it.
func (s *Session) sendBulk(raw []byte) error {
	str, err := s.sess.OpenUniStream()
	if err != nil {
		return fmt.Errorf("open bulk stream: %w", err)
	}
	go func() {
		defer str.Close()
		if err := writeFrame(str, raw); err != nil {
			s.log("wtsrv: bulk write failed", "err", err)
		}
	}()
	return nil
}

// sendRealtime never blocks and never reports failure, matching the WebSocket
// lane's contract. Under a slow consumer it drops the OLDEST queued frame,
// because in a live stream the stale frame is the worthless one.
func (s *Session) sendRealtime(raw []byte) error {
	for attempt := 0; attempt <= realtimeLane; attempt++ {
		select {
		case s.rt <- raw:
			return nil
		case <-s.closed:
			return errors.New("session closed")
		default:
		}
		select { // make room by discarding the oldest
		case <-s.rt:
		default:
		}
	}
	return nil
}

// writeRealtime drains the realtime queue onto the wire.
//
// A datagram is tried first and a stream is the fallback, decided by the error
// rather than by measuring the path MTU: quic-go answers DatagramTooLargeError
// with the size that would have fit, which is more trustworthy than any number
// this package could compute, and it stays correct when the path changes
// mid-session.
func (s *Session) writeRealtime() {
	for {
		select {
		case <-s.closed:
			return
		case raw := <-s.rt:
			if err := s.sess.SendDatagram(raw); err == nil {
				continue
			} else {
				var tooLarge *quic.DatagramTooLargeError
				if !errors.As(err, &tooLarge) {
					// Datagrams unsupported or the session is going away.
					// A stream still delivers the frame without blocking the
					// others, which is the property that matters.
					s.log("wtsrv: datagram refused, falling back to a stream", "err", err)
				}
			}
			str, err := s.sess.OpenUniStream()
			if err != nil {
				s.log("wtsrv: realtime stream refused", "err", err)
				continue
			}
			go func(str *webtransport.SendStream, raw []byte) {
				defer str.Close()
				_ = writeFrame(str, raw)
			}(str, raw)
		}
	}
}

// ── receive ─────────────────────────────────────────────────────────

func (s *Session) readCtrl() {
	defer s.Close()
	for {
		raw, err := readFrame(s.ctrl)
		if err != nil {
			return
		}
		if !s.deliver(raw) {
			return
		}
	}
}

// readUniStreams accepts the streams that carry one frame each — realtime
// frames too large for a datagram, and bulk transfers.
func (s *Session) readUniStreams() {
	for {
		str, err := s.sess.AcceptUniStream(context.Background())
		if err != nil {
			return
		}
		go func(str *webtransport.ReceiveStream) {
			raw, err := readFrame(str)
			if err != nil {
				return
			}
			s.deliver(raw)
		}(str)
	}
}

func (s *Session) readDatagrams() {
	for {
		raw, err := s.sess.ReceiveDatagram(context.Background())
		if err != nil {
			return
		}
		if !s.deliver(raw) {
			return
		}
	}
}

// deliver hands one envelope up, reporting whether the session is still live.
func (s *Session) deliver(raw []byte) bool {
	select {
	case s.in <- raw:
		return true
	case <-s.closed:
		return false
	case <-time.After(30 * time.Second):
		// A consumer this far behind is not going to catch up, and holding the
		// frame would leak the reader goroutine for the life of the session.
		s.log("wtsrv: dropping a frame nobody read for 30s")
		return true
	}
}

// closeGrace is how long a closing session keeps the connection alive after
// closing its send side.
//
// It exists because of a failure that only shows up on QUIC: the last thing a
// refused peer is sent is an error explaining the refusal, and tearing the
// connection down in the same breath discards it. The peer then sees the
// connection drop with no reason, which is the least useful way to be told
// something was wrong. TCP hid this — a gorilla writer had already flushed by
// the time the handler returned.
const closeGrace = 250 * time.Millisecond

// Close ends the session. Safe to call more than once.
func (s *Session) Close() error {
	s.once.Do(func() {
		close(s.closed)
		// Send side first: this FINs the control stream and lets everything
		// already written drain.
		_ = s.ctrl.Close()
		go func() {
			time.Sleep(closeGrace)
			_ = s.sess.CloseWithError(0, "")
		}()
	})
	return nil
}

// Context ends when the session does, for callers that want to select on it.
func (s *Session) Context() <-chan struct{} { return s.closed }

// RemoteAddr reports the peer, for the same logging and liveness bookkeeping
// the WebSocket path does.
func (s *Session) RemoteAddr() string {
	if s.sess == nil {
		return ""
	}
	return s.sess.RemoteAddr().String()
}

// ── framing ─────────────────────────────────────────────────────────
//
// A QUIC stream is a byte stream, so envelopes need a boundary. Four bytes of
// big-endian length: enough for the 8 MiB cap, and cheap to read. Datagrams
// need none — a datagram is already exactly one message, which is the whole
// reason the realtime lane prefers them.

func writeFrame(w io.Writer, raw []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(raw)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(raw)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return nil, fmt.Errorf("frame of %d bytes exceeds the %d-byte limit", n, maxFrame)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
