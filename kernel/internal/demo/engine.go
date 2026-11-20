package demo

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// simEngine is an inference server that produces tokens on a timer.
//
// It speaks enough of the OpenAI API for the node to treat it as an engine:
// streaming chat completions, usage on request, a model list. It is not a
// model and nothing here measures one. What it gives the demo is the one thing
// a real engine cannot: it knows the instant a caller hung up on it, so "the
// engine was released N ms after the cancel" is observed at the engine rather
// than inferred from the node saying so.
type simEngine struct {
	key        string
	firstToken time.Duration
	tokenEvery time.Duration

	ln  net.Listener
	srv *http.Server

	requests atomic.Int64
	active   atomic.Int64
	tokens   atomic.Int64
	refused  atomic.Int64 // requests that did not carry the engine's key

	mu       sync.Mutex
	released map[string]release // by prompt
}

// release is a generation that ended because its caller went away.
type release struct {
	at       time.Time
	produced int
	planned  int
}

func newSimEngine(key string) (*simEngine, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	e := &simEngine{
		key: key, firstToken: 18 * time.Millisecond, tokenEvery: 4 * time.Millisecond,
		ln: ln, released: map[string]release{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", e.models)
	mux.HandleFunc("POST /v1/chat/completions", e.chat)
	e.srv = &http.Server{Handler: mux}
	go func() { _ = e.srv.Serve(ln) }()
	return e, nil
}

func (e *simEngine) url() string { return "http://" + e.ln.Addr().String() + "/v1" }
func (e *simEngine) close()      { _ = e.srv.Close() }

func (e *simEngine) authorised(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") == "Bearer "+e.key {
		return true
	}
	e.refused.Add(1)
	http.Error(w, `{"error":"this engine was not given its key"}`, http.StatusUnauthorized)
	return false
}

func (e *simEngine) models(w http.ResponseWriter, r *http.Request) {
	if !e.authorised(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"simulated","object":"model"}]}`)
}

type chatRequest struct {
	MaxTokens     int  `json:"max_tokens"`
	Stream        bool `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

func (e *simEngine) chat(w http.ResponseWriter, r *http.Request) {
	if !e.authorised(w, r) {
		return
	}
	var req chatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	planned := req.MaxTokens
	if planned <= 0 {
		planned = 16
	}
	prompt := ""
	if n := len(req.Messages); n > 0 {
		prompt = req.Messages[n-1].Content
	}
	e.requests.Add(1)
	e.active.Add(1)
	defer e.active.Add(-1)

	flusher, _ := w.(http.Flusher)
	gone := r.Context().Done()
	produced := 0
	cut := func() {
		e.mu.Lock()
		e.released[prompt] = release{at: time.Now(), produced: produced, planned: planned}
		e.mu.Unlock()
	}
	wait := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-gone:
			return false
		case <-t.C:
			return true
		}
	}

	if !wait(e.firstToken) {
		cut()
		return
	}
	usage := fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%%d,"prompt_tokens_details":{"cached_tokens":24}}`,
		32+len(prompt)/4)

	if !req.Stream {
		if !wait(time.Duration(planned) * e.tokenEvery) {
			cut()
			return
		}
		e.tokens.Add(int64(planned))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":%s}`,
			strings.Repeat("tok ", planned), fmt.Sprintf(usage, planned))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	send := func(data string) bool {
		if _, err := io.WriteString(w, "data: "+data+"\n\n"); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	if !send(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`) {
		cut()
		return
	}
	for produced < planned {
		if !send(`{"choices":[{"index":0,"delta":{"content":"tok "}}]}`) {
			cut()
			return
		}
		produced++
		e.tokens.Add(1)
		if produced < planned && !wait(e.tokenEvery) {
			cut()
			return
		}
	}
	send(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	if req.StreamOptions.IncludeUsage {
		send(`{"choices":[],"usage":` + fmt.Sprintf(usage, produced) + `}`)
	}
	send("[DONE]")
}

// waitReleased waits until every prompt has been cut, or limit passes, and
// returns what it has.
func (e *simEngine) waitReleased(prompts []string, limit time.Duration) map[string]release {
	end := time.Now().Add(limit)
	for {
		out := map[string]release{}
		e.mu.Lock()
		for _, p := range prompts {
			if r, ok := e.released[p]; ok {
				out[p] = r
			}
		}
		e.mu.Unlock()
		if len(out) == len(prompts) || time.Now().After(end) {
			return out
		}
		time.Sleep(time.Millisecond)
	}
}
