// Package live is a stream in, an address out.
//
// A live stream here is the VOD pipeline with the end taken off: a broadcaster
// pushes RTMP, one ffmpeg encodes the same ladder in two-second segments, and
// the segments are published as they close by the same code that publishes a
// VOD while it encodes. When the broadcaster stops, the playlists are closed
// and what is left is an ordinary asset, ready, at the address viewers were
// already watching.
//
// What it does not contain is a viewer. Nobody connects to this process to
// watch: a viewer fetches a playlist and segments, which are files, and files
// are what a CDN is for. One broadcaster costs one ffmpeg; ten thousand
// viewers cost a cache.
package live

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aura/media/internal/assets"
	"aura/media/internal/id"
	"aura/media/internal/pipeline"
	"aura/media/internal/publish"
	"aura/media/internal/rtmp"
	"aura/media/internal/store"
)

// App is the RTMP application name streams are published under:
// rtmp://host/live/<stream key>.
const App = "live"

// Config is how live is wired.
type Config struct {
	Repo   *Repo
	Assets *assets.Repo
	Store  store.Store
	Runner pipeline.Runner
	Log    *slog.Logger

	// WorkDir is local scratch space; each stream gets a directory in it for
	// as long as it is live.
	WorkDir string
	// IngestURL is the address broadcasters are told to push to, without the
	// key: rtmp://host:1935/live.
	IngestURL string
	Encoding  pipeline.Encoding

	// Grace is how long a broadcaster whose connection dropped has to come
	// back before the stream is closed. Coming back inside it continues the
	// same stream and the same asset.
	Grace time.Duration
	// Window is how much of the stream the live playlist lists. Zero lists
	// all of it. Segments older than the window are kept either way; this
	// bounds what a live player downloads every two seconds, not what is
	// stored.
	Window time.Duration
	// Lease is how long this node's hold on a stream lasts between renewals.
	// A node that dies mid-stream loses it after this, and whichever node
	// notices closes what was published into a VOD.
	Lease time.Duration

	FrameEvery float64
	MaxFrames  int
	// SyncEvery is how often closed segments are looked for and uploaded.
	SyncEvery time.Duration
	// ProbeAfter is how much of the stream is held back before ffprobe is
	// asked what it is. The ladder depends on the answer, so the encoder
	// cannot start without it.
	ProbeAfter time.Duration
}

func (c *Config) withDefaults() {
	if c.Grace <= 0 {
		c.Grace = 30 * time.Second
	}
	if c.Lease <= 0 {
		c.Lease = 30 * time.Second
	}
	if c.SyncEvery <= 0 {
		c.SyncEvery = 500 * time.Millisecond
	}
	if c.ProbeAfter <= 0 {
		c.ProbeAfter = time.Second
	}
	if c.FrameEvery <= 0 {
		c.FrameEvery = 5
	}
	if c.MaxFrames <= 0 {
		c.MaxFrames = 200
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
}

// Manager owns every stream this node is encoding.
type Manager struct {
	cfg  Config
	node string

	mu       sync.Mutex
	sessions map[string]*session
	wg       sync.WaitGroup
	// base is the lifetime of the encodes. It outlives any one RTMP
	// connection, which is the point: a dropped connection must not end the
	// stream.
	base context.Context
	stop context.CancelFunc
}

// New prepares a manager.
func New(cfg Config) (*Manager, error) {
	if cfg.Repo == nil || cfg.Assets == nil || cfg.Store == nil {
		return nil, errors.New("live needs a stream repo, an asset repo and a store")
	}
	cfg.withDefaults()
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(os.TempDir(), "aura-media-live")
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("live work directory: %w", err)
	}
	base, stop := context.WithCancel(context.Background())
	return &Manager{
		cfg:      cfg,
		node:     "n-" + id.New()[10:18],
		sessions: map[string]*session{},
		base:     base,
		stop:     stop,
	}, nil
}

