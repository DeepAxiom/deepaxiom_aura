// Package rtmp is the ingest door for live: enough of RTMP to accept one
// publisher per connection and hand its audio and video on as FLV tags.
//
// It is here, in a few hundred lines, instead of being ffmpeg's own
// `-listen 1` or a library, for three reasons that are not taste.
//
// ffmpeg's listener does not check who is publishing. It logs a warning when
// the stream name is not the one it expected and carries on, so a stream key
// would be a secret nothing enforces. It also wants a port per stream, and the
// key would have to be on its command line, where `ps` shows it to every user
// on the machine.
//
// A general RTMP library brings playback, relaying and a codec layer this
// service has no use for, into the one artefact in this repository whose
// reason for being separate is how many CVEs its dependencies attract.
//
// And what is needed is small: the handshake, the chunk stream, five
// commands, and three message types passed through untouched. Nothing here
// understands a codec. A tag is a type, a timestamp and bytes, and ffmpeg is
// what makes sense of them.
package rtmp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// FLV tag types, which are also the RTMP message type ids for the same things.
const (
	TagAudio  byte = 8
	TagVideo  byte = 9
	TagScript byte = 18
)

// Handler decides who may publish and receives what they send.
type Handler interface {
	// Publish is called once per connection, when the client names the stream
	// it wants to publish. app is the application part of the URL and name is
	// the stream name — which, for this service, is the stream key, and must
	// be treated as the secret it is. Returning an error refuses the publish;
	// the error's text is not sent to the client, which is told only that the
	// name was refused.
	Publish(ctx context.Context, app, name string, remote net.Addr) (Sink, error)
}

// Sink receives one publisher's stream.
type Sink interface {
	// Tag is one FLV tag body. An error ends the connection.
	Tag(kind byte, timestamp uint32, data []byte) error
	// Close says the publisher is gone. err is nil when it said goodbye and
	// the reason when the connection simply stopped.
	Close(err error)
}

// Limits a client cannot talk its way past. Each is far above anything a real
// encoder sends, and each is the size of the allocation a hostile one could
// otherwise ask for before it has proved it holds a key.
const (
	maxChunkSize   = 1 << 20
	maxMessageSize = 16 << 20
	maxChunkIDs    = 64
	// maxCommandSize bounds a message before a publish has been accepted:
	// commands are a few hundred bytes.
	maxCommandSize = 64 << 10
)

// Server accepts publishers.
type Server struct {
	Handler Handler
	Log     *slog.Logger
	// SetupTimeout bounds everything up to an accepted publish. A connection
	// that has not named a valid stream by then is closed.
	SetupTimeout time.Duration
	// IdleTimeout is how long a publisher may send nothing before it is
	// considered gone.
	IdleTimeout time.Duration
}

// Serve accepts connections until ctx is cancelled, then closes the listener
// and every connection and waits for their handlers.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns = map[net.Conn]struct{}{}
	)
	stop := context.AfterFunc(ctx, func() {
		ln.Close()
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
	})
	defer stop()
	defer wg.Wait()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return fmt.Errorf("rtmp accept: %w", err)
		}
		mu.Lock()
		conns[conn] = struct{}{}
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				conn.Close()
				mu.Lock()
				delete(conns, conn)
				mu.Unlock()
			}()
			s.serve(ctx, conn)
		}()
	}
}

func (s *Server) setupTimeout() time.Duration {
	if s.SetupTimeout > 0 {
		return s.SetupTimeout
	}
	return 10 * time.Second
}

func (s *Server) idleTimeout() time.Duration {
	if s.IdleTimeout > 0 {
		return s.IdleTimeout
	}
	return 15 * time.Second
}

// errGoodbye is how the read loop says the publisher ended the stream itself.
var errGoodbye = errors.New("the publisher ended the stream")

