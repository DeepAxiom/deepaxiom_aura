package rtmp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- a handler that records ------------------------------------------------

type tag struct {
	kind byte
	ts   uint32
	data []byte
}

type recorder struct {
	mu       sync.Mutex
	app      string
	name     string
	refuse   error
	tags     []tag
	closed   chan error
	publishN int
}

func newRecorder() *recorder { return &recorder{closed: make(chan error, 4)} }

func (r *recorder) Publish(_ context.Context, app, name string, _ net.Addr) (Sink, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.publishN++
	r.app, r.name = app, name
	if r.refuse != nil {
		return nil, r.refuse
	}
	return r, nil
}

func (r *recorder) Tag(kind byte, ts uint32, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tags = append(r.tags, tag{kind, ts, append([]byte(nil), data...)})
	return nil
}

func (r *recorder) Close(err error) { r.closed <- err }

func (r *recorder) snapshot() []tag {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tag(nil), r.tags...)
}

// logBuffer collects what the server logs. Locked, because the server writes
// to it from its own goroutines while the test reads it.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// --- a client: just enough of an encoder to drive the server -----------------

type client struct {
	t         *testing.T
	conn      net.Conn
	chunkSize int
	// last timestamp sent per chunk stream, for delta headers
	lastTs map[byte]uint32
	lastD  map[byte]uint32
}

func serve(t *testing.T, h Handler, logs io.Writer) (*client, context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if logs == nil {
		logs = io.Discard
	}
	srv := &Server{
		Handler:      h,
		Log:          slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		SetupTimeout: 3 * time.Second,
		IdleTimeout:  3 * time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, ln)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the server did not stop when its context was cancelled")
		}
	})
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	return &client{t: t, conn: conn, chunkSize: 128, lastTs: map[byte]uint32{}, lastD: map[byte]uint32{}}, cancel
}

func (c *client) handshake() {
	c.t.Helper()
	c0c1 := make([]byte, 1537)
	c0c1[0] = 3
	for i := 9; i < len(c0c1); i++ {
		c0c1[i] = byte(i)
	}
	if _, err := c.conn.Write(c0c1); err != nil {
		c.t.Fatal(err)
	}
	reply := make([]byte, 1+1536+1536)
	if _, err := io.ReadFull(c.conn, reply); err != nil {
		c.t.Fatalf("no handshake reply: %v", err)
	}
	if reply[0] != 3 {
		c.t.Fatalf("server version %d", reply[0])
	}
	// S2 must be our C1, echoed.
	if !bytes.Equal(reply[1537:], c0c1[1:]) {
		c.t.Fatal("S2 is not an echo of C1")
	}
	// C2 is S1 echoed.
	if _, err := c.conn.Write(reply[1 : 1+1536]); err != nil {
		c.t.Fatal(err)
	}
}

// write sends one message, split into chunks of the client's chunk size, using
// the header format asked for on the first chunk and type 3 on the rest.
func (c *client) write(format, csid, typeID byte, streamID, ts uint32, payload []byte) {
	c.t.Helper()
	var b []byte
	field := ts
	if format != 0 {
		field = ts - c.lastTs[csid]
	}
	if format == 3 {
		field = c.lastD[csid]
	}
	ext := field >= 0xffffff
	short := field
	if ext {
		short = 0xffffff
	}
	b = append(b, format<<6|csid)
	switch format {
	case 0:
		b = append(b, byte(short>>16), byte(short>>8), byte(short))
		b = append(b, byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload)), typeID)
		b = binary.LittleEndian.AppendUint32(b, streamID)
	case 1:
		b = append(b, byte(short>>16), byte(short>>8), byte(short))
		b = append(b, byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload)), typeID)
	case 2:
		b = append(b, byte(short>>16), byte(short>>8), byte(short))
	}
	if ext {
		b = binary.BigEndian.AppendUint32(b, field)
	}
	c.lastTs[csid], c.lastD[csid] = ts, field
	for off := 0; ; {
		n := len(payload) - off
		if n > c.chunkSize {
			n = c.chunkSize
		}
		b = append(b, payload[off:off+n]...)
		off += n
		if off >= len(payload) {
			break
		}
		b = append(b, 3<<6|csid)
		if ext {
			b = binary.BigEndian.AppendUint32(b, field)
		}
	}
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *client) command(streamID uint32, values ...any) {
	c.write(0, 3, 20, streamID, 0, amfEncode(values...))
}

