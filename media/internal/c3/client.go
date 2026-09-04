// Package c3 connects this service to a kernel as a skill.
//
// The service is useful on its own — upload, poll, fetch an address — but a
// graph cannot call an HTTP API it was never told about. Registering makes the
// same work reachable as a capability: an envelope naming a video arrives on
// asset_in, an envelope naming the packaged one leaves on asset_out, and the
// encode happens in this process where it cannot make the kernel wait.
package c3

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"

	"aura/media/internal/assets"
	"aura/media/internal/ingest"
)

// Protocol major this client speaks.
const Protocol = "1"

// Envelope kinds, from C3.
const (
	KindData         = "data"
	KindDone         = "done"
	KindError        = "error"
	KindStatus       = "status"
	KindRegister     = "register"
	KindCancel       = "cancel"
	KindConfigUpdate = "config_update"
)

// Ports this skill declares. They must match skill.yaml, which is why the
// manifest is parsed rather than described twice.
const (
	PortIn  = "asset_in"
	PortOut = "asset_out"
	Schema  = "std/media-asset@1"
)

// Envelope is the C3 wire format.
type Envelope struct {
	V       string          `json:"v"`
	ID      string          `json:"id"`
	CauseID string          `json:"cause_id,omitempty"`
	Session string          `json:"session,omitempty"`
	Node    string          `json:"node,omitempty"`
	Port    string          `json:"port,omitempty"`
	Seq     int             `json:"seq,omitempty"`
	Idem    string          `json:"idem,omitempty"`
	Schema  string          `json:"schema,omitempty"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// MediaAsset is std/media-asset@1.
type MediaAsset struct {
	URI        string             `json:"uri"`
	MIME       string             `json:"mime,omitempty"`
	AssetID    string             `json:"asset_id,omitempty"`
	State      string             `json:"state,omitempty"`
	DurationMs int64              `json:"duration_ms,omitempty"`
	Renditions []assets.Rendition `json:"renditions,omitempty"`
	Frames     []string           `json:"frames,omitempty"`
	Poster     string             `json:"poster,omitempty"`
	Error      string             `json:"error,omitempty"`
}

// Ingestor is what turns a named source into queued work.
type Ingestor interface {
	FromURI(ctx context.Context, uri string, req ingest.Request) (ingest.Result, error)
}

// Catalogue is how the client watches an asset reach its end state.
type Catalogue interface {
	Get(ctx context.Context, id string) (assets.Asset, error)
}

// Addresser turns a stored key into the address a consumer can fetch.
type Addresser interface {
	Address(key string) string
}

// Client is one connection to one kernel.
type Client struct {
	URL      string
	Token    string
	Manifest []byte // the C1 manifest, as JSON
	Ingest   Ingestor
	Assets   Catalogue
	Store    Addresser
	Log      *slog.Logger

	// Poll is how often an in-flight asset is checked. This is the one place
	// the design is a poll rather than a push, and it is deliberate: the work
	// is minutes long and the row is the only shared state between this
	// connection and the worker that may be in another process entirely.
	Poll time.Duration

	conn     *websocket.Conn
	writeMu  sync.Mutex
	seqMu    sync.Mutex
	seq      map[string]int
	seenMu   sync.Mutex
	seen     map[string]time.Time
	cancelMu sync.Mutex
	cancels  map[string]context.CancelFunc
}

var semver = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// ManifestJSON reads skill.yaml and returns it as the JSON a register frame
// carries, with the version replaced by the build's when there is a real one.
//
// One manifest, parsed — not a second copy written in Go. A skill whose
// declared ports differ from the ports it serves is a skill the kernel routes
// to and that then answers on a port nobody is listening to.
func ManifestJSON(raw []byte, version string) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("skill.yaml: %w", err)
	}
	if semver.MatchString(version) {
		doc["version"] = version
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("skill.yaml as JSON: %w", err)
	}
	return encoded, nil
}

// Run keeps a registration up until ctx ends, reconnecting with backoff.
//
// A kernel restarting must not need this service restarted, and a service that
// gave up after one refused connection would be a service an operator has to
// remember to nudge.
func (c *Client) Run(ctx context.Context) {
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Poll <= 0 {
		c.Poll = time.Second
	}
	backoff := []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}
	attempt := 0
	for ctx.Err() == nil {
		err := c.serve(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := backoff[min(attempt, len(backoff)-1)]
		attempt++
		if err != nil {
			c.Log.Warn("kernel connection ended; retrying", "error", err, "in", wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (c *Client) serve(ctx context.Context) error {
	header := http.Header{}
	if c.Token != "" {
		header.Set("Authorization", "Bearer "+c.Token)
	}
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 15 * time.Second
	conn, resp, err := dialer.DialContext(ctx, c.URL, header)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("the kernel refused this service's token: pass --node-token or set AURA_TOKEN")
		}
		return fmt.Errorf("connecting to %s: %w", c.URL, err)
	}
	defer conn.Close()

	c.conn = conn
	c.seq = map[string]int{}
	c.seen = map[string]time.Time{}
	c.cancels = map[string]context.CancelFunc{}

	if err := c.send(Envelope{V: Protocol, ID: newID(), Kind: KindRegister, Payload: c.Manifest}); err != nil {
		return fmt.Errorf("registering: %w", err)
	}

	// The connection outlives ctx only long enough to unblock the reader.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		var env Envelope
		if err := conn.ReadJSON(&env); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		switch env.Kind {
		case KindStatus:
			c.Log.Info("registered with the kernel", "detail", string(env.Payload))
		case KindError:
			// A register the kernel refused is not something to retry blindly:
			// the manifest is wrong, and reconnecting would say the same thing.
			c.Log.Error("the kernel refused a frame", "detail", string(env.Payload))
		case KindCancel:
			c.cancelChain(env.CauseID)
		case KindConfigUpdate:
			c.Log.Info("configuration changed", "detail", string(env.Payload))
		case KindData:
			if c.duplicate(env) {
				c.Log.Info("ignoring a redelivered envelope", "idem", env.Idem)
				continue
			}
			// One goroutine per delivery: an encode is minutes, and a reader
			// blocked on it would not see the cancel that asks it to stop.
			wg.Add(1)
			go func(in Envelope) {
				defer wg.Done()
				c.handle(ctx, in)
			}(env)
		}
	}
}

// handle turns one inbound envelope into one packaged asset.
func (c *Client) handle(ctx context.Context, in Envelope) {
	var asset MediaAsset
	if err := json.Unmarshal(in.Payload, &asset); err != nil {
		c.fail(in, fmt.Sprintf("the payload is not a std/media-asset@1: %v", err))
		return
	}
	if strings.TrimSpace(asset.URI) == "" {
		c.fail(in, "no `uri`: an envelope names a video, it does not carry one")
		return
	}

	chainCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.trackChain(in, cancel)
	defer c.untrackChain(in)

	// The envelope's own idempotency key becomes the job's, so C3's
	// at-least-once delivery cannot start a second encode of the same video.
	//
	// An envelope that carries no key falls back to its id, which is unique per
	// message. Without that, every keyless envelope in one session would share
	// the key "<session>:" — and the second video sent in that session would be
	// answered with the first one's address. C3 requires the field, but a
	// producer that omits it should get no deduplication, not the wrong asset.
	idem := in.Idem
	if idem == "" {
		idem = in.ID
	}
	result, err := c.Ingest.FromURI(chainCtx, asset.URI, ingest.Request{MIME: asset.MIME, Idem: in.Session + ":" + idem})
	if err != nil {
		c.fail(in, err.Error())
		return
	}
	c.status(in, "working", fmt.Sprintf("asset %s queued", result.Asset.ID))

	final, err := c.await(chainCtx, result.Asset.ID)
	if err != nil {
		if chainCtx.Err() != nil {
			// Cancelled, or the process is stopping. The kernel suppresses the
			// chain either way, so there is nothing useful left to send.
			c.Log.Info("stopped waiting on a cancelled chain", "asset", result.Asset.ID)
			return
		}
		c.fail(in, err.Error())
		return
	}
	if final.State == assets.StateFailed {
		c.fail(in, final.Error)
		return
	}

	out := MediaAsset{
		URI:        final.Address,
		AssetID:    final.ID,
		State:      string(final.State),
		MIME:       "application/vnd.apple.mpegurl",
		DurationMs: final.Outputs.DurationMs,
		Renditions: final.Outputs.Renditions,
		Poster:     c.address(final.Outputs.Poster),
	}
	for _, key := range final.Outputs.Frames {
		out.Frames = append(out.Frames, c.address(key))
	}
	if err := c.emit(in, PortOut, KindData, out); err != nil {
		c.Log.Error("could not deliver the finished asset", "asset", final.ID, "error", err)
		return
	}
	// done closes the logical stream. A consumer that never sees it waits for a
	// second envelope that is never coming.
	if err := c.emit(in, PortOut, KindDone, nil); err != nil {
		c.Log.Error("could not close the stream", "asset", final.ID, "error", err)
	}
}

// await watches an asset until it stops moving.
func (c *Client) await(ctx context.Context, assetID string) (assets.Asset, error) {
	ticker := time.NewTicker(c.Poll)
	defer ticker.Stop()
	for {
		asset, err := c.Assets.Get(ctx, assetID)
		if err != nil {
			return assets.Asset{}, err
		}
		if asset.State == assets.StateReady || asset.State == assets.StateFailed {
			return asset, nil
		}
		select {
		case <-ctx.Done():
			return assets.Asset{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Client) address(key string) string {
	if key == "" || c.Store == nil {
		return ""
	}
	return c.Store.Address(key)
}

func (c *Client) emit(cause Envelope, port, kind string, payload any) error {
	env := Envelope{
		V:       Protocol,
		ID:      newID(),
		CauseID: cause.ID,
		Session: cause.Session,
		Port:    port,
		Kind:    kind,
	}
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		env.Payload = encoded
		env.Schema = Schema
	}
	env.Seq = c.nextSeq(cause.Session, port)
	// C3 requires the key to be unique within a session; deriving it from the
	// incoming one keeps it stable if this reply is ever produced twice.
	env.Idem = fmt.Sprintf("%s:%s:%d", cause.Idem, port, env.Seq)
	return c.send(env)
}

func (c *Client) status(cause Envelope, state, detail string) {
	if err := c.emit(cause, PortOut, KindStatus, map[string]string{"state": state, "detail": detail}); err != nil {
		c.Log.Debug("could not send a status", "error", err)
	}
}

// fail says why, on the chain that asked. An error envelope is terminal, so it
// is the last thing this chain sends.
func (c *Client) fail(cause Envelope, detail string) {
	c.Log.Warn("refusing a delivery", "detail", detail, "session", cause.Session)
	if err := c.emit(cause, PortOut, KindError, map[string]string{"state": "error", "detail": detail}); err != nil {
		c.Log.Error("could not report a failure", "error", err)
	}
}

func (c *Client) send(env Envelope) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.conn == nil {
		return errors.New("not connected")
	}
	return c.conn.WriteJSON(env)
}

func (c *Client) nextSeq(session, port string) int {
	c.seqMu.Lock()
	defer c.seqMu.Unlock()
	key := session + "\x00" + port
	c.seq[key]++
	return c.seq[key]
}

// duplicate reports whether this envelope has been handled already. C3 delivery
// is at-least-once and every handler must be idempotent; this is the cheap half
// of that, and the job's idempotency key is the half that survives a restart.
func (c *Client) duplicate(env Envelope) bool {
	if env.Idem == "" {
		return false
	}
	key := env.Session + "\x00" + env.Idem
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	if _, ok := c.seen[key]; ok {
		return true
	}
	if len(c.seen) > 4096 {
		cutoff := time.Now().Add(-time.Hour)
		for k, at := range c.seen {
			if at.Before(cutoff) {
				delete(c.seen, k)
			}
		}
	}
	c.seen[key] = time.Now()
	return false
}

func (c *Client) trackChain(in Envelope, cancel context.CancelFunc) {
	c.cancelMu.Lock()
	defer c.cancelMu.Unlock()
	c.cancels[in.Session+"\x00"+in.ID] = cancel
}

func (c *Client) untrackChain(in Envelope) {
	c.cancelMu.Lock()
	defer c.cancelMu.Unlock()
	delete(c.cancels, in.Session+"\x00"+in.ID)
}

// cancelChain honours a cancel. C3 says the cause_id carried is the id of what
// the kernel delivered to THIS skill, so that is what is matched.
func (c *Client) cancelChain(causeID string) {
	if causeID == "" {
		return
	}
	c.cancelMu.Lock()
	defer c.cancelMu.Unlock()
	for key, cancel := range c.cancels {
		if strings.HasSuffix(key, "\x00"+causeID) {
			c.Log.Info("cancelled", "cause", causeID)
			cancel()
		}
	}
}

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newID mints a C3 message id: sortable by time, unique per message.
func newID() string {
	ms := time.Now().UnixMilli()
	out := make([]byte, 26)
	for i := 9; i >= 0; i-- {
		out[i] = alphabet[ms&31]
		ms >>= 5
	}
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	for i, b := range buf {
		out[10+i] = alphabet[b&31]
	}
	return string(out)
}