func (s *Server) serve(ctx context.Context, conn net.Conn) {
	// The remote address and nothing else: the stream name is a credential,
	// and this is the only place in the package that logs.
	log := s.Log.With("remote", conn.RemoteAddr().String())
	c := &session{
		conn:      conn,
		r:         bufio.NewReaderSize(conn, 64<<10),
		w:         bufio.NewWriter(conn),
		inChunk:   128,
		streams:   map[uint32]*chunkStream{},
		ackWindow: 2_500_000,
	}
	_ = conn.SetDeadline(time.Now().Add(s.setupTimeout()))
	if err := c.handshake(); err != nil {
		log.Debug("rtmp handshake failed", "error", err)
		return
	}

	var sink Sink
	// leaving is set once the publisher has started saying goodbye. An encoder
	// sends FCUnpublish, then deleteStream, then closes; hanging up on the
	// first would have it write the second into a closed socket and report a
	// failed broadcast for one that went perfectly well.
	leaving := false
	err := func() error {
		for {
			msg, err := c.readMessage(sink != nil)
			if err != nil {
				if leaving {
					return errGoodbye
				}
				return err
			}
			switch msg.typeID {
			case 1: // Set Chunk Size
				if len(msg.payload) < 4 {
					return errors.New("short Set Chunk Size")
				}
				size := binary.BigEndian.Uint32(msg.payload) & 0x7fffffff
				if size < 1 || size > maxChunkSize {
					return fmt.Errorf("chunk size %d is outside what this server accepts", size)
				}
				c.inChunk = int(size)
			case 2, 3, 4, 5, 6:
				// Abort, acknowledgement, user control, window size, peer
				// bandwidth: nothing a server that only receives needs to act on.
			case 20, 17: // AMF0 command; AMF3 command is the same after one byte
				body := msg.payload
				if msg.typeID == 17 && len(body) > 0 {
					body = body[1:]
				}
				done, opened, err := s.command(ctx, c, msg.streamID, body, sink)
				if err != nil {
					return err
				}
				if opened != nil {
					sink = opened
					log.Info("rtmp publisher accepted")
				}
				switch done {
				case goodbyeStarted:
					leaving = true
				case goodbyeDone:
					return errGoodbye
				}
			case TagAudio, TagVideo, TagScript, 15:
				if sink == nil {
					return errors.New("media before a publish was accepted")
				}
				kind, data := msg.typeID, msg.payload
				if kind == 15 {
					// AMF3 data: a leading format byte, then the same AMF0.
					if len(data) > 0 {
						data = data[1:]
					}
					kind = TagScript
				}
				if kind == TagScript {
					data = stripSetDataFrame(data)
				}
				if len(data) == 0 {
					continue
				}
				if err := sink.Tag(kind, msg.timestamp, data); err != nil {
					return err
				}
			}
			if sink != nil {
				// Published: from here the only deadline is silence.
				_ = conn.SetDeadline(time.Now().Add(s.idleTimeout()))
			}
		}
	}()

	if sink != nil {
		if errors.Is(err, errGoodbye) {
			sink.Close(nil)
			log.Info("rtmp publisher finished")
		} else {
			sink.Close(err)
			log.Info("rtmp publisher disconnected", "error", err)
		}
		return
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, errGoodbye) {
		log.Debug("rtmp connection closed before publishing", "error", err)
	}
}

// stripSetDataFrame removes the "@setDataFrame" marker publishers put in front
// of onMetaData. It is an instruction to a server that stores metadata, not
// part of the metadata, and an FLV script tag that begins with it is one
// ffmpeg's demuxer does not recognise.
func stripSetDataFrame(data []byte) []byte {
	const marker = "@setDataFrame"
	if len(data) >= 3+len(marker) && data[0] == amfString &&
		int(binary.BigEndian.Uint16(data[1:3])) == len(marker) && string(data[3:3+len(marker)]) == marker {
		return data[3+len(marker):]
	}
	return data
}

// How far through leaving a publisher is, as command reports it.
const (
	goodbyeNone = iota
	goodbyeStarted
	goodbyeDone
)