// setChunkSize tells the server our chunks are a different size from now on.
func (c *client) setChunkSize(n int) {
	c.write(0, 2, 1, 0, 0, be32(uint32(n)))
	c.chunkSize = n
}

// reply is one message read back from the server.
type reply struct {
	typeID   byte
	streamID uint32
	payload  []byte
}

// read takes one message from the server. The server never splits a message
// (it raises its chunk size first), which is itself part of what is checked.
func (c *client) read() (reply, error) {
	var h [12]byte
	if _, err := io.ReadFull(c.conn, h[:]); err != nil {
		return reply{}, err
	}
	if h[0]>>6 != 0 {
		c.t.Fatalf("the server sent a chunk header of format %d; it only ever sends whole messages", h[0]>>6)
	}
	n := int(h[4])<<16 | int(h[5])<<8 | int(h[6])
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return reply{}, err
	}
	return reply{typeID: h[7], streamID: binary.LittleEndian.Uint32(h[8:12]), payload: payload}, nil
}

// until reads until a command with this name arrives, and returns its values.
func (c *client) until(name string) []any {
	c.t.Helper()
	for {
		r, err := c.read()
		if err != nil {
			c.t.Fatalf("waiting for %s: %v", name, err)
		}
		if r.typeID != 20 {
			continue
		}
		values, err := amfDecode(r.payload)
		if err != nil {
			c.t.Fatalf("the server sent a command that does not decode: %v", err)
		}
		if got, _ := values[0].(string); got == name {
			return values
		}
	}
}

// publish runs an encoder's opening: connect, the FMS rituals, createStream,
// publish. It returns the onStatus the server answered the publish with.
func (c *client) publish(app, name string) map[string]any {
	c.t.Helper()
	c.handshake()
	c.command(0, "connect", 1.0, []amfField{{"app", app}, {"type", "nonprivate"}, {"flashVer", "FMLE/3.0"}, {"tcUrl", "rtmp://example/" + app}})
	result := c.until("_result")
	info, _ := result[3].(map[string]any)
	if info["code"] != "NetConnection.Connect.Success" {
		c.t.Fatalf("connect was answered with %v", result)
	}
	c.command(0, "releaseStream", 2.0, nil, name)
	c.command(0, "FCPublish", 3.0, nil, name)
	c.command(0, "createStream", 4.0, nil)
	for {
		values := c.until("_result")
		if txn, _ := values[1].(float64); txn == 4 {
			if id, _ := values[3].(float64); id != 1 {
				c.t.Fatalf("createStream returned stream %v", values[3])
			}
			break
		}
	}
	c.command(1, "publish", 5.0, nil, name, "live")
	status := c.until("onStatus")
	info, _ = status[3].(map[string]any)
	return info
}

func waitClosed(t *testing.T, r *recorder) error {
	t.Helper()
	select {
	case err := <-r.closed:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the sink was never closed")
		return nil
	}
}

