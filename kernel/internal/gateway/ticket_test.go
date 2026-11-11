package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/channel"
)

func TestTicketRoundTripAndTampering(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_800_000_000, 0)
	good := signTicket(key, ticketClaims{V: ticketVersion, Session: "sess-1", Expires: now.Unix() + 60})

	claims, err := verifyTicket(key, good, now)
	if err != nil || claims.Session != "sess-1" {
		t.Fatalf("a valid ticket was refused: %v %+v", err, claims)
	}

	payload, sig, _ := strings.Cut(good, ".")
	other := signTicket(key, ticketClaims{V: ticketVersion, Session: "sess-2", Expires: now.Unix() + 60})
	otherPayload, _, _ := strings.Cut(other, ".")
	bad := map[string]string{
		"expired":                   signTicket(key, ticketClaims{V: ticketVersion, Session: "sess-1", Expires: now.Unix()}),
		"signed with another key":   signTicket([]byte("another-key-another-key-another!"), ticketClaims{V: ticketVersion, Session: "sess-1", Expires: now.Unix() + 60}),
		"another session's payload": otherPayload + "." + sig,
		"truncated signature":       payload + "." + sig[:len(sig)-2],
		"no signature":              payload,
		"empty":                     "",
		"unknown version":           signTicket(key, ticketClaims{V: 9, Session: "sess-1", Expires: now.Unix() + 60}),
		"no session":                signTicket(key, ticketClaims{V: ticketVersion, Expires: now.Unix() + 60}),
		"oversized":                 strings.Repeat("a", 2000) + "." + sig,
		"not base64":                "!!!." + sig,
		"the operator token itself": "s3cret",
	}
	for name, ticket := range bad {
		if _, err := verifyTicket(key, ticket, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTheSameOperatorTokenDerivesTheSameKey(t *testing.T) {
	// What lets a standby honour tickets the node it replaced had issued.
	var a, b, c ticketKeys
	if !bytes.Equal(a.get("s3cret"), b.get("s3cret")) {
		t.Fatal("two processes with the same operator token derive different ticket keys")
	}
	if bytes.Equal(a.get("s3cret"), c.get("other")) {
		t.Fatal("different operator tokens derive the same ticket key")
	}
	if bytes.Equal(a.get("s3cret"), []byte("s3cret")) {
		t.Fatal("the ticket key is the operator token itself")
	}
}

// A ticket opens one route and nothing else.
func TestAWatchPrincipalReachesOnlyTheWatchRoute(t *testing.T) {
	p := Principal{Scope: ScopeWatch, Session: "sess-1"}
	if !p.Allows(http.MethodGet, "/v1/watch") {
		t.Fatal("a watch ticket cannot reach /v1/watch")
	}
	for _, route := range [][2]string{
		{http.MethodGet, "/v1/stream"}, {http.MethodGet, "/ws/skill"}, {http.MethodGet, "/ws/expertise"},
		{http.MethodGet, "/v1/sessions"}, {http.MethodGet, "/v1/sessions/sess-1/events"},
		{http.MethodGet, "/v1/ledger"}, {http.MethodGet, "/v1/expertise"}, {http.MethodGet, "/v1/stats"},
		{http.MethodPost, "/v1/graphs"}, {http.MethodPost, "/v1/watch/tickets"},
		{http.MethodPost, "/v1/watch"}, {http.MethodGet, "/v1/watch/tickets"},
		{http.MethodPost, "/v1/secrets/resolve"}, {http.MethodGet, "/metrics"},
	} {
		if p.Allows(route[0], route[1]) {
			t.Errorf("a watch ticket may %s %s", route[0], route[1])
		}
	}
	if p.IsOperator() || p.MayRegister("logical.echo") || ScopeWatch.Valid() {
		t.Fatal("a watch principal has an authority beyond watching")
	}
}

// Through the real middleware: a ticket is honoured on /v1/watch as a query
// parameter and nowhere else in any form.
func TestTicketAuthenticationThroughTheMiddleware(t *testing.T) {
	auth := &Auth{Token: "s3cret"}
	h := auth.Authenticate(okHandler())
	ticket := signTicket(auth.tickets.get(auth.Token),
		ticketClaims{V: ticketVersion, Session: "sess-1", Expires: time.Now().Unix() + 60})

	if code := request(t, h, http.MethodGet, "/v1/watch?session=sess-1&ticket="+ticket, nil); code != http.StatusOK {
		t.Fatalf("a valid ticket on /v1/watch returned %d", code)
	}
	refused := []struct {
		name, method, path string
		hdr                http.Header
	}{
		{"as a bearer token", http.MethodGet, "/v1/watch?session=sess-1", bearer(ticket)},
		{"on the session socket", http.MethodGet, "/v1/stream?graph=echo&ticket=" + ticket, nil},
		{"as ?token=", http.MethodGet, "/v1/watch?session=sess-1&token=" + ticket, nil},
		{"on the catalog", http.MethodGet, "/v1/expertise?ticket=" + ticket, nil},
		{"to mint another", http.MethodPost, "/v1/watch/tickets?ticket=" + ticket, nil},
		{"a forged one", http.MethodGet, "/v1/watch?session=sess-1&ticket=" + ticket + "x", nil},
	}
	for _, tc := range refused {
		if code := request(t, h, tc.method, tc.path, tc.hdr); code != http.StatusUnauthorized {
			t.Errorf("a ticket %s returned %d; want 401", tc.name, code)
		}
	}
}

func issue(t *testing.T, h http.Handler, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/watch/tickets", strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestIssuingATicket(t *testing.T) {
	srv, _, session := liveEchoSession(t, 1)
	h := srv.Config.Handler

	code, out := issue(t, h, fmt.Sprintf(`{"session":%q}`, session))
	if code != http.StatusOK || out["ticket"] == "" || out["session"] != session {
		t.Fatalf("issue: %d %v", code, out)
	}
	expires := int64(out["expires"].(float64))
	if d := expires - time.Now().Unix(); d < 240 || d > 330 {
		t.Fatalf("default lifetime is %ds; want about five minutes", d)
	}

	if code, _ := issue(t, h, `{"session":"not-a-live-session"}`); code != http.StatusNotFound {
		t.Errorf("a ticket for a session that is not live: %d", code)
	}
	if code, _ := issue(t, h, `{}`); code != http.StatusBadRequest {
		t.Errorf("a ticket with no session: %d", code)
	}
	if code, _ := issue(t, h, fmt.Sprintf(`{"session":%q,"ttl_s":999999}`, session)); code != http.StatusBadRequest {
		t.Errorf("a ticket that outlives a day: %d", code)
	}
	if code, _ := issue(t, h, `not json`); code != http.StatusBadRequest {
		t.Errorf("a malformed body: %d", code)
	}
}

// The whole path a viewer takes: the application asks for a ticket, the viewer
// connects with it and nothing else, and sees the session.
func TestAViewerWatchesWithOnlyATicket(t *testing.T) {
	srv, client, session := liveEchoSession(t, 3)
	_, out := issue(t, srv.Config.Handler, fmt.Sprintf(`{"session":%q}`, session))
	ticket := out["ticket"].(string)

	// The session id is implied by the ticket; a viewer need not know it.
	viewer, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/v1/watch?ticket="+ticket), nil)
	if err != nil {
		t.Fatalf("a viewer holding a valid ticket could not connect: %v", err)
	}
	defer viewer.Close()
	readKind(t, viewer, channel.KindStatus)

	_ = client.WriteMessage(websocket.TextMessage, []byte(`{"text":"go"}`))
	for i := 0; i < 3; i++ {
		got := readKind(t, viewer, channel.KindData)
		want := readKind(t, client, channel.KindData)
		if got.ID != want.ID {
			t.Fatalf("frame %d: viewer %s, client %s", i, got.ID, want.ID)
		}
	}
}

func TestATicketDoesNotOpenAnotherSession(t *testing.T) {
	srv, _, session := liveEchoSession(t, 1)
	_, out := issue(t, srv.Config.Handler, fmt.Sprintf(`{"session":%q}`, session))
	ticket := out["ticket"].(string)

	other, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/v1/stream?graph=live"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	hello := readKind(t, other, channel.KindStatus)
	var p struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(hello.Payload, &p)

	_, resp, err := websocket.DefaultDialer.Dial(
		wsURL(srv, "/v1/watch?session="+p.Session+"&ticket="+ticket), nil)
	if err == nil {
		t.Fatal("a ticket for one session opened another")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %v", resp)
	}
}

// ── history ────────────────────────────────────────────────────────────────

func drainStatusState(t *testing.T, c *websocket.Conn) (state string, count int) {
	t.Helper()
	env := readKind(t, c, channel.KindStatus)
	var p struct {
		State string `json:"state"`
		Count int    `json:"count"`
	}
	_ = json.Unmarshal(env.Payload, &p)
	return p.State, p.Count
}

func TestHistoryReplaysWhatTheClientWasSentThenGoesLive(t *testing.T) {
	srv, client, session := liveEchoSession(t, 4)

	// Two exchanges before anyone is watching.
	var sent []string
	for round := 0; round < 2; round++ {
		_ = client.WriteMessage(websocket.TextMessage, []byte(`{"text":"go"}`))
		for i := 0; i < 4; i++ {
			sent = append(sent, readKind(t, client, channel.KindData).ID)
		}
	}

	viewer, _, err := websocket.DefaultDialer.Dial(
		wsURL(srv, "/v1/watch?session="+session+"&history=6"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()

	state, count := drainStatusState(t, viewer)
	if state != "history" || count != 6 {
		t.Fatalf("first status is %q with count %d; want history/6", state, count)
	}
	var replayed []string
	for i := 0; i < 6; i++ {
		replayed = append(replayed, readKind(t, viewer, channel.KindData).ID)
	}
	if fmt.Sprint(replayed) != fmt.Sprint(sent[len(sent)-6:]) {
		t.Fatalf("replay was %v; the client's last six were %v", replayed, sent[len(sent)-6:])
	}
	if state, _ := drainStatusState(t, viewer); state != "watching" {
		t.Fatalf("after the replay the status is %q; want watching", state)
	}

	// And then live, with nothing repeated.
	_ = client.WriteMessage(websocket.TextMessage, []byte(`{"text":"go"}`))
	seen := map[string]bool{}
	for _, id := range replayed {
		seen[id] = true
	}
	for i := 0; i < 4; i++ {
		live := readKind(t, viewer, channel.KindData)
		want := readKind(t, client, channel.KindData)
		if live.ID != want.ID {
			t.Fatalf("live frame %d: viewer %s, client %s", i, live.ID, want.ID)
		}
		if seen[live.ID] {
			t.Fatalf("frame %s was delivered in the replay and again live", live.ID)
		}
	}
}

func TestHistoryIsWhatTheClientReceivedNotWhatItSent(t *testing.T) {
	srv, client, session := liveEchoSession(t, 1)
	_ = client.WriteMessage(websocket.TextMessage, []byte(`{"text":"something only the client typed"}`))
	readKind(t, client, channel.KindData)

	viewer, _, err := websocket.DefaultDialer.Dial(
		wsURL(srv, "/v1/watch?session="+session+"&history=50"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	_, count := drainStatusState(t, viewer)
	if count != 1 {
		t.Fatalf("history holds %d envelopes; the client was sent exactly one", count)
	}
	env := readKind(t, viewer, channel.KindData)
	if env.Port != "text_in" || strings.Contains(string(env.Payload), "only the client typed") {
		t.Fatalf("history replayed the client's own input: port %s payload %s", env.Port, env.Payload)
	}
}

func TestHistoryParameterIsValidatedAndCapped(t *testing.T) {
	srv, _, session := liveEchoSession(t, 1)
	for _, bad := range []string{"-1", "many", "1.5"} {
		_, resp, err := websocket.DefaultDialer.Dial(
			wsURL(srv, "/v1/watch?session="+session+"&history="+bad), nil)
		if err == nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
			t.Errorf("history=%s: err=%v resp=%v", bad, err, resp)
		}
	}
	// Far over the cap is accepted and clamped, not refused: a viewer asking
	// for "everything" should get what is allowed.
	viewer, _, err := websocket.DefaultDialer.Dial(
		wsURL(srv, "/v1/watch?session="+session+"&history=1000000"), nil)
	if err != nil {
		t.Fatalf("an oversized history request was refused: %v", err)
	}
	viewer.Close()
}

func TestWatchGateHoldsThenPassesThroughWithoutDuplicates(t *testing.T) {
	var out []string
	g := &watchGate{out: func(raw []byte) bool { out = append(out, string(raw)); return true }}
	g.offer([]byte(`{"id":"a"}`))
	g.offer([]byte(`{"id":"b"}`))
	if len(out) != 0 {
		t.Fatal("a frame passed the gate before it was opened")
	}
	if !g.open(map[string]bool{"a": true}) {
		t.Fatal("open reported the watcher lost")
	}
	g.offer([]byte(`{"id":"c"}`))
	if fmt.Sprint(out) != `[{"id":"b"} {"id":"c"}]` {
		t.Fatalf("after opening, the watcher was sent %v", out)
	}
}

func TestWatchGateCutsOffAWatcherThatFallsBehindDuringReplay(t *testing.T) {
	g := &watchGate{out: func([]byte) bool { return true }}
	for i := 0; i < maxWatchPending; i++ {
		if !g.offer([]byte(`{}`)) {
			t.Fatalf("frame %d refused inside the bound", i)
		}
	}
	if g.offer([]byte(`{}`)) {
		t.Fatal("the gate held more than its bound")
	}
	if g.open(nil) {
		t.Fatal("a watcher that overflowed during replay was let through with a hole in its stream")
	}
}
