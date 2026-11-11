package api

import (
	"errors"
	"net/http"

	"aura/media/internal/live"
)

// The live routes: a stream in, an address out.
//
//	POST /v1/live            create a stream; the answer carries its key, once
//	GET  /v1/live            list streams
//	GET  /v1/live/{id}       read one
//	POST /v1/live/{id}/end   stop it: revoke the key, or cut off the broadcast
//
// All four are behind the token, like every other control route. What a
// viewer needs — the address — is in the open output tree, like every other
// address.

// liveOff answers every live route on a node that was not started to take
// streams. 503 rather than 404: the route exists in this build, the operator
// simply has not turned it on, and the body says how.
func (s *Server) liveOff(w http.ResponseWriter) bool {
	if s.Live != nil {
		return false
	}
	writeProblem(w, http.StatusServiceUnavailable,
		"Live is not enabled on this node. Start it with --rtmp-bind (for example --rtmp-bind 0.0.0.0:1935) so there is somewhere for a broadcaster to connect.")
	return true
}

func (s *Server) createLive(w http.ResponseWriter, r *http.Request) {
	if s.liveOff(w) {
		return
	}
	view, err := s.Live.Create(r.Context())
	if err != nil {
		s.Log.Error("creating a live stream", "error", err)
		writeProblem(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Location", "/v1/live/"+view.ID)
	// The key is in this body and will never be in another. It must not be
	// kept by anything between here and the caller.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) getLive(w http.ResponseWriter, r *http.Request) {
	if s.liveOff(w) {
		return
	}
	view, err := s.Live.Get(r.Context(), r.PathValue("id"))
	s.writeLive(w, view, err)
}

func (s *Server) endLive(w http.ResponseWriter, r *http.Request) {
	if s.liveOff(w) {
		return
	}
	view, err := s.Live.End(r.Context(), r.PathValue("id"))
	s.writeLive(w, view, err)
}

func (s *Server) writeLive(w http.ResponseWriter, view live.View, err error) {
	if errors.Is(err, live.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "No live stream with that id on this node.")
		return
	}
	if err != nil {
		s.Log.Error("reading a live stream", "error", err)
		writeProblem(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) listLive(w http.ResponseWriter, r *http.Request) {
	if s.liveOff(w) {
		return
	}
	list, err := s.Live.List(r.Context(), intParam(r, "limit"))
	if err != nil {
		s.Log.Error("listing live streams", "error", err)
		writeProblem(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"streams": list})
}