func waitTags(t *testing.T, r *recorder, n int) []tag {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if tags := r.snapshot(); len(tags) >= n {
			return tags
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d tags arrived, want %d", len(r.snapshot()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- the tests -------------------------------------------------------------

func TestAPublisherIsAcceptedAndItsMediaArrivesIntact(t *testing.T) {
	rec := newRecorder()
	c, _ := serve(t, rec, nil)
	info := c.publish("live", "STREAM_secret?token=ignored")
	if info["code"] != "NetStream.Publish.Start" {
		t.Fatalf("publish was answered with %v", info)
	}
	if rec.app != "live" {
		t.Errorf("app is %q", rec.app)
	}
	// What an encoder appends after ? is its own business, not part of the key.
	if rec.name != "STREAM_secret" {
		t.Errorf("stream name is %q", rec.name)
	}

	// Metadata the way encoders send it: @setDataFrame in front. An FLV script
	// tag must start at onMetaData, or ffmpeg's demuxer does not know it.
	meta := amfEncode("@setDataFrame", "onMetaData", []amfField{{"width", 1280.0}, {"height", 720.0}})
	c.write(0, 4, TagScript, 1, 0, meta)

	// A keyframe far larger than a chunk, so it crosses many of them; then
	// frames sent with every header format an encoder uses to save bytes.
	big := make([]byte, 5000)
	for i := range big {
		big[i] = byte(i * 7)
	}
	c.write(0, 6, TagVideo, 1, 1000, big)                // absolute timestamp
	c.write(1, 6, TagVideo, 1, 1040, []byte{0x27, 1, 2}) // delta, new length
	c.write(2, 6, TagVideo, 1, 1080, []byte{0x27, 1, 3}) // delta only
	c.write(3, 6, TagVideo, 1, 1120, []byte{0x27, 1, 4}) // nothing: repeat the last delta
	c.write(0, 5, TagAudio, 1, 1010, []byte{0xaf, 1, 9, 9})

	tags := waitTags(t, rec, 6)
	if tags[0].kind != TagScript || !bytes.HasPrefix(tags[0].data, []byte{amfString, 0, 10, 'o', 'n', 'M', 'e', 't', 'a', 'D', 'a', 't', 'a'}) {
		t.Errorf("the metadata tag does not start at onMetaData: %q", tags[0].data[:16])
	}
	if tags[1].kind != TagVideo || tags[1].ts != 1000 || !bytes.Equal(tags[1].data, big) {
		t.Errorf("the keyframe did not survive being split across chunks (ts %d, %d bytes)", tags[1].ts, len(tags[1].data))
	}
	for i, want := range []uint32{1040, 1080, 1120} {
		if got := tags[2+i]; got.ts != want || got.data[2] != byte(2+i) {
			t.Errorf("frame %d: timestamp %d payload %v, want timestamp %d", i, got.ts, got.data, want)
		}
	}
	if tags[5].kind != TagAudio || tags[5].ts != 1010 {
		t.Errorf("audio arrived as %+v", tags[5])
	}

	// An encoder stopping: FCUnpublish, deleteStream, close. That is a goodbye,
	// not a dropped connection, and the difference decides whether the stream
	// is ended now or held open for a reconnect.
	c.command(1, "FCUnpublish", 6.0, nil, "STREAM_secret")
	c.command(1, "deleteStream", 7.0, nil, 1.0)
	if err := waitClosed(t, rec); err != nil {
		t.Errorf("a clean goodbye was reported as %v", err)
	}
}

func TestAConnectionThatJustStopsIsADropNotAGoodbye(t *testing.T) {
	rec := newRecorder()
	c, _ := serve(t, rec, nil)
	c.publish("live", "key")
	c.write(0, 6, TagVideo, 1, 0, []byte{0x17, 1, 0})
	waitTags(t, rec, 1)
	c.conn.Close()
	if err := waitClosed(t, rec); err == nil {
		t.Error("a connection that vanished was reported as a clean goodbye")
	}
}

func TestSilenceIsADrop(t *testing.T) {
	// A broadcaster whose network died sends no FIN. Only the clock notices.
	rec := newRecorder()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Handler: rec, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), IdleTimeout: 300 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, ln) }()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	c := &client{t: t, conn: conn, chunkSize: 128, lastTs: map[byte]uint32{}, lastD: map[byte]uint32{}}
	c.publish("live", "key")
	c.write(0, 6, TagVideo, 1, 0, []byte{0x17, 1, 0})
	start := time.Now()
	if err := waitClosed(t, rec); err == nil {
		t.Error("silence was reported as a clean goodbye")
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Errorf("a silent publisher was held for %s with a 300ms idle timeout", waited)
	}
}

