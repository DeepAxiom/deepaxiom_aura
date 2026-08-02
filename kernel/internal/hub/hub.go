// Package hub implements the federable package registry.
//
// It is deliberately NOT part of the kernel: it runs as its own server
// (`aura registry serve`), anyone can host one — like container registries —
// which is what makes the neutrality promise credible (R13).
//
// Trust model (published mode):
//   - Every package is signed (Ed25519) over manifest hash + artifact hash.
//   - Trust-on-first-use per package id: the first publish binds the key;
//     later versions must be signed by the same key (409 otherwise).
//   - Versions are immutable: republishing the same version with different
//     content is rejected (idempotent republish is allowed).
package hub

import (
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"aura/kernel/internal/registry"
	"aura/kernel/internal/signing"
)

const maxArtifactSize = 64 << 20 // 64 MiB

type Hub struct {
	db    *sql.DB
	blobs string
	log   *slog.Logger
}

func Open(dataDir string, log *slog.Logger) (*Hub, error) {
	blobs := filepath.Join(dataDir, "blobs")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		return nil, err
	}
	dsn := filepath.Join(dataDir, "registry.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS packages (
  id            TEXT NOT NULL,
  version       TEXT NOT NULL,
  capability    TEXT NOT NULL,
  manifest      TEXT NOT NULL,
  artifact_hash TEXT NOT NULL,
  signature     TEXT NOT NULL,
  pubkey        TEXT NOT NULL,
  published     INTEGER NOT NULL,
  PRIMARY KEY (id, version)
);
CREATE INDEX IF NOT EXISTS idx_packages_capability ON packages(capability);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Hub{db: db, blobs: blobs, log: log}, nil
}

func (h *Hub) Close() error { return h.db.Close() }

// ── HTTP API (/r1/*) ─────────────────────────────────────────────

func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /r1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "registry": "aura", "api": "r1"})
	})
	mux.HandleFunc("POST /r1/packages", h.publish)
	mux.HandleFunc("GET /r1/packages", h.list)
	mux.HandleFunc("GET /r1/packages/{org}/{cat}/{name}", h.versions)
	mux.HandleFunc("GET /r1/packages/{org}/{cat}/{name}/{version}", h.get)
	mux.HandleFunc("GET /r1/packages/{org}/{cat}/{name}/{version}/artifact", h.artifact)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type publishRequest struct {
	Manifest    map[string]any `json:"manifest"`
	Pubkey      string         `json:"pubkey"`
	Signature   string         `json:"signature"`
	ArtifactB64 string         `json:"artifact_b64"`
}

func (h *Hub) publish(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxArtifactSize+(8<<20))
	var req publishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}

	// 1. Manifest must be valid C1.
	rawManifest, _ := json.Marshal(req.Manifest)
	var manifest registry.Manifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		writeJSON(w, 422, map[string]string{"error": "manifest: " + err.Error()})
		return
	}
	if err := manifest.Validate(); err != nil {
		writeJSON(w, 422, map[string]string{"error": "manifest rejected: " + err.Error()})
		return
	}

	// 2. Artifact + signature (published mode is mandatory here).
	artifact, err := base64.StdEncoding.DecodeString(req.ArtifactB64)
	if err != nil || len(artifact) == 0 {
		writeJSON(w, 422, map[string]string{"error": "missing or invalid artifact"})
		return
	}
	if len(artifact) > maxArtifactSize {
		writeJSON(w, 413, map[string]string{"error": "artifact exceeds 64 MiB"})
		return
	}
	if req.Pubkey == "" || req.Signature == "" {
		writeJSON(w, 422, map[string]string{"error": "publishing requires pubkey and signature (published mode)"})
		return
	}
	manifestHash, err := signing.CanonicalManifestHash(req.Manifest)
	if err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	artifactHash := signing.ArtifactHash(artifact)
	if err := signing.Verify(req.Pubkey, req.Signature,
		signing.Payload(manifestHash, artifactHash)); err != nil {
		writeJSON(w, 403, map[string]string{"error": err.Error()})
		return
	}

	// 3. TOFU: the first publish of an id binds the key.
	var boundKey string
	err = h.db.QueryRow(`SELECT pubkey FROM packages WHERE id=? LIMIT 1`, manifest.ID).Scan(&boundKey)
	if err == nil && boundKey != req.Pubkey {
		writeJSON(w, 409, map[string]string{
			"error": "package id is bound to a different publisher key (trust-on-first-use)"})
		return
	}

	// 4. Version immutability (idempotent republish allowed).
	hashHex := hex.EncodeToString(artifactHash)
	var existingHash string
	err = h.db.QueryRow(`SELECT artifact_hash FROM packages WHERE id=? AND version=?`,
		manifest.ID, manifest.Version).Scan(&existingHash)
	if err == nil {
		if existingHash == hashHex {
			writeJSON(w, 200, map[string]string{"status": "already published",
				"id": manifest.ID, "version": manifest.Version})
			return
		}
		writeJSON(w, 409, map[string]string{
			"error": "version already exists with different content — versions are immutable, bump the version"})
		return
	}

	// 5. Persist blob + metadata.
	if err := os.WriteFile(filepath.Join(h.blobs, hashHex+".zip"), artifact, 0o644); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	_, err = h.db.Exec(`
INSERT INTO packages (id, version, capability, manifest, artifact_hash, signature, pubkey, published)
VALUES (?,?,?,?,?,?,?,?)`,
		manifest.ID, manifest.Version, manifest.Capability, string(rawManifest),
		hashHex, req.Signature, req.Pubkey, time.Now().UnixMilli())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	h.log.Info("package published", "id", manifest.ID, "version", manifest.Version,
		"capability", manifest.Capability)
	writeJSON(w, 201, map[string]string{"id": manifest.ID, "version": manifest.Version,
		"artifact_hash": hashHex})
}

