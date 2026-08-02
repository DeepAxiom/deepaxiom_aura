package gateway

// The OpenEnv border — this node as a Hugging Face OpenEnv environment.
//
// OpenEnv is Hugging Face's contract for agentic-RL environments: a
// Gymnasium-shaped `reset` / `step` / `state` over HTTP, publishable to Spaces
// and consumed by TRL, torchforge, SkyRL and the rest of the training stack.
// It is small enough to implement in one file and is where the research
// audience already is.
//
// This is a *border*, in the same sense as the MCP server and the A2A card:
// userland-shaped code speaking someone else's protocol at the edge, with no
// special privileges inside the kernel. A graph becomes an environment; an
// episode becomes a session; a step becomes one turn through that session.
//
// # What makes this worth doing rather than merely compatible
//
// Every other OpenEnv environment returns an observation and a reward. This
// one can also return **the audit bundle for the episode** — the trajectory,
// a verifiable receipt per effect that reached the world, and the model
// configuration that argued for each one.
//
// That matters because of what agent evaluation currently cannot do. A 2026
// study of benchmark protocols (arXiv 2607.22368) found 67% of examined traces
// contained paths by which a score could be earned without the capability
// being measured, and concluded that reports should ship the evidence needed
// to interpret them rather than a number. An environment that hands back a
// tamper-evident record of what actually happened during the episode is the
// missing half of that: the score and the receipts arrive together.
//
// # The mapping, and where it is lossy
//
//	OpenEnv        AURA                     note
//	environment    a registered graph       one env per graph
//	episode        a session                reset opens one, and closes any prior
//	action         a client message         text in, on client.text_out
//	observation    what the graph replied   collected until the turn settles
//	reward         NOT computed here        see below
//	done           the session ended        or the caller ended the episode
//
// **Reward is deliberately absent.** A runtime cannot know what a task counts
// as success, and inventing a number would be worse than returning none — it
// would be a score with no protocol behind it, which is the failure the study
// above is about. The reward belongs to the environment author, who computes
// it from the observation and the bundle. `reward` is present in the response
// and always null, so the field exists for a wrapper to fill.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/ledger"
)

const (
	// maxActionBytes bounds one step's action payload.
	maxActionBytes = 1 << 20
	// stepSettleWindow is how long a step waits for the graph to go quiet
	// after its last output before calling the turn done.
	//
	// A turn has no explicit end in a streaming runtime: a graph may emit a
	// token, pause to call a tool, and emit more. Quiescence is the honest
	// signal, and the window is reported back in the observation so a caller
	// tuning throughput knows what it is trading.
	stepSettleWindow = 400 * time.Millisecond
	// stepMaxWait caps a step regardless of quiescence, so a wedged graph
	// times out into an observation rather than hanging a training loop.
	stepMaxWait = 120 * time.Second
)

// openEnvEpisode is one live episode: a session plus what it has emitted.
type openEnvEpisode struct {
	mu        sync.Mutex
	id        string
	graph     string
	sessionID string
	steps     int
	done      bool
	started   time.Time

	// pending collects envelopes the graph sent to the client since the last
	// read, and lastAt marks when the most recent one arrived — quiescence is
	// measured from there.
	pending []channel.Envelope
	lastAt  time.Time
}

// openEnvRegistry holds live episodes for this node.
type openEnvRegistry struct {
	mu       sync.Mutex
	episodes map[string]*openEnvEpisode
}

func (g *Gateway) openEnv() *openEnvRegistry {
	g.openEnvOnce.Do(func() {
		g.openEnvReg = &openEnvRegistry{episodes: map[string]*openEnvEpisode{}}
	})
	return g.openEnvReg
}