// View is a stream as the API shows it.
type View struct {
	Stream
	// Address is where the stream plays, and where its VOD will be once it
	// ends. It is known before anyone has broadcast, and serves nothing until
	// they have.
	Address   string `json:"address"`
	IngestURL string `json:"ingest_url"`
	// StreamKey is present exactly once: in the answer to creating the stream.
	StreamKey string `json:"stream_key,omitempty"`
}

func (m *Manager) view(s Stream) View {
	return View{
		Stream:    s,
		Address:   m.cfg.Store.Address(path.Join("out", s.AssetID, pipeline.MasterPlaylist)),
		IngestURL: m.cfg.IngestURL,
	}
}

// Create makes a new stream and returns it with its key. This is the only
// time the key exists outside the broadcaster's encoder.
func (m *Manager) Create(ctx context.Context) (View, error) {
	streamID := id.New()
	key, hash, err := NewKey(streamID)
	if err != nil {
		return View{}, err
	}
	// The asset id is chosen now so the playback address can be handed out
	// with the key. The asset row itself is not created until someone
	// broadcasts: a stream nobody ever starts should not sit in the asset list
	// looking like an upload that never encoded.
	s, err := m.cfg.Repo.Create(ctx, streamID, hash, id.New())
	if err != nil {
		return View{}, err
	}
	v := m.view(s)
	v.StreamKey = key
	return v, nil
}

// Get reads one stream.
func (m *Manager) Get(ctx context.Context, streamID string) (View, error) {
	s, err := m.cfg.Repo.Get(ctx, streamID)
	if err != nil {
		return View{}, err
	}
	return m.view(s), nil
}

// List returns streams newest first.
func (m *Manager) List(ctx context.Context, limit int) ([]View, error) {
	streams, err := m.cfg.Repo.List(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(streams))
	for _, s := range streams {
		out = append(out, m.view(s))
	}
	return out, nil
}

// End stops a stream on request. If this node is the one encoding it, it
// stops now; otherwise the node that is finds out at its next heartbeat.
func (m *Manager) End(ctx context.Context, streamID string) (View, error) {
	s, err := m.cfg.Repo.RequestEnd(ctx, streamID)
	if err != nil {
		return View{}, err
	}
	m.mu.Lock()
	sess := m.sessions[streamID]
	m.mu.Unlock()
	if sess != nil {
		sess.requestEnd("ended through the API")
	}
	return m.view(s), nil
}

// errRefused is the one answer to every publish that is not accepted. The
// reason is logged here, by stream id, and is not sent to the client.
var errRefused = errors.New("refused")

// Publish implements rtmp.Handler: it is called when a broadcaster names the
// stream it wants to publish, and decides whether it may.
func (m *Manager) Publish(ctx context.Context, app, name string, remote net.Addr) (rtmp.Sink, error) {
	// The key is never logged, here or anywhere: only the id half, which is
	// public, and only once the key has been checked.
	if app != App {
		return nil, errRefused
	}
	streamID, ok := KeyStreamID(name)
	if !ok {
		return nil, errRefused
	}
	stream, err := m.cfg.Repo.Get(ctx, streamID)
	if err != nil {
		// Hash anyway, so a key for a stream that does not exist takes as long
		// to refuse as a wrong key for one that does.
		_ = KeyMatches(name, strings.Repeat("0", 64))
		return nil, errRefused
	}
	if !KeyMatches(name, stream.KeyHash) {
		m.cfg.Log.Warn("a publish presented the wrong key", "stream", streamID, "remote", remote.String())
		return nil, errRefused
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.base.Err() != nil {
		return nil, errRefused
	}
	if sess := m.sessions[streamID]; sess != nil {
		// The stream is live on this node: this is the broadcaster coming
		// back, or coming back before the old connection was noticed dead.
		conn := sess.attach()
		if conn == nil {
			return nil, errRefused
		}
		m.cfg.Log.Info("broadcaster reconnected; continuing the same stream", "stream", streamID)
		return conn, nil
	}
	claimed, err := m.cfg.Repo.Claim(ctx, streamID, m.node, m.cfg.Lease)
	if err != nil {
		m.cfg.Log.Info("a publish with a valid key was refused", "stream", streamID, "state", string(stream.State), "error", err)
		return nil, errRefused
	}
	sess := newSession(m, claimed)
	m.sessions[streamID] = sess
	conn := sess.attach()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		sess.run(m.base)
		m.mu.Lock()
		delete(m.sessions, streamID)
		m.mu.Unlock()
	}()
	return conn, nil
}