func TestARefusedPublishLearnsNothingAndSendsNothing(t *testing.T) {
	var logs logBuffer
	rec := newRecorder()
	rec.refuse = errors.New("stream 01ABC: wrong key s3cr3t-detail")
	c, _ := serve(t, rec, &logs)
	info := c.publish("live", "STREAM_wrongsecret")
	if info["level"] != "error" || info["code"] != "NetStream.Publish.BadName" {
		t.Errorf("a refused publish was answered with %v", info)
	}
	// The handler's reason stays on the server: the client is told it was
	// refused, not why, so the answer cannot be used to probe for stream ids.
	for _, v := range info {
		if s, ok := v.(string); ok && strings.Contains(s, "01ABC") {
			t.Errorf("the refusal tells the client why: %q", s)
		}
	}
	// The connection is closed; media sent anyway goes nowhere.
	c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.Copy(io.Discard, c.conn); err != nil && !errors.Is(err, io.EOF) {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Error("the server left a refused connection open")
		}
	}
	if len(rec.snapshot()) != 0 {
		t.Error("a refused publisher delivered media")
	}
	// The stream name is a credential. Whatever was logged about this
	// connection, the name is not in it.
	if strings.Contains(logs.String(), "STREAM_wrongsecret") || strings.Contains(logs.String(), "wrongsecret") {
		t.Errorf("the stream key was logged:\n%s", logs.String())
	}
}

func TestTheStreamKeyIsNeverLoggedOnTheHappyPathEither(t *testing.T) {
	var logs logBuffer
	rec := newRecorder()
	c, _ := serve(t, rec, &logs)
	c.publish("live", "STREAM_topsecretvalue")
	c.write(0, 6, TagVideo, 1, 0, []byte{0x17, 1, 0})
	waitTags(t, rec, 1)
	c.command(1, "deleteStream", 7.0, nil, 1.0)
	waitClosed(t, rec)
	if strings.Contains(logs.String(), "topsecretvalue") {
		t.Errorf("the stream key was logged:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "rtmp publisher accepted") {
		t.Errorf("nothing was logged about an accepted publisher:\n%s", logs.String())
	}
}

func TestMediaBeforeAPublishIsRefused(t *testing.T) {
	rec := newRecorder()
	c, _ := serve(t, rec, nil)
	c.handshake()
	c.command(0, "connect", 1.0, []amfField{{"app", "live"}})
	c.until("_result")
	// No publish. Straight to video.
	c.write(0, 6, TagVideo, 1, 0, []byte{0x17, 1, 0})
	c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := io.Copy(io.Discard, c.conn)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Error("the server kept a connection that sent media without publishing")
	}
	if rec.publishN != 0 || len(rec.snapshot()) != 0 {
		t.Error("media reached a handler nobody was authorised for")
	}
}

func TestALargeMessageIsNotBufferedForSomeoneWhoHasNotPublished(t *testing.T) {
	// Before a key has been accepted the only messages are commands. A
	// stranger must not be able to make the server allocate megabytes by
	// declaring a message that long.
	rec := newRecorder()
	c, _ := serve(t, rec, nil)
	c.handshake()
	// A 12-byte header announcing a 10 MB command, and nothing after it.
	header := []byte{3, 0, 0, 0, 0x98, 0x96, 0x80, 20, 0, 0, 0, 0}
	if _, err := c.conn.Write(header); err != nil {
		t.Fatal(err)
	}
	c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := io.Copy(io.Discard, c.conn)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Error("the server is waiting for 10 MB of command from an unauthenticated client")
	}
}