// openEnvSpec describes this node as an environment (GET /openenv/spec).
//
// OpenEnv discovery: what environments exist here, and what an action looks
// like. One environment per registered graph, because a graph is exactly "a
// configured task this node knows how to run".
func (g *Gateway) openEnvSpec(w http.ResponseWriter, _ *http.Request) {
	graphs, err := g.St.ListGraphs()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	envs := make([]map[string]any, 0, len(graphs))
	for _, id := range graphs {
		envs = append(envs, map[string]any{
			"id":                id,
			"action_space":      map[string]any{"type": "text", "schema": "std/text@1"},
			"observation_space": map[string]any{"type": "text", "schema": "std/text@1"},
		})
	}
	writeJSON(w, 200, map[string]any{
		"openenv":      "1",
		"runtime":      "deepaxiom-aura",
		"node":         g.Node.ID,
		"environments": envs,
		// Stated in the spec so a consumer never has to discover it by finding
		// a suspicious zero in their training curve.
		"reward": map[string]any{
			"provided": false,
			"note": "this runtime does not compute reward — a runtime cannot know what a " +
				"task counts as success, and a fabricated number is a score with no " +
				"protocol behind it. Compute it from the observation and the audit bundle.",
		},
		"audit_bundle": map[string]any{
			"provided": true,
			"note": "every episode can return a tamper-evident record of what actually " +
				"happened: trajectory, a verifiable receipt per effect, and the model " +
				"configuration behind each one.",
			"endpoint": "/openenv/bundle?episode=<id>",
		},
	})
}

// openEnvReset opens an episode (POST /openenv/reset).
func (g *Gateway) openEnvReset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Environment string `json:"environment"`
		Graph       string `json:"graph"` // alias, so either name works
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, maxActionBytes)).Decode(&body)
	graph := body.Environment
	if graph == "" {
		graph = body.Graph
	}
	if graph == "" {
		writeJSON(w, 400, map[string]string{
			"error": "reset needs an environment id — see GET /openenv/spec"})
		return
	}
	if _, err := g.St.LoadGraph(graph); err != nil {
		writeJSON(w, 404, map[string]string{
			"error": fmt.Sprintf("no environment %q on this node", graph)})
		return
	}

	ep := &openEnvEpisode{
		id:        "ep-" + channel.NewID(),
		graph:     graph,
		sessionID: "sess-" + channel.NewID(),
		started:   time.Now(),
	}
	reg := g.openEnv()
	reg.mu.Lock()
	reg.episodes[ep.id] = ep
	reg.mu.Unlock()

	if err := g.openEnvOpenSession(ep); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"episode_id":  ep.id,
		"observation": map[string]any{"text": "", "final": true},
		"reward":      nil,
		"done":        false,
		"info":        map[string]any{"environment": graph, "session": ep.sessionID},
	})
}

// openEnvOpenSession instantiates the graph behind an episode, routing what
// the session sends to the client into the episode's pending buffer.
func (g *Gateway) openEnvOpenSession(ep *openEnvEpisode) error {
	_, err := g.Mgr.Start(ep.sessionID, ep.graph, "", func(raw []byte, _ string) error {
		var env channel.Envelope
		if json.Unmarshal(raw, &env) != nil {
			return nil
		}
		ep.mu.Lock()
		ep.pending = append(ep.pending, env)
		ep.lastAt = time.Now()
		ep.mu.Unlock()
		return nil
	})
	if err != nil {
		return fmt.Errorf("open environment %q: %w", ep.graph, err)
	}
	return nil
}