// command handles one AMF0 command. It returns how far the publisher is
// through saying goodbye, and the sink if this command was an accepted
// publish.
func (s *Server) command(ctx context.Context, c *session, streamID uint32, body []byte, sink Sink) (int, Sink, error) {
	values, err := amfDecode(body)
	if len(values) < 1 {
		return goodbyeNone, nil, fmt.Errorf("unreadable command: %w", err)
	}
	// A command whose tail did not parse is still acted on by its head: the
	// name, transaction and first arguments are what matter, and encoders are
	// inventive about what they append.
	name, _ := values[0].(string)
	txn := 0.0
	if len(values) > 1 {
		txn, _ = values[1].(float64)
	}
	switch name {
	case "connect":
		if len(values) > 2 {
			if obj, ok := values[2].(map[string]any); ok {
				c.app, _ = obj["app"].(string)
			}
		}
		if err := c.writeControl(5, be32(2_500_000)); err != nil { // Window Acknowledgement Size
			return goodbyeNone, nil, err
		}
		if err := c.writeControl(6, append(be32(2_500_000), 2)); err != nil { // Set Peer Bandwidth, dynamic
			return goodbyeNone, nil, err
		}
		// Raise our own chunk size before saying anything long, so every
		// reply fits one chunk and nothing here has to split a message.
		if err := c.writeControl(1, be32(outChunkSize)); err != nil {
			return goodbyeNone, nil, err
		}
		return goodbyeNone, nil, c.writeCommand(0, "_result", txn,
			[]amfField{{"fmsVer", "FMS/3,0,1,123"}, {"capabilities", 31.0}},
			[]amfField{
				{"level", "status"},
				{"code", "NetConnection.Connect.Success"},
				{"description", "Connection succeeded."},
				{"objectEncoding", 0.0},
			})
	case "createStream":
		return goodbyeNone, nil, c.writeCommand(0, "_result", txn, nil, 1.0)
	case "releaseStream", "FCPublish":
		// Flash Media Server rituals. Encoders send them and some wait for an
		// answer; the answer carries nothing.
		return goodbyeNone, nil, c.writeCommand(0, "_result", txn, nil, nil)
	case "publish":
		if sink != nil {
			return goodbyeNone, nil, errors.New("a second publish on one connection")
		}
		key := ""
		if len(values) > 3 {
			key, _ = values[3].(string)
		}
		// Encoders append their own query string to the name; it is not part
		// of the key.
		key, _, _ = strings.Cut(key, "?")
		app := strings.Trim(c.app, "/")
		opened, perr := s.Handler.Publish(ctx, app, key, c.conn.RemoteAddr())
		if perr != nil {
			// One answer for every refusal — no such stream, wrong key, stream
			// already over — so the reply cannot be used to find out which
			// stream ids exist.
			_ = c.writeCommand(streamID, "onStatus", 0.0, nil, []amfField{
				{"level", "error"},
				{"code", "NetStream.Publish.BadName"},
				{"description", "Publishing was refused."},
			})
			return goodbyeNone, nil, fmt.Errorf("publish refused: %w", perr)
		}
		// Stream Begin, then the status every encoder waits for before it
		// sends a frame.
		if err := c.writeControl(4, append([]byte{0, 0}, be32(streamID)...)); err != nil {
			opened.Close(err)
			return goodbyeNone, nil, err
		}
		if err := c.writeCommand(streamID, "onStatus", 0.0, nil, []amfField{
			{"level", "status"},
			{"code", "NetStream.Publish.Start"},
			{"description", "Publishing."},
		}); err != nil {
			opened.Close(err)
			return goodbyeNone, nil, err
		}
		return goodbyeNone, opened, nil
	case "FCUnpublish":
		if sink != nil {
			return goodbyeStarted, nil, nil
		}
	case "deleteStream", "closeStream":
		if sink != nil {
			return goodbyeDone, nil, nil
		}
	}
	return goodbyeNone, nil, nil
}

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// outChunkSize is the chunk size this server announces for what it sends.
// Every message it writes is smaller, so none is ever split.
const outChunkSize = 4096

// session is one connection's protocol state.
type session struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer

	app     string
	inChunk int
	streams map[uint32]*chunkStream

	// Acknowledgements: some encoders stop sending when the bytes they have
	// sent since the last acknowledgement exceed the window they were told.
	received  uint64
	acked     uint64
	ackWindow uint64
}

// chunkStream is the reassembly state of one chunk stream id.
type chunkStream struct {
	timestamp uint32
	// field is the last timestamp field seen: the absolute time of a type 0
	// header or the delta of a type 1 or 2. A type 3 header that starts a new
	// message repeats it as a delta.
	field    uint32
	extended bool
	length   int
	typeID   byte
	streamID uint32
	buf      []byte
	seen     bool
}

type message struct {
	typeID    byte
	streamID  uint32
	timestamp uint32
	payload   []byte
}

// handshake is the plain RTMP handshake: version 3, no digest, no encryption.
// Every encoder in use falls back to it or starts with it.
func (c *session) handshake() error {
	var c0c1 [1 + 1536]byte
	if _, err := io.ReadFull(c.r, c0c1[:]); err != nil {
		return err
	}
	if c0c1[0] != 3 {
		return fmt.Errorf("RTMP version %d; only plain RTMP (3) is spoken here", c0c1[0])
	}
	var s0s1 [1 + 1536]byte
	s0s1[0] = 3
	// Bytes 1..8 stay zero: time, and the zero field that tells the client
	// this is the plain handshake rather than the digest one.
	if _, err := rand.Read(s0s1[9:]); err != nil {
		return err
	}
	if _, err := c.w.Write(s0s1[:]); err != nil {
		return err
	}
	// S2 is C1 echoed.
	if _, err := c.w.Write(c0c1[1:]); err != nil {
		return err
	}
	if err := c.w.Flush(); err != nil {
		return err
	}
	// C2 is S1 echoed. It is read and not compared: a client that got this far
	// has proved nothing either way, and the stream key is the credential.
	var c2 [1536]byte
	_, err := io.ReadFull(c.r, c2[:])
	return err
}

func (c *session) read(buf []byte) error {
	n, err := io.ReadFull(c.r, buf)
	c.received += uint64(n)
	return err
}