type packageInfo struct {
	ID           string          `json:"id"`
	Version      string          `json:"version"`
	Capability   string          `json:"capability"`
	Manifest     json.RawMessage `json:"manifest"`
	ArtifactHash string          `json:"artifact_hash"`
	Signature    string          `json:"signature"`
	Pubkey       string          `json:"pubkey"`
}

func (h *Hub) scanRows(rows *sql.Rows) ([]packageInfo, error) {
	defer rows.Close()
	var out []packageInfo
	for rows.Next() {
		var p packageInfo
		var manifest string
		if err := rows.Scan(&p.ID, &p.Version, &p.Capability, &manifest,
			&p.ArtifactHash, &p.Signature, &p.Pubkey); err != nil {
			return nil, err
		}
		p.Manifest = json.RawMessage(manifest)
		out = append(out, p)
	}
	return out, rows.Err()
}

const selectCols = `SELECT id, version, capability, manifest, artifact_hash, signature, pubkey FROM packages`

// list returns the latest version of every package; ?capability= filters by
// exact match or prefix (auto-discovery, R5).
func (h *Hub) list(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(selectCols + ` ORDER BY id`)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	all, err := h.scanRows(rows)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	capFilter := r.URL.Query().Get("capability")
	latest := map[string]packageInfo{}
	for _, p := range all {
		if capFilter != "" && p.Capability != capFilter &&
			!strings.HasPrefix(p.Capability, capFilter+".") {
			continue
		}
		if cur, ok := latest[p.ID]; !ok || semverLess(cur.Version, p.Version) {
			latest[p.ID] = p
		}
	}
	out := make([]packageInfo, 0, len(latest))
	for _, p := range latest {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, 200, map[string]any{"packages": out})
}

func pkgID(r *http.Request) string {
	return r.PathValue("org") + "/" + r.PathValue("cat") + "/" + r.PathValue("name")
}

func (h *Hub) versions(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(selectCols+` WHERE id=? ORDER BY published`, pkgID(r))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	list, err := h.scanRows(rows)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if len(list) == 0 {
		writeJSON(w, 404, map[string]string{"error": "package not found: " + pkgID(r)})
		return
	}
	writeJSON(w, 200, map[string]any{"versions": list})
}

func (h *Hub) resolveVersion(r *http.Request) (packageInfo, error) {
	id, version := pkgID(r), r.PathValue("version")
	var rows *sql.Rows
	var err error
	if version == "latest" {
		rows, err = h.db.Query(selectCols+` WHERE id=?`, id)
	} else {
		rows, err = h.db.Query(selectCols+` WHERE id=? AND version=?`, id, version)
	}
	if err != nil {
		return packageInfo{}, err
	}
	list, err := h.scanRows(rows)
	if err != nil {
		return packageInfo{}, err
	}
	if len(list) == 0 {
		return packageInfo{}, fmt.Errorf("not found: %s@%s", id, version)
	}
	best := list[0]
	for _, p := range list[1:] {
		if semverLess(best.Version, p.Version) {
			best = p
		}
	}
	return best, nil
}

func (h *Hub) get(w http.ResponseWriter, r *http.Request) {
	p, err := h.resolveVersion(r)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, p)
}

func (h *Hub) artifact(w http.ResponseWriter, r *http.Request) {
	p, err := h.resolveVersion(r)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	http.ServeFile(w, r, filepath.Join(h.blobs, p.ArtifactHash+".zip"))
}

// semverLess reports a < b for x.y.z versions (lexicographic fallback).
func semverLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3 && i < len(pa) && i < len(pb); i++ {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		if ea != nil || eb != nil {
			return a < b
		}
		if na != nb {
			return na < nb
		}
	}
	return len(pa) < len(pb)
}
