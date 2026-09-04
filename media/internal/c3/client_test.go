package c3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"aura/media"
	"aura/media/internal/assets"
	"aura/media/internal/ingest"
	"aura/media/internal/queue"
)

// The manifest is the contract the kernel routes by. If it says asset_in and
// this client listens on something else, the kernel delivers into nothing.
func TestManifestDeclaresThePortsThisClientServes(t *testing.T) {
	raw, err := ManifestJSON(media.SkillYAML, "1.2.3")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var doc struct {
		ID         string `json:"id"`
		Version    string `json:"version"`
		Protocol   string `json:"protocol"`
		Capability string `json:"capability"`
		Type       string `json:"type"`
		Format     string `json:"format"`
		Ports      struct {
			Ingress []struct{ Name, Schema string } `json:"ingress"`
			Egress  []struct{ Name, Schema string } `json:"egress"`
		} `json:"ports"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the manifest is not JSON the kernel can read: %v", err)
	}
	if doc.Capability != "logical.media.transcode" || doc.Type != "logical" {
		t.Errorf("capability %q / type %q", doc.Capability, doc.Type)
	}
	if doc.Protocol != Protocol {
		t.Errorf("protocol %q, want %q", doc.Protocol, Protocol)
	}
	if len(doc.Ports.Ingress) != 1 || doc.Ports.Ingress[0].Name != PortIn || doc.Ports.Ingress[0].Schema != Schema {
		t.Errorf("ingress ports %+v, want one %s carrying %s", doc.Ports.Ingress, PortIn, Schema)
	}
	if len(doc.Ports.Egress) != 1 || doc.Ports.Egress[0].Name != PortOut || doc.Ports.Egress[0].Schema != Schema {
		t.Errorf("egress ports %+v, want one %s carrying %s", doc.Ports.Egress, PortOut, Schema)
	}
	// The build's version wins over the file's, so a released binary does not
	// register as whatever number was last committed.
	if doc.Version != "1.2.3" {
		t.Errorf("version %q, want the build's 1.2.3", doc.Version)
	}
	// ...but only when it is a real version. `dev` is not one.
	dev, err := ManifestJSON(media.SkillYAML, "dev")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if !strings.Contains(string(dev), `"version":"0.1.0"`) {
		t.Error("a non-semver build version overwrote the manifest's")
	}
}

// fakeIngest and fakeCatalogue stand in for the service behind the client.
type fakeIngest struct {
	mu      sync.Mutex
	calls   []string
	idems   []string
	err     error
	assetID string
}

func (f *fakeIngest) FromURI(_ context.Context, uri string, req ingest.Request) (ingest.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return ingest.Result{}, f.err
	}
	f.calls = append(f.calls, uri)
	f.idems = append(f.idems, req.Idem)
	return ingest.Result{
		Asset: assets.Asset{ID: f.assetID, State: assets.StateReceived},
		Job:   queue.Job{ID: 1, AssetID: f.assetID, State: queue.StateQueued},
	}, nil
}

func (f *fakeIngest) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeCatalogue struct {
	mu    sync.Mutex
	asset assets.Asset
}

func (f *fakeCatalogue) Get(_ context.Context, _ string) (assets.Asset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asset, nil
}

func (f *fakeCatalogue) settle(a assets.Asset) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asset = a
}

type fakeStore struct{}

func (fakeStore) Address(key string) string { return "/media/" + key }

// kernel is a fake node: it accepts one connection, acknowledges the register,
// and hands the test the frames the client sends.
type kernel struct {
	server   *httptest.Server
	conn     *websocket.Conn
	incoming chan Envelope
	register chan Envelope
	writeMu  sync.Mutex
}

func newKernel(t *testing.T) *kernel {
	t.Helper()
	k := &kernel{incoming: make(chan Envelope, 32), register: make(chan Envelope, 1)}
	upgrader := websocket.Upgrader{}
	k.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer node-token" {
			// The kernel's routes are behind its token, /ws/skill included.
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		k.conn = conn
		for {
			var env Envelope
			if err := conn.ReadJSON(&env); err != nil {
				close(k.incoming)
				return
			}
			if env.Kind == KindRegister {
				k.register <- env
				_ = k.send(Envelope{V: Protocol, ID: "ack", Kind: KindStatus,
					Payload: json.RawMessage(`{"state":"registered","skill":"deepaxiom/logical/media-transcode"}`)})
				continue
			}
			k.incoming <- env
		}
	}))
	t.Cleanup(k.server.Close)
	return k
}

func (k *kernel) url() string { return "ws" + strings.TrimPrefix(k.server.URL, "http") + "/ws/skill" }

func (k *kernel) send(env Envelope) error {
	k.writeMu.Lock()
	defer k.writeMu.Unlock()
	return k.conn.WriteJSON(env)
}

func (k *kernel) next(t *testing.T, kind string) Envelope {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case env, ok := <-k.incoming:
			if !ok {
				t.Fatalf("the connection closed while waiting for a %q frame", kind)
			}
			if kind == "" || env.Kind == kind {
				return env
			}
		case <-deadline:
			t.Fatalf("no %q frame arrived", kind)
		}
	}
}

func startClient(t *testing.T, k *kernel, in Ingestor, cat Catalogue) {
	t.Helper()
	manifest, err := ManifestJSON(media.SkillYAML, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{
		URL:      k.url(),
		Token:    "node-token",
		Manifest: manifest,
		Ingest:   in,
		Assets:   cat,
		Store:    fakeStore{},
		Poll:     10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go client.Run(ctx)
	select {
	case <-k.register:
	case <-time.After(5 * time.Second):
		t.Fatal("the client never registered")
	}
}

func TestOneDeliveryBecomesOnePackagedAsset(t *testing.T) {
	k := newKernel(t)
	in := &fakeIngest{assetID: "01ASSET"}
	cat := &fakeCatalogue{asset: assets.Asset{ID: "01ASSET", State: assets.StateProcessing}}
	startClient(t, k, in, cat)

	if err := k.send(Envelope{
		V: Protocol, ID: "01IN", Kind: KindData, Session: "sess-1", Port: PortIn,
		Idem: "sess-1:client:asset_in:1", Schema: Schema,
		Payload: json.RawMessage(`{"uri":"https://example.test/clip.mp4","mime":"video/mp4"}`),
	}); err != nil {
		t.Fatal(err)
	}

	// While it works, it says so, rather than going silent for four minutes.
	status := k.next(t, KindStatus)
	if status.CauseID != "01IN" || status.Session != "sess-1" {
		t.Errorf("status is not on the chain that asked: %+v", status)
	}

	cat.settle(assets.Asset{
		ID: "01ASSET", State: assets.StateReady, Address: "/media/out/01ASSET/master.m3u8",
		Outputs: assets.Outputs{
			DurationMs: 12000,
			Renditions: []assets.Rendition{{Name: "720p", Height: 720, Playlist: "out/01ASSET/720p/index.m3u8"}},
			Frames:     []string{"out/01ASSET/frames/frame_00001.jpg"},
			Poster:     "out/01ASSET/poster.jpg",
		},
	})

	data := k.next(t, KindData)
	if data.CauseID != "01IN" {
		t.Errorf("cause_id is %q, want the envelope that caused it", data.CauseID)
	}
	if data.Port != PortOut || data.Schema != Schema {
		t.Errorf("delivered on %q as %q", data.Port, data.Schema)
	}
	if data.Seq != 2 {
		t.Errorf("seq is %d; it must be monotonic per session and port so a gap is detectable", data.Seq)
	}
	if data.Idem == "" {
		t.Error("no idempotency key: a receiver has nothing to deduplicate by")
	}
	var out MediaAsset
	if err := json.Unmarshal(data.Payload, &out); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if out.URI != "/media/out/01ASSET/master.m3u8" || out.State != "ready" {
		t.Errorf("payload is %+v", out)
	}
	if len(out.Frames) != 1 || out.Frames[0] != "/media/out/01ASSET/frames/frame_00001.jpg" {
		t.Errorf("frames came back as keys rather than addresses: %v", out.Frames)
	}
	if out.Poster != "/media/out/01ASSET/poster.jpg" {
		t.Errorf("poster is %q", out.Poster)
	}

	// done closes the stream: a consumer that never sees it waits forever.
	if got := k.next(t, ""); got.Kind != KindDone {
		t.Errorf("after the data came a %q, want done", got.Kind)
	}

	// The envelope's idempotency key rode into the queue, so a redelivery of
	// this same envelope cannot start a second encode even across a restart.
	if len(in.idems) != 1 || !strings.Contains(in.idems[0], "sess-1:client:asset_in:1") {
		t.Errorf("job idempotency key is %v", in.idems)
	}
}

// C3 is at-least-once. A redelivered envelope must not be encoded twice.
func TestRedeliveryIsIgnored(t *testing.T) {
	k := newKernel(t)
	in := &fakeIngest{assetID: "01ASSET"}
	cat := &fakeCatalogue{asset: assets.Asset{
		ID: "01ASSET", State: assets.StateReady, Address: "/media/out/01ASSET/master.m3u8",
	}}
	startClient(t, k, in, cat)

	env := Envelope{
		V: Protocol, ID: "01IN", Kind: KindData, Session: "sess-1", Port: PortIn,
		Idem: "sess-1:client:asset_in:1", Schema: Schema,
		Payload: json.RawMessage(`{"uri":"https://example.test/clip.mp4"}`),
	}
	if err := k.send(env); err != nil {
		t.Fatal(err)
	}
	k.next(t, KindDone)
	if err := k.send(env); err != nil {
		t.Fatal(err)
	}
	// Give the duplicate time to be wrongly acted on.
	time.Sleep(300 * time.Millisecond)
	if got := in.count(); got != 1 {
		t.Errorf("ingested %d times for one envelope delivered twice", got)
	}
	select {
	case extra := <-k.incoming:
		t.Errorf("a redelivered envelope produced another %q frame", extra.Kind)
	default:
	}
}

// A cancel names the id the kernel delivered to THIS skill; anything else is
// silently ignored, which is the failure this checks against.
func TestCancelStopsWaitingOnTheNamedChain(t *testing.T) {
	k := newKernel(t)
	in := &fakeIngest{assetID: "01ASSET"}
	cat := &fakeCatalogue{asset: assets.Asset{ID: "01ASSET", State: assets.StateProcessing}}
	startClient(t, k, in, cat)

	if err := k.send(Envelope{
		V: Protocol, ID: "01IN", Kind: KindData, Session: "sess-1", Port: PortIn,
		Idem: "sess-1:client:asset_in:1", Schema: Schema,
		Payload: json.RawMessage(`{"uri":"https://example.test/clip.mp4"}`),
	}); err != nil {
		t.Fatal(err)
	}
	k.next(t, KindStatus)

	if err := k.send(Envelope{V: Protocol, ID: "01CANCEL", Kind: KindCancel, Session: "sess-1", CauseID: "01IN"}); err != nil {
		t.Fatal(err)
	}
	// The asset later becomes ready. Nothing may be sent for a chain that was
	// abandoned: the kernel would drop it anyway, and the work is wasted.
	time.Sleep(200 * time.Millisecond)
	cat.settle(assets.Asset{ID: "01ASSET", State: assets.StateReady, Address: "/media/out/01ASSET/master.m3u8"})
	time.Sleep(300 * time.Millisecond)

	select {
	case env := <-k.incoming:
		t.Errorf("a cancelled chain still emitted a %q frame", env.Kind)
	default:
	}
}

func TestABadPayloadIsRefusedOnTheChainThatSentIt(t *testing.T) {
	k := newKernel(t)
	in := &fakeIngest{assetID: "01ASSET"}
	startClient(t, k, in, &fakeCatalogue{})

	if err := k.send(Envelope{
		V: Protocol, ID: "01IN", Kind: KindData, Session: "sess-1", Port: PortIn,
		Idem: "x1", Schema: Schema, Payload: json.RawMessage(`{"mime":"video/mp4"}`),
	}); err != nil {
		t.Fatal(err)
	}
	env := k.next(t, KindError)
	if !strings.Contains(string(env.Payload), "uri") {
		t.Errorf("the refusal does not say what was missing: %s", env.Payload)
	}
	if in.count() != 0 {
		t.Error("an envelope with no uri still reached the ingestor")
	}
}

func TestAnIngestFailureIsReportedRatherThanSwallowed(t *testing.T) {
	k := newKernel(t)
	in := &fakeIngest{assetID: "01ASSET", err: errors.New("this node does not fetch sources over HTTP")}
	startClient(t, k, in, &fakeCatalogue{})

	if err := k.send(Envelope{
		V: Protocol, ID: "01IN", Kind: KindData, Session: "sess-1", Port: PortIn,
		Idem: "x1", Schema: Schema, Payload: json.RawMessage(`{"uri":"https://example.test/clip.mp4"}`),
	}); err != nil {
		t.Fatal(err)
	}
	env := k.next(t, KindError)
	if !strings.Contains(string(env.Payload), "does not fetch") {
		t.Errorf("the error does not carry the reason: %s", env.Payload)
	}
}

// A producer that omits `idem` must get no deduplication — not somebody else's
// asset. Before the fallback to the envelope id, every keyless envelope in one
// session shared the job key "<session>:", so the second video sent in a
// session was answered with the first one's address.
func TestKeylessEnvelopesDoNotShareOneIdempotencyKey(t *testing.T) {
	k := newKernel(t)
	in := &fakeIngest{assetID: "01ASSET"}
	cat := &fakeCatalogue{asset: assets.Asset{
		ID: "01ASSET", State: assets.StateReady, Address: "/media/out/01ASSET/master.m3u8",
	}}
	startClient(t, k, in, cat)

	for i, uri := range []string{"https://example.test/first.mp4", "https://example.test/second.mp4"} {
		if err := k.send(Envelope{
			V: Protocol, ID: fmt.Sprintf("01IN%d", i), Kind: KindData, Session: "sess-1",
			Port: PortIn, Schema: Schema,
			Payload: json.RawMessage(fmt.Sprintf(`{"uri":%q}`, uri)),
		}); err != nil {
			t.Fatal(err)
		}
		k.next(t, KindDone)
	}

	if in.count() != 2 {
		t.Fatalf("two different videos produced %d ingests", in.count())
	}
	if in.idems[0] == in.idems[1] {
		t.Errorf("both envelopes were given the same job key %q, so the second video would be answered with the first one's address", in.idems[0])
	}
	for _, key := range in.idems {
		if strings.HasSuffix(key, ":") {
			t.Errorf("job key %q is just the session: every keyless envelope in it would collide", key)
		}
	}
}