// readMessage reads chunks until one message is complete. large says whether
// a publish has been accepted; before that, only command-sized messages are
// taken.
func (c *session) readMessage(large bool) (message, error) {
	for {
		var first [1]byte
		if err := c.read(first[:]); err != nil {
			return message{}, err
		}
		format := first[0] >> 6
		csid := uint32(first[0] & 0x3f)
		switch csid {
		case 0:
			var b [1]byte
			if err := c.read(b[:]); err != nil {
				return message{}, err
			}
			csid = uint32(b[0]) + 64
		case 1:
			var b [2]byte
			if err := c.read(b[:]); err != nil {
				return message{}, err
			}
			csid = uint32(b[0]) + uint32(b[1])<<8 + 64
		}
		cs := c.streams[csid]
		if cs == nil {
			if len(c.streams) >= maxChunkIDs {
				return message{}, errors.New("too many chunk streams")
			}
			cs = &chunkStream{}
			c.streams[csid] = cs
		}
		if format != 0 && !cs.seen {
			return message{}, errors.New("a chunk continues a stream that never began")
		}

		starting := len(cs.buf) == 0
		var header [11]byte
		switch format {
		case 0:
			if err := c.read(header[:11]); err != nil {
				return message{}, err
			}
			cs.field = be24(header[0:3])
			cs.length = int(be24(header[3:6]))
			cs.typeID = header[6]
			cs.streamID = binary.LittleEndian.Uint32(header[7:11])
		case 1:
			if err := c.read(header[:7]); err != nil {
				return message{}, err
			}
			cs.field = be24(header[0:3])
			cs.length = int(be24(header[3:6]))
			cs.typeID = header[6]
		case 2:
			if err := c.read(header[:3]); err != nil {
				return message{}, err
			}
			cs.field = be24(header[0:3])
		}
		if format != 3 {
			if !starting {
				return message{}, errors.New("a new message began inside an unfinished one")
			}
			cs.extended = cs.field == 0xffffff
		}
		if cs.extended {
			// Present on every chunk of the message, including the type 3
			// continuations — which is not what the specification says and is
			// what every implementation does.
			var ext [4]byte
			if err := c.read(ext[:]); err != nil {
				return message{}, err
			}
			if format != 3 {
				cs.field = binary.BigEndian.Uint32(ext[:])
			}
		}
		if starting {
			switch format {
			case 0:
				cs.timestamp = cs.field
			default:
				// Types 1 and 2 carry a delta; type 3 starting a message
				// repeats the last one.
				cs.timestamp += cs.field
			}
			cs.seen = true
			limit := maxCommandSize
			if large {
				limit = maxMessageSize
			}
			if cs.length > limit {
				return message{}, fmt.Errorf("a %d byte message is larger than this server accepts here", cs.length)
			}
		}

		want := cs.length - len(cs.buf)
		if want > c.inChunk {
			want = c.inChunk
		}
		if want > 0 {
			start := len(cs.buf)
			cs.buf = append(cs.buf, make([]byte, want)...)
			if err := c.read(cs.buf[start:]); err != nil {
				return message{}, err
			}
		}
		if err := c.acknowledge(); err != nil {
			return message{}, err
		}
		if len(cs.buf) < cs.length {
			continue
		}
		msg := message{typeID: cs.typeID, streamID: cs.streamID, timestamp: cs.timestamp, payload: cs.buf}
		// A new buffer for the next message rather than this one truncated:
		// the payload has been handed on and may still be in someone's queue.
		cs.buf = nil
		return msg, nil
	}
}

func be24(b []byte) uint32 { return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]) }

func (c *session) acknowledge() error {
	if c.received-c.acked < c.ackWindow/2 {
		return nil
	}
	c.acked = c.received
	return c.writeControl(3, be32(uint32(c.received)))
}

// writeControl sends a protocol control message: chunk stream 2, stream 0.
func (c *session) writeControl(typeID byte, payload []byte) error {
	return c.writeMessage(2, typeID, 0, payload)
}

// writeCommand sends an AMF0 command on chunk stream 3.
func (c *session) writeCommand(streamID uint32, values ...any) error {
	return c.writeMessage(3, 20, streamID, amfEncode(values...))
}

func (c *session) writeMessage(csid, typeID byte, streamID uint32, payload []byte) error {
	if len(payload) > outChunkSize {
		return errors.New("reply larger than one chunk")
	}
	header := make([]byte, 12, 12+len(payload))
	header[0] = csid // format 0
	// Timestamp stays zero: nothing this server sends is timed.
	header[4] = byte(len(payload) >> 16)
	header[5] = byte(len(payload) >> 8)
	header[6] = byte(len(payload))
	header[7] = typeID
	binary.LittleEndian.PutUint32(header[8:12], streamID)
	if _, err := c.w.Write(append(header, payload...)); err != nil {
		return err
	}
	return c.w.Flush()
}
