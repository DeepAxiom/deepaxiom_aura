// Package api is the door a consumer uses: an asset in, an address out.
//
// Two exposures, decided deliberately. The control surface — uploading,
// listing, reading an asset — is behind a bearer token, like every route on the
// kernel. The output tree is not: an address a player cannot fetch is not an
// address, and these are public-zone bytes by construction. The whole reason
// this subsystem is a separate process is that nothing in it holds PHI.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"aura/media/internal/assets"
	"aura/media/internal/ingest"
	"aura/media/internal/queue"
	"aura/media/internal/store"
)

// Server serves the media API.
type Server struct {
	Assets  *assets.Repo
	Queue   *queue.Queue
	Store   store.Store
	Ingest  *ingest.Ingestor
	Log     *slog.Logger
	Version string

	// Token is the bearer credential the control surface requires. Empty means
	// the node was started with --no-auth, which is a deliberate act and is
	// said out loud at start-up.
	Token string
	// MediaPrefix is the URL path the output tree is served under.
	MediaPrefix string
	// Ready reports whether this node can actually do the job — the database
	// answers and ffmpeg exists. It is what /readyz calls.
	Ready func() error
}

// Handler builds the routes.
func (s *Server) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	prefix := s.MediaPrefix
	if prefix == "" {
		prefix = "/media"
	}
	prefix = "/" + strings.Trim(prefix, "/")

	mux := http.NewServeMux()

	// Open, and only these: liveness, readiness, and the bytes a player fetches.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET "+prefix+"/", s.serveObject(prefix))

	// Behind the token.
	mux.HandleFunc("POST /v1/assets", s.auth(s.createAsset))
	mux.HandleFunc("GET /v1/assets", s.auth(s.listAssets))
	mux.HandleFunc("GET /v1/assets/{id}", s.auth(s.getAsset))
	mux.HandleFunc("GET /v1/queue", s.auth(s.queueDepth))
	mux.HandleFunc("GET /v1/version", s.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"service": "aura-media", "version": s.Version})
	}))

	return logging(s.Log, mux)
}

// auth refuses anything without the token. The comparison is constant time:
// a token check that leaks its answer through timing is a token check that can
// be walked one byte at a time.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" {
			next(w, r)
			return
		}
		header := r.Header.Get("Authorization")
		presented, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(presented)), []byte(s.Token)) != 1 {
			writeProblem(w, http.StatusUnauthorized,
				"This node requires a token. Pass it as `Authorization: Bearer <token>`; the operator who started the node has it in the data directory as media.token.")
			return
		}
		next(w, r)
	}
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.Ready == nil {
		writeText(w, http.StatusOK, "ready")
		return
	}
	if err := s.Ready(); err != nil {
		// Readiness says what is wrong, because the reader is whoever has to
		// fix it: a load balancer ignores the body and an operator needs it.
		writeText(w, http.StatusServiceUnavailable, "not ready: "+err.Error())
		return
	}
	writeText(w, http.StatusOK, "ready")
}

// createAsset takes either raw bytes or a JSON body naming a URI to fetch.
func (s *Server) createAsset(w http.ResponseWriter, r *http.Request) {
	req := ingest.Request{
		Idem:     strings.TrimSpace(r.Header.Get("Idempotency-Key")),
		Priority: intParam(r, "priority"),
	}
	contentType := r.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)

	var (
		result ingest.Result
		err    error
	)
	if mediaType == "application/json" {
		var body struct {
			URI      string `json:"uri"`
			MIME     string `json:"mime"`
			Priority int    `json:"priority"`
			Idem     string `json:"idem"`
		}
		if derr := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); derr != nil {
			writeProblem(w, http.StatusBadRequest, "The JSON body could not be read: "+derr.Error())
			return
		}
		if strings.TrimSpace(body.URI) == "" {
			writeProblem(w, http.StatusBadRequest, "A JSON body must name a source with `uri`; to upload bytes, send them as the body with their own Content-Type.")
			return
		}
		req.MIME = body.MIME
		if body.Priority != 0 {
			req.Priority = body.Priority
		}
		if req.Idem == "" {
			req.Idem = strings.TrimSpace(body.Idem)
		}
		result, err = s.Ingest.FromURI(r.Context(), body.URI, req)
	} else {
		req.MIME = mediaType
		result, err = s.Ingest.FromReader(r.Context(), r.Body, req)
	}
	if err != nil {
		// The client went away mid-upload; nobody is reading this answer.
		if r.Context().Err() != nil {
			return
		}
		status := http.StatusInternalServerError
		if errors.Is(err, ingest.ErrTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		if status == http.StatusInternalServerError {
			s.Log.Error("ingest failed", "error", err)
		}
		writeProblem(w, status, err.Error())
		return
	}

	// 202, not 201: the asset exists, the address does not yet. Saying 201 here
	// would be telling a caller to fetch something that is still encoding.
	//
	// 200 when nothing new was queued — the same bytes, or an idempotency key
	// this node has already answered. The distinction matters: a client that
	// reuses one key for two different videos gets the first one's address back,
	// which is correct idempotency and a nasty surprise if the answer looks
	// identical to "your upload was accepted".
	status := http.StatusAccepted
	if result.Deduplicated {
		status = http.StatusOK
	}
	w.Header().Set("Location", "/v1/assets/"+result.Asset.ID)
	writeJSON(w, status, assetView{
		Asset:        result.Asset,
		JobID:        result.Job.ID,
		JobState:     string(result.Job.State),
		Deduplicated: result.Deduplicated,
	})
}

