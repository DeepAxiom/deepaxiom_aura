package demo

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
)

// service is the thing the effects act on: an HTTP API that carries out an
// order only when it comes with the right key.
//
// It stands for whatever a deployment protects — an ERP, a payment rail, a
// controller. It is deliberately dumb about AURA: it knows nothing of receipts
// or policy, only of a bearer key, because that is all a real service knows.
// Whether the wrong party can get that key is the node's problem, and it is the
// thing the demo attacks.
type service struct {
	key string
	ln  net.Listener
	srv *http.Server

	executed atomic.Int64 // orders carried out
	rejected atomic.Int64 // calls turned away for lacking the key

	mu   sync.Mutex
	done map[string]bool // order ids carried out
}

func newKey() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newService() (*service, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &service{key: newKey(), ln: ln, done: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", s.order)
	s.srv = &http.Server{Handler: mux}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

func (s *service) url() string { return "http://" + s.ln.Addr().String() }
func (s *service) close()      { _ = s.srv.Close() }

func (s *service) order(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.key {
		s.rejected.Add(1)
		http.Error(w, `{"error":"unauthorised"}`, http.StatusUnauthorized)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	s.mu.Lock()
	s.done[body.ID] = true
	s.mu.Unlock()
	s.executed.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true}`)
}

// carriedOut reports whether the service executed an order with this id.
func (s *service) carriedOut(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done[id]
}