// openEnvStep takes one action and returns the observation (POST /openenv/step).
func (g *Gateway) openEnvStep(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EpisodeID string `json:"episode_id"`
		Action    struct {
			Text string `json:"text"`
		} `json:"action"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxActionBytes)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid step: " + err.Error()})
		return
	}
	reg := g.openEnv()
	reg.mu.Lock()
	ep := reg.episodes[body.EpisodeID]
	reg.mu.Unlock()
	if ep == nil {
		writeJSON(w, 404, map[string]string{
			"error": "unknown episode — call POST /openenv/reset first"})
		return
	}
	if ep.done {
		writeJSON(w, 409, map[string]string{
			"error": "this episode has ended; call reset for a new one"})
		return
	}

	sess, ok := g.Mgr.Get(ep.sessionID)
	if !ok {
		writeJSON(w, 410, map[string]string{"error": "the episode's session is gone"})
		return
	}

	ep.mu.Lock()
	ep.pending = nil
	ep.steps++
	step := ep.steps
	ep.mu.Unlock()

	payload, _ := json.Marshal(map[string]any{"text": body.Action.Text, "final": true})
	sess.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), Session: ep.sessionID,
		Node: "client", Port: "text_out", Seq: uint64(step),
		Idem:   fmt.Sprintf("%s:step:%d", ep.sessionID, step),
		Schema: "std/text@1", Kind: channel.KindData, Payload: payload,
	})

	text, done, timedOut := ep.collect()
	if done {
		ep.done = true
	}
	writeJSON(w, 200, map[string]any{
		"episode_id":  ep.id,
		"observation": map[string]any{"text": text, "final": true},
		// Always null, always present. See this file's header: a runtime that
		// invented a reward would be manufacturing a score with no protocol
		// behind it.
		"reward": nil,
		"done":   done,
		"info": map[string]any{
			"step":              step,
			"session":           ep.sessionID,
			"settle_window_ms":  stepSettleWindow.Milliseconds(),
			"timed_out":         timedOut,
			"audit_bundle_path": "/openenv/bundle?episode=" + ep.id,
		},
	})
}

// collect waits for the graph to go quiet and returns what it said.
func (ep *openEnvEpisode) collect() (text string, done bool, timedOut bool) {
	deadline := time.Now().Add(stepMaxWait)
	for {
		time.Sleep(20 * time.Millisecond)

		ep.mu.Lock()
		quiet := !ep.lastAt.IsZero() && time.Since(ep.lastAt) > stepSettleWindow
		gotAnything := len(ep.pending) > 0
		ep.mu.Unlock()

		if gotAnything && quiet {
			break
		}
		if time.Now().After(deadline) {
			timedOut = true
			break
		}
	}

	ep.mu.Lock()
	defer ep.mu.Unlock()
	for _, env := range ep.pending {
		switch env.Kind {
		case channel.KindData:
			var p struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(env.Payload, &p) == nil {
				text += p.Text
			}
		case channel.KindError:
			done = true
		case channel.KindDone:
			done = true
		}
	}
	ep.pending = nil
	return text, done, timedOut
}

// openEnvState returns episode metadata (GET /openenv/state?episode=<id>).
func (g *Gateway) openEnvState(w http.ResponseWriter, r *http.Request) {
	reg := g.openEnv()
	reg.mu.Lock()
	ep := reg.episodes[r.URL.Query().Get("episode")]
	reg.mu.Unlock()
	if ep == nil {
		writeJSON(w, 404, map[string]string{"error": "unknown episode"})
		return
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"episode_id":  ep.id,
		"environment": ep.graph,
		"session":     ep.sessionID,
		"steps":       ep.steps,
		"done":        ep.done,
		"elapsed_ms":  time.Since(ep.started).Milliseconds(),
	})
}

// openEnvBundle returns the audit bundle for an episode
// (GET /openenv/bundle?episode=<id>).
//
// The reason this border exists rather than merely conforming: an environment
// that returns evidence alongside its observation lets a benchmark report the
// protocol assumptions behind a score instead of only the score.
func (g *Gateway) openEnvBundle(w http.ResponseWriter, r *http.Request) {
	reg := g.openEnv()
	reg.mu.Lock()
	ep := reg.episodes[r.URL.Query().Get("episode")]
	reg.mu.Unlock()
	if ep == nil {
		writeJSON(w, 404, map[string]string{"error": "unknown episode"})
		return
	}
	bundle, err := ledger.BuildBundle(g.St, ep.sessionID)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, bundle)
}