func (s *Server) getAsset(w http.ResponseWriter, r *http.Request) {
	asset, err := s.Assets.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, assets.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "No asset with that id on this node.")
		return
	}
	if err != nil {
		s.Log.Error("reading an asset", "error", err)
		writeProblem(w, http.StatusInternalServerError, err.Error())
		return
	}
	view := assetView{Asset: asset}
	if jobs, err := s.Queue.ForAsset(r.Context(), asset.ID); err == nil && len(jobs) > 0 {
		view.JobID = jobs[0].ID
		view.JobState = string(jobs[0].State)
		view.Attempts = jobs[0].Attempts
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) listAssets(w http.ResponseWriter, r *http.Request) {
	list, err := s.Assets.List(r.Context(), intParam(r, "limit"))
	if err != nil {
		s.Log.Error("listing assets", "error", err)
		writeProblem(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assets": list})
}

func (s *Server) queueDepth(w http.ResponseWriter, r *http.Request) {
	queued, running, err := s.Queue.Depth(r.Context())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"queued": queued, "running": running})
}

// PublicPrefix is the only part of the store this server will hand out without
// a token: packaged output.
//
// The store also holds every source under src/, and a source is the opposite of
// public — it is the clip before anything de-identified it. Serving the store's
// whole root would mean the original of every upload was fetchable by anyone who
// could guess an id, which is exactly the zone crossing this subsystem exists to
// avoid. So the boundary is a prefix check here, not a convention about which
// URLs anyone happens to publish.
const PublicPrefix = "out/"

// serveObject serves the packaged output. Read-only, output only, and every key
// goes through the store's own validation, so a request cannot walk out of the
// tree either.
func (s *Server) serveObject(prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, prefix+"/")
		clean, err := store.CleanKey(key)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		if !strings.HasPrefix(clean, PublicPrefix) {
			// Not 403: whether a source with this id exists is itself something
			// an anonymous caller has no business learning.
			writeProblem(w, http.StatusNotFound, "No such object.")
			return
		}
		body, size, err := s.Store.Open(r.Context(), clean)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "No such object.")
				return
			}
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		defer body.Close()
		w.Header().Set("Content-Type", contentType(clean))
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		// A segment never changes: it is written once under a key that contains
		// the asset id. A playlist for a finished VOD does not change either,
		// but it is the file a player re-reads, so it gets a shorter life.
		if strings.HasSuffix(clean, ".m3u8") {
			w.Header().Set("Cache-Control", "public, max-age=60")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		if _, err := io.Copy(w, body); err != nil {
			s.Log.Debug("client went away mid-object", "key", clean, "error", err)
		}
	}
}

// assetView is what the API returns: the asset, plus the state of the work
// behind it. A consumer polls this and stops when `state` is ready or failed.
type assetView struct {
	assets.Asset
	JobID        int64  `json:"job_id,omitempty"`
	JobState     string `json:"job_state,omitempty"`
	Attempts     int    `json:"attempts,omitempty"`
	Deduplicated bool   `json:"deduplicated,omitempty"`
}

func contentType(key string) string {
	switch strings.ToLower(path.Ext(key)) {
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	case ".m4s", ".mp4":
		return "video/mp4"
	case ".ts":
		return "video/mp2t"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".vtt":
		return "text/vtt"
	default:
		return "application/octet-stream"
	}
}

func intParam(r *http.Request, name string) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return 0
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, body)
}

// writeProblem answers a failure in the words of whoever has to act on it: what
// was refused and what would fix it, never a code on its own.
func writeProblem(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"error": detail})
}

func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// The output tree is fetched a few hundred times per playback; logging
		// every segment at info would bury everything else.
		level := slog.LevelInfo
		if strings.HasPrefix(r.URL.Path, "/media/") || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			level = slog.LevelDebug
		}
		log.Log(r.Context(), level, "request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"took", time.Since(start).Round(time.Millisecond))
	})
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