// Run closes what dead nodes left behind, until ctx is cancelled; then it ends
// every stream this node is encoding and waits for them to be closed into
// VODs. A deploy is a SIGTERM, and a SIGTERM must not cost a broadcaster the
// recording of everything they had streamed so far.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.Lease / 2)
	defer ticker.Stop()
	m.adoptOrphans(ctx)
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			m.adoptOrphans(ctx)
		}
	}
	m.mu.Lock()
	m.stop()
	m.mu.Unlock()
	m.wg.Wait()
}

// session is one stream being encoded.
type session struct {
	m      *Manager
	stream Stream
	log    *slog.Logger
	dir    string

	// wmu guards everything a connection's goroutine touches: which
	// connection is current, the timestamp rebasing, and the pipe into ffmpeg.
	wmu     sync.Mutex
	current *connection
	conns   int
	closed  bool
	stdin   io.WriteCloser
	// input is the same pipe as stdin, reachable without wmu, so that it can
	// be closed out from under a write that is blocked on an ffmpeg that has
	// stopped reading.
	input atomic.Pointer[io.WriteCloser]
	// pending is the stream so far, as FLV, held until ffprobe has said what
	// it is and ffmpeg has been started to read it.
	pending      []byte
	sawKeyframe  bool
	firstVideoTs uint32
	lastOut      uint32
	sawTag       bool

	probeReady chan struct{}
	probeOnce  sync.Once
	// events carries connection changes and end requests to the control loop.
	events chan event
	done   chan struct{}
}

type event struct {
	kind   eventKind
	reason string
}

type eventKind int

const (
	evDropped eventKind = iota
	evGoodbye
	evEnd
)

func newSession(m *Manager, s Stream) *session {
	return &session{
		m:          m,
		stream:     s,
		log:        m.cfg.Log.With("stream", s.ID, "asset", s.AssetID),
		probeReady: make(chan struct{}),
		events:     make(chan event, 16),
		done:       make(chan struct{}),
	}
}

func (s *session) notify(e event) {
	select {
	case s.events <- e:
	case <-s.done:
	}
}

func (s *session) requestEnd(reason string) { s.notify(event{kind: evEnd, reason: reason}) }

// connection is one RTMP connection feeding a session. A session outlives its
// connections; at most one is current.
type connection struct {
	s *session
	// first is whether this is the connection the stream started on.
	first bool
	// offset is added to this connection's timestamps. A broadcaster that
	// reconnects starts counting from zero again, and ffmpeg must see one
	// stream whose clock only goes forward.
	offset    int64
	offsetSet bool
}

// attach makes a new connection the current one, replacing any other. The one
// replaced finds out the next time it tries to write, and is dropped: whoever
// holds the key and connected last is the broadcaster.
func (s *session) attach() *connection {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.closed {
		return nil
	}
	c := &connection{s: s, first: s.conns == 0}
	s.conns++
	s.current = c
	return c
}

// connected reports whether a broadcaster is attached right now.
func (s *session) connected() bool {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.current != nil
}

var errSuperseded = errors.New("another connection took over this stream")
var errStreamOver = errors.New("the stream has ended")

