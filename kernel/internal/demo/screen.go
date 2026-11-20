package demo

import (
	_ "embed"
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

// The screen is one HTML file with nothing to fetch but the state. It is
// embedded so `aura demo` works from the binary alone, and so what a viewer
// sees cannot drift from the code that measured it.
//
//go:embed screen.html
var screenHTML string

// Screen serves the page and the run's state.
type Screen struct {
	run *Run
	ln  net.Listener
	srv *http.Server
}

// Serve starts the screen for a run. lang fixes the language; empty leaves it
// to the browser.
func Serve(run *Run, addr, lang string) (*Screen, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Screen{run: run, ln: ln}
	page := strings.Replace(screenHTML, `data-lang=""`, `data-lang="`+safeLang(lang)+`"`, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(page))
	})
	// The page asks for the state several times a second. The first time it
	// does, somebody is watching, and a held run may begin.
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watching") == "1" {
			run.Begin()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(run.Snapshot())
	})
	s.srv = &http.Server{Handler: mux}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

func safeLang(lang string) string {
	switch strings.ToLower(lang) {
	case "es", "en":
		return strings.ToLower(lang)
	}
	return ""
}

// Addr is where the screen is being served.
func (s *Screen) Addr() string { return s.ln.Addr().String() }

// Close stops serving.
func (s *Screen) Close() { _ = s.srv.Close() }