func TestAnAbsurdChunkSizeIsRefused(t *testing.T) {
	rec := newRecorder()
	c, _ := serve(t, rec, nil)
	c.publish("live", "key")
	c.write(0, 2, 1, 0, 0, be32(0x7fffffff))
	if err := waitClosed(t, rec); err == nil {
		t.Error("a 2 GB chunk size was accepted")
	}
}

func TestALargerChunkSizeIsHonoured(t *testing.T) {
	// OBS and ffmpeg both raise the chunk size straight after connecting.
	rec := newRecorder()
	c, _ := serve(t, rec, nil)
	c.publish("live", "key")
	c.setChunkSize(4096)
	payload := bytes.Repeat([]byte{0xab}, 10_000)
	c.write(0, 6, TagVideo, 1, 77, payload)
	tags := waitTags(t, rec, 1)
	if !bytes.Equal(tags[0].data, payload) || tags[0].ts != 77 {
		t.Errorf("a message in 4096-byte chunks arrived as %d bytes at %d", len(tags[0].data), tags[0].ts)
	}
}

func TestExtendedTimestampsSurviveChunking(t *testing.T) {
	// Past 4h39m the 24-bit timestamp field overflows and four more bytes
	// carry it — repeated on every continuation chunk, which is the part
	// implementations get wrong.
	rec := newRecorder()
	c, _ := serve(t, rec, nil)
	c.publish("live", "key")
	payload := bytes.Repeat([]byte{0x5a}, 700) // several 128-byte chunks
	const late = 0x01234567
	c.write(0, 6, TagVideo, 1, late, payload)
	c.write(1, 6, TagVideo, 1, late+40, []byte{0x27, 1, 1})
	tags := waitTags(t, rec, 2)
	if tags[0].ts != late || !bytes.Equal(tags[0].data, payload) {
		t.Errorf("extended timestamp message arrived as ts %#x, %d bytes", tags[0].ts, len(tags[0].data))
	}
	if tags[1].ts != late+40 {
		t.Errorf("the frame after it has timestamp %#x, want %#x", tags[1].ts, late+40)
	}
}

func TestTheServerAcknowledgesWhatItReceives(t *testing.T) {
	// Some encoders stop sending when the bytes they have sent since the last
	// acknowledgement pass the window they were given.
	rec := newRecorder()
	c, _ := serve(t, rec, nil)
	c.publish("live", "key")
	c.setChunkSize(60_000)
	payload := make([]byte, 60_000)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := 0; i < 40; i++ { // 2.4 MB, past half of the 2.5 MB window
			c.write(0, 6, TagVideo, 1, uint32(i*40), payload)
		}
	}()
	defer func() { <-sent }()
	for {
		r, err := c.read()
		if err != nil {
			t.Fatalf("no acknowledgement arrived: %v", err)
		}
		if r.typeID == 3 {
			if got := binary.BigEndian.Uint32(r.payload); got < 1_000_000 {
				t.Errorf("acknowledged %d bytes after megabytes were sent", got)
			}
			return
		}
	}
}

func TestGarbageNeverPanicsTheServer(t *testing.T) {
	// Anything can connect to an open port. Whatever it sends, the connection
	// ends and the server is still there for the next one.
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 60; i++ {
		rec := newRecorder()
		c, _ := serve(t, rec, nil)
		junk := make([]byte, 200+rng.Intn(3000))
		rng.Read(junk)
		if i%2 == 0 {
			// Half the time, get through the handshake first so the junk
			// reaches the chunk parser and the AMF decoder.
			c.handshake()
			if i%4 == 0 {
				// And half of those, make the junk a command.
				junk = append([]byte{3, 0, 0, 0, 0, byte(len(junk) >> 8 & 0x3f), byte(len(junk)), 20, 0, 0, 0, 0}, junk...)
			}
		} else {
			junk[0] = 3
		}
		c.conn.Write(junk)
		c.conn.Close()
	}
	// The process is still here, which is the assertion; and a real client
	// can still publish.
	rec := newRecorder()
	c, _ := serve(t, rec, nil)
	if info := c.publish("live", "key"); info["code"] != "NetStream.Publish.Start" {
		t.Errorf("after the garbage, a publish was answered with %v", info)
	}
}