// Tag implements rtmp.Sink. It runs on the connection's goroutine, and a slow
// encoder slows it down, which slows the read from the socket, which is the
// only back-pressure RTMP has.
func (c *connection) Tag(kind byte, timestamp uint32, data []byte) error {
	s := c.s
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.closed {
		return errStreamOver
	}
	if s.current != c {
		return errSuperseded
	}
	if kind == rtmp.TagScript && !c.first {
		// The metadata of a reconnect describes the same stream again. The
		// encoder has already been configured from the first one.
		return nil
	}
	if !c.offsetSet {
		c.offsetSet = true
		if !c.first {
			// Carry on one frame after where the last connection stopped. The
			// time the broadcaster was gone is not in the recording: nothing
			// was broadcast during it.
			c.offset = int64(s.lastOut) + 40 - int64(timestamp)
		}
	}
	out := int64(timestamp) + c.offset
	if out < int64(s.lastOut) && s.sawTag {
		// Audio and video interleave a little out of order and that is fine;
		// time going backwards by more than that is not.
		if int64(s.lastOut)-out > 1000 {
			out = int64(s.lastOut)
		}
	}
	if out < 0 {
		out = 0
	}
	ts := uint32(out)
	if ts > s.lastOut {
		s.lastOut = ts
	}
	s.sawTag = true

	if s.stdin != nil {
		_, err := s.stdin.Write(flvTag(kind, ts, data))
		return err
	}

	// Still deciding what this stream is: hold it.
	s.pending = append(s.pending, flvTag(kind, ts, data)...)
	if len(s.pending) > maxPending {
		return errors.New("the stream sent more than can be held before its first keyframe")
	}
	if kind == rtmp.TagVideo && isKeyframe(data) {
		if !s.sawKeyframe {
			s.sawKeyframe, s.firstVideoTs = true, ts
		}
	}
	if s.sawKeyframe && time.Duration(int64(ts)-int64(s.firstVideoTs))*time.Millisecond >= s.m.cfg.ProbeAfter {
		s.probeOnce.Do(func() { close(s.probeReady) })
	}
	return nil
}

// Close implements rtmp.Sink.
func (c *connection) Close(err error) {
	s := c.s
	s.wmu.Lock()
	current := s.current == c
	if current {
		s.current = nil
	}
	s.wmu.Unlock()
	if !current {
		return // superseded: the stream has a newer connection and carries on
	}
	if err == nil {
		s.notify(event{kind: evGoodbye})
		return
	}
	s.notify(event{kind: evDropped})
}

// maxPending bounds what is held while waiting to probe. A second of even a
// very high bitrate stream is a few megabytes.
const maxPending = 48 << 20

// isKeyframe reads the frame type out of an FLV video tag header, in both the
// original layout and the enhanced one newer encoders use for HEVC and AV1. A
// sequence header is not a frame and does not count.
func isKeyframe(data []byte) bool {
	if len(data) < 2 {
		return false
	}
	if data[0]&0x80 != 0 {
		// Enhanced RTMP: bit 7 set, frame type in bits 4-6, packet type in the
		// low nibble (0 is a sequence header).
		return (data[0]>>4)&0x7 == 1 && data[0]&0x0f != 0
	}
	// Classic: frame type in the high nibble; for AVC the next byte is 0 for a
	// sequence header and 1 for a frame.
	if data[0]>>4 != 1 {
		return false
	}
	if data[0]&0x0f == 7 {
		return data[1] == 1
	}
	return true
}

// flvHeader opens an FLV stream. The flags byte says whether to expect audio
// and video; ffmpeg waits for every stream it was told to expect, so it is
// told the truth.
func flvHeader(audio bool) []byte {
	flags := byte(0x01)
	if audio {
		flags |= 0x04
	}
	return []byte{'F', 'L', 'V', 1, flags, 0, 0, 0, 9, 0, 0, 0, 0}
}