func TestAMF0(t *testing.T) {
	encoded := amfEncode("connect", 1.0, []amfField{{"app", "live"}, {"n", 2.5}, {"nested", []amfField{{"k", "v"}}}}, nil)
	values, err := amfDecode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 4 || values[0] != "connect" || values[1] != 1.0 || values[3] != nil {
		t.Fatalf("decoded %v", values)
	}
	obj := values[2].(map[string]any)
	if obj["app"] != "live" || obj["n"] != 2.5 || obj["nested"].(map[string]any)["k"] != "v" {
		t.Errorf("object decoded as %v", obj)
	}

	// The types an encoder may put in a connect object that this server never
	// writes: they must be skipped correctly so what follows is still read.
	mixed := []byte{amfBoolean, 1}
	mixed = append(mixed, amfECMAArray, 0, 0, 0, 9) // a count that is wrong, as they are
	mixed = append(mixed, 0, 1, 'a', amfNumber, 0x40, 0x08, 0, 0, 0, 0, 0, 0)
	mixed = append(mixed, 0, 0, amfObjectEnd)
	mixed = append(mixed, amfStrictArray, 0, 0, 0, 2, amfNull, amfUndefined)
	mixed = append(mixed, amfDate, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	mixed = append(mixed, amfLongString, 0, 0, 0, 2, 'h', 'i')
	mixed = append(mixed, amfString, 0, 3, 'e', 'n', 'd')
	values, err = amfDecode(mixed)
	if err != nil {
		t.Fatalf("mixed types: %v", err)
	}
	if len(values) != 6 || values[0] != true || values[1].(map[string]any)["a"] != 3.0 || values[4] != "hi" || values[5] != "end" {
		t.Errorf("mixed types decoded as %v", values)
	}

	// Truncated at every length: an error, never a panic and never a value
	// invented from bytes that are not there.
	for cut := 1; cut < len(encoded); cut++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("decoding %d of %d bytes panicked: %v", cut, len(encoded), r)
				}
			}()
			_, _ = amfDecode(encoded[:cut])
		}()
	}
	// A strict array claiming a billion elements must not allocate for them.
	if _, err := amfDecode([]byte{amfStrictArray, 0x3b, 0x9a, 0xca, 0x00, amfNull}); err == nil {
		t.Error("an array longer than its message was accepted")
	}
	// Nesting deeper than any command has cause to be.
	deep := bytes.Repeat([]byte{amfObject, 0, 1, 'a'}, 100)
	if _, err := amfDecode(deep); err == nil {
		t.Error("a hundred levels of nesting were accepted")
	}
	// Random bytes: never a panic.
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 5000; i++ {
		junk := make([]byte, rng.Intn(64))
		rng.Read(junk)
		_, _ = amfDecode(junk)
	}
}

func TestStripSetDataFrame(t *testing.T) {
	meta := amfEncode("onMetaData", []amfField{{"width", 1280.0}})
	if got := stripSetDataFrame(append(amfEncode("@setDataFrame"), meta...)); !bytes.Equal(got, meta) {
		t.Errorf("the marker was not removed: %q", got)
	}
	if got := stripSetDataFrame(meta); !bytes.Equal(got, meta) {
		t.Errorf("metadata with no marker was changed: %q", got)
	}
	for _, short := range [][]byte{nil, {amfString}, {amfString, 0, 13, '@'}} {
		if got := stripSetDataFrame(short); !bytes.Equal(got, short) {
			t.Errorf("a short tag was changed: %q", got)
		}
	}
}