// flvTag frames one tag: an 11-byte header, the body, and the size of both.
func flvTag(kind byte, timestamp uint32, data []byte) []byte {
	out := make([]byte, 11, 11+len(data)+4)
	out[0] = kind
	out[1], out[2], out[3] = byte(len(data)>>16), byte(len(data)>>8), byte(len(data))
	out[4], out[5], out[6] = byte(timestamp>>16), byte(timestamp>>8), byte(timestamp)
	out[7] = byte(timestamp >> 24)
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, uint32(11+len(data)))
}

// run is the life of one stream: wait for enough to probe, start the encoder,
// publish until it is over, close it into a VOD.
func (s *session) run(ctx context.Context) {
	defer close(s.done)
	cfg := s.m.cfg
	prefix := path.Join("out", s.stream.AssetID)

	dir, err := os.MkdirTemp(cfg.WorkDir, "live-*")
	if err != nil {
		s.abandon(fmt.Sprintf("work directory: %v", err))
		return
	}
	s.dir = dir
	defer os.RemoveAll(dir)

	// --- What is it? ------------------------------------------------------
	probed, err := s.probe(ctx)
	if err != nil {
		s.log.Warn("a broadcast started and could not be used", "error", err)
		s.abandon(err.Error())
		return
	}
	enc := cfg.Encoding
	ladder := enc.Ladder(pipeline.Ladder(probed))
	outDir, framesDir := filepath.Join(dir, "out"), filepath.Join(dir, "frames")
	rungs := make([]string, 0, len(ladder))
	for _, r := range ladder {
		if err := os.MkdirAll(filepath.Join(outDir, r.Name), 0o755); err != nil {
			s.abandon(fmt.Sprintf("output directory: %v", err))
			return
		}
		rungs = append(rungs, r.Name)
	}
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		s.abandon(fmt.Sprintf("frames directory: %v", err))
		return
	}

	// --- Encode -----------------------------------------------------------
	args := pipeline.HLSJob{
		OutDir:     outDir,
		Ladder:     ladder,
		Source:     probed,
		Encoding:   enc,
		Live:       true,
		FramesDir:  framesDir,
		FrameEvery: cfg.FrameEvery,
		MaxFrames:  cfg.MaxFrames,
	}.Args()
	// Not ctx: when the node is told to stop, ffmpeg must be allowed to finish
	// the segment it is in, and a context that kills it would lose that.
	proc, err := cfg.Runner.Start(context.WithoutCancel(ctx), args, true, nil)
	if err != nil {
		s.abandon(err.Error())
		return
	}
	s.wmu.Lock()
	_, werr := proc.Stdin.Write(append(flvHeader(probed.HasAudio), s.pending...))
	s.pending = nil
	s.stdin = proc.Stdin
	s.input.Store(&proc.Stdin)
	s.wmu.Unlock()
	if werr != nil {
		proc.Kill()
		s.abandon(fmt.Sprintf("ffmpeg would not take the stream: %v", proc.Wait()))
		return
	}

	// From here there is an asset, and whatever happens it ends as one: ready
	// if a single segment got out, failed if not.
	if _, err := cfg.Assets.CreateLive(ctx, s.stream.AssetID); err != nil {
		s.shut()
		proc.Kill()
		_ = proc.Wait()
		s.abandon(err.Error())
		return
	}
	s.log.Info("live", "source", fmt.Sprintf("%dx%d", probed.Width, probed.Height), "rungs", len(ladder),
		"audio", probed.HasAudio, "encoder", enc.Encoder())

	window := 0
	if cfg.Window > 0 {
		window = int(cfg.Window / (pipeline.LiveSegmentSeconds * time.Second))
		if window < 3 {
			window = 3 // fewer than three and a player has nothing to buffer
		}
	}
	pub := &publish.Publisher{Store: cfg.Store, Dir: outDir, Prefix: prefix, Rungs: rungs, Window: window, DropLocal: true}
	address := cfg.Store.Address(path.Join(prefix, pipeline.MasterPlaylist))
	out := assets.Outputs{}
	for _, r := range ladder {
		out.Renditions = append(out.Renditions, assets.Rendition{
			Name:        r.Name,
			Height:      r.Height,
			Width:       pipeline.WidthFor(probed, r.Height),
			BitrateKbps: r.VideoKbps,
			Playlist:    path.Join(prefix, r.Name, publish.RungPlaylist),
		})
	}

	syncTick := time.NewTicker(cfg.SyncEvery)
	defer syncTick.Stop()
	beat := time.NewTicker(cfg.Lease / 3)
	defer beat.Stop()
	grace := time.NewTimer(time.Hour)
	grace.Stop()
	defer grace.Stop()

	published := false
	reason := ""
	// failingSince is when publishing started failing, if it is. A store that
	// refuses one write is not a reason to end somebody's broadcast: the next
	// pass uploads what this one could not. A store that has refused every
	// write for this long is.
	var failingSince time.Time
	const giveUpAfter = 20 * time.Second
	for reason == "" {
		select {
		case <-ctx.Done():
			reason = "the node stopped"
		case <-proc.Exited():
			reason = "the encoder stopped"
		case <-grace.C:
			// Asked rather than tracked: a reconnect does not cancel this
			// timer, it makes the answer here "someone is connected", and a
			// later drop starts the wait again from then.
			if !s.connected() {
				reason = "the broadcaster disconnected and did not come back"
			}
		case e := <-s.events:
			switch e.kind {
			case evDropped:
				s.log.Info("broadcaster dropped; holding the stream open", "grace", cfg.Grace.String())
				grace.Reset(cfg.Grace)
			case evGoodbye:
				reason = "the broadcaster ended the stream"
			case evEnd:
				reason = e.reason
			}
		case <-beat.C:
			beatCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			err := cfg.Repo.Heartbeat(beatCtx, s.stream.ID, s.m.node, cfg.Lease)
			cancel()
			if errors.Is(err, ErrNotHeld) {
				reason = "ended through the API"
			} else if err != nil {
				// The database blinked. The stream carries on; if it stays
				// down past the lease another node will take the stream, and
				// the heartbeat after that says so.
				s.log.Warn("could not renew the hold on this stream", "error", err)
			}
		case <-syncTick.C:
			if ctx.Err() != nil {
				reason = "the node stopped"
				break
			}
			status, err := pub.Sync(ctx)
			if err == nil && status.Playable && !published {
				if err = cfg.Assets.MarkPublished(ctx, s.stream.AssetID, address, out); err == nil {
					published = true
					s.log.Info("live and playable", "address", address)
				}
			}
			switch {
			case err == nil:
				if !failingSince.IsZero() {
					s.log.Info("publishing recovered", "after", time.Since(failingSince).Round(time.Millisecond).String())
					failingSince = time.Time{}
				}
			case failingSince.IsZero():
				failingSince = time.Now()
				s.log.Warn("could not publish the stream; trying again", "error", err)
			case time.Since(failingSince) > giveUpAfter:
				s.log.Error("could not publish the stream", "error", err, "for", giveUpAfter.String())
				reason = "publishing failed: " + err.Error()
			}
		}
	}

	// --- Close it into a VOD ----------------------------------------------
	s.log.Info("ending the stream", "reason", reason)
	// Its own deadline, detached from the node's: this is the part that must
	// finish even though — especially though — the node is stopping.
	endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()

	s.shut() // end of input: ffmpeg flushes, closes the last segment, writes ENDLIST
	select {
	case <-proc.Exited():
	case <-time.After(20 * time.Second):
		s.log.Warn("ffmpeg did not finish after its input ended; stopping it")
		proc.Kill()
	}
	clean := proc.Wait() == nil

	finish := func() error {
		result, err := pub.Finish(endCtx, clean)
		if err != nil && clean {
			// ffmpeg said it finished and the playlists disagree. For a VOD
			// that is a failed encode; here the segments are already
			// published and watched, and closing them is still right.
			result, err = pub.Finish(endCtx, false)
		}
		if err != nil {
			return err
		}
		if err := pipeline.CheckAligned(result.Playlists); err != nil {
			// Too late to encode it again, and the stream is still a video.
			// Said loudly, because it means this encoder's ladder cannot be
			// trusted for switching.
			s.log.Error("the live ladder's rungs are not aligned", "error", err, "encoder", enc.Encoder())
		}
		out.DurationMs = result.DurationMs
		// Best-effort, like every frame this service samples.
		if keys, err := publish.Frames(endCtx, cfg.Store, prefix, framesDir); err == nil && len(keys) > 0 {
			out.Frames = keys
			if poster, err := copyObject(endCtx, cfg.Store, keys[0], path.Join(prefix, "poster.jpg")); err == nil {
				out.Poster = poster
			}
		}
		return cfg.Assets.MarkReady(endCtx, s.stream.AssetID, address, out)
	}
	detail := ""
	if err := finish(); err != nil {
		s.log.Error("the stream could not be closed into a VOD", "error", err)
		detail = err.Error()
		if merr := cfg.Assets.MarkFailed(endCtx, s.stream.AssetID, detail); merr != nil {
			s.log.Error("could not update the asset", "error", merr)
		}
	} else {
		s.log.Info("stream closed into a VOD", "duration_ms", out.DurationMs)
	}
	if err := cfg.Repo.Finish(endCtx, s.stream.ID, s.m.node, detail); err != nil {
		s.log.Error("could not record the end of the stream", "error", err)
	}
}

// shut stops taking input and closes ffmpeg's. Connections still attached are
// dropped the next time they write.
func (s *session) shut() {
	// The pipe first, and without the lock: a connection blocked writing into
	// it holds the lock, and closing the pipe is what unblocks it.
	if stdin := s.input.Load(); stdin != nil {
		_ = (*stdin).Close()
	}
	s.wmu.Lock()
	s.closed = true
	s.wmu.Unlock()
}

// abandon gives the stream back, unused: nothing was published, so the key is
// still good and the broadcaster can try again.
func (s *session) abandon(reason string) {
	s.shut()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.m.cfg.Repo.Release(ctx, s.stream.ID, s.m.node, reason); err != nil {
		s.log.Error("could not release the stream", "error", err)
	}
}

// probe waits until about a second of the stream has arrived, writes it to a
// file and asks ffprobe what it is.
func (s *session) probe(ctx context.Context) (pipeline.Media, error) {
	wait := time.NewTimer(10*time.Second + s.m.cfg.ProbeAfter)
	defer wait.Stop()
waiting:
	for {
		select {
		case <-s.probeReady:
			break waiting
		case <-ctx.Done():
			return pipeline.Media{}, errors.New("the node stopped before the stream began")
		case <-wait.C:
			return pipeline.Media{}, errors.New("no video keyframe arrived in the first ten seconds; this service packages video")
		case e := <-s.events:
			if e.kind == evDropped || e.kind == evGoodbye || e.kind == evEnd {
				return pipeline.Media{}, errors.New("the broadcaster left before the stream began")
			}
		}
	}
	s.wmu.Lock()
	sample := append(flvHeader(true), s.pending...)
	s.wmu.Unlock()
	file := filepath.Join(s.dir, "probe.flv")
	if err := os.WriteFile(file, sample, 0o600); err != nil {
		return pipeline.Media{}, err
	}
	defer os.Remove(file)
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return s.m.cfg.Runner.Probe(probeCtx, file)
}

// copyObject duplicates a stored object under another key.
func copyObject(ctx context.Context, s store.Store, from, to string) (string, error) {
	body, _, err := s.Open(ctx, from)
	if err != nil {
		return "", err
	}
	defer body.Close()
	obj, err := s.Put(ctx, to, body)
	if err != nil {
		return "", err
	}
	return obj.Key, nil
}
