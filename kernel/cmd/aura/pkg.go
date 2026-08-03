package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"aura/kernel/internal/hub"
	"aura/kernel/internal/signing"
)

func defaultRegistry() string {
	if v := os.Getenv("AURA_REGISTRY"); v != "" {
		return v
	}
	return "http://localhost:9091"
}

// cmdRegistry — run a federable package registry.
//
//	aura registry serve [--port 9091] [--data <dir>]
func cmdRegistry(args []string) {
	if len(args) < 1 || args[0] != "serve" {
		fatal(fmt.Errorf("usage: aura registry serve [--port 9091] [--data <dir>]"))
	}
	fs := flag.NewFlagSet("registry serve", flag.ExitOnError)
	port := fs.Int("port", 9091, "registry port")
	data := fs.String("data", filepath.Join(defaultDataDir(), "registry"), "data directory")
	_ = fs.Parse(args[1:])

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	h, err := hub.Open(*data, log)
	if err != nil {
		fatal(err)
	}
	defer h.Close()

	fmt.Printf(`
  aura registry — up
  api       http://localhost:%d/r1
  data      %s
  publish   aura publish <skill-dir> --registry http://localhost:%d

`, *port, *data, *port)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", *port), h.Handler()); err != nil {
		fatal(err)
	}
}

// cmdPublish — sign and upload a skill package (published mode).
//
//	aura publish <skill-dir> [--registry <url>]
func cmdPublish(args []string) {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	registryURL := fs.String("registry", defaultRegistry(), "registry URL")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		// allow flags after the positional
		if len(rest) > 1 {
			_ = fs.Parse(rest[1:])
		}
	}
	if len(rest) < 1 {
		fatal(fmt.Errorf("usage: aura publish <skill-dir> [--registry <url>]"))
	}
	dir := rest[0]

	manifestRaw, err := os.ReadFile(filepath.Join(dir, "skill.yaml"))
	if err != nil {
		fatal(fmt.Errorf("no skill.yaml in %s: %w", dir, err))
	}
	var manifest map[string]any
	if err := yaml.Unmarshal(manifestRaw, &manifest); err != nil {
		fatal(fmt.Errorf("invalid skill.yaml: %w", err))
	}

	artifact, files, err := zipSkillDir(dir)
	if err != nil {
		fatal(err)
	}

	keys, err := signing.LoadOrCreate(filepath.Join(defaultDataDir(), "keys"))
	if err != nil {
		fatal(err)
	}
	manifestHash, err := signing.CanonicalManifestHash(manifest)
	if err != nil {
		fatal(err)
	}
	artifactHash := signing.ArtifactHash(artifact)
	sig := keys.Sign(signing.Payload(manifestHash, artifactHash))

	body, _ := json.Marshal(map[string]any{
		"manifest": manifest, "pubkey": keys.PublicB64(), "signature": sig,
		"artifact_b64": base64.StdEncoding.EncodeToString(artifact),
	})
	resp, err := http.Post(*registryURL+"/r1/packages", "application/json", bytes.NewReader(body))
	if err != nil {
		fatal(fmt.Errorf("registry not reachable at %s (%w)", *registryURL, err))
	}
	defer resp.Body.Close()
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		fatal(fmt.Errorf("publish rejected (%d): %s", resp.StatusCode, out["error"]))
	}
	fmt.Printf("published %s@%s (%d files, %.1f KiB)\n",
		out["id"], out["version"], files, float64(len(artifact))/1024)
	fmt.Printf("  signed with key %s…\n", keys.PublicB64()[:16])
	fmt.Printf("  install:  aura add %s --registry %s\n", out["id"], *registryURL)
}

// cmdAdd — download, verify (hash + signature), review permissions, install.
//
//	aura add <org/cat/name>[@version] [--registry <url>] [--yes]
//	aura add --capability sensorial.ocr [--registry <url>] [--yes]
func cmdAdd(args []string) {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	registryURL := fs.String("registry", defaultRegistry(), "registry URL")
	capability := fs.String("capability", "", "resolve by capability instead of name")
	yes := fs.Bool("yes", false, "skip the permissions prompt")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) > 1 {
		_ = fs.Parse(rest[1:])
		rest = rest[:1]
	}

	var pkgURL string
	switch {
	case *capability != "":
		id := discoverByCapability(*registryURL, *capability)
		fmt.Printf("  resolved capability %q → %s\n", *capability, id)
		pkgURL = fmt.Sprintf("%s/r1/packages/%s/latest", *registryURL, id)
	case len(rest) == 1:
		id, version, _ := strings.Cut(rest[0], "@")
		if version == "" {
			version = "latest"
		}
		pkgURL = fmt.Sprintf("%s/r1/packages/%s/%s", *registryURL, id, version)
	default:
		fatal(fmt.Errorf("usage: aura add <org/cat/name>[@version] | aura add --capability <cap>"))
	}

	var pkg struct {
		ID           string         `json:"id"`
		Version      string         `json:"version"`
		Capability   string         `json:"capability"`
		Manifest     map[string]any `json:"manifest"`
		ArtifactHash string         `json:"artifact_hash"`
		Signature    string         `json:"signature"`
		Pubkey       string         `json:"pubkey"`
	}
	fetchJSON(pkgURL, &pkg)

	// Download artifact and verify hash + signature BEFORE anything else.
	resp, err := http.Get(pkgURL + "/artifact")
	if err != nil {
		fatal(err)
	}
	defer resp.Body.Close()
	artifact, err := io.ReadAll(io.LimitReader(resp.Body, 96<<20))
	if err != nil || resp.StatusCode != 200 {
		fatal(fmt.Errorf("artifact download failed (%d)", resp.StatusCode))
	}
	if hex.EncodeToString(signing.ArtifactHash(artifact)) != pkg.ArtifactHash {
		fatal(fmt.Errorf("artifact hash MISMATCH — refusing to install"))
	}
	manifestHash, err := signing.CanonicalManifestHash(pkg.Manifest)
	if err != nil {
		fatal(err)
	}
	if err := signing.Verify(pkg.Pubkey, pkg.Signature,
		signing.Payload(manifestHash, signing.ArtifactHash(artifact))); err != nil {
		fatal(fmt.Errorf("%w — refusing to install", err))
	}

	// Permissions review (capability-based security: show before install).
	fmt.Printf("\n  %s@%s  (%s)\n", pkg.ID, pkg.Version, pkg.Capability)
	if desc, ok := pkg.Manifest["description"].(string); ok {
		fmt.Printf("  %s\n", desc)
	}
	fmt.Printf("  publisher key %s…  · signature OK · hash OK\n", pkg.Pubkey[:16])
	fmt.Println("  permissions:")
	perms, _ := pkg.Manifest["permissions"].(map[string]any)
	if len(perms) == 0 {
		fmt.Println("    (none declared — deny-by-default applies)")
	}
	for k, v := range perms {
		fmt.Printf("    %-12s %v\n", k, v)
	}
	if !*yes {
		fmt.Print("\n  install? [y/N]: ")
		sc := bufio.NewScanner(os.Stdin)
		if !sc.Scan() || !strings.EqualFold(strings.TrimSpace(sc.Text()), "y") {
			fmt.Println("  aborted")
			return
		}
	}

	dest := filepath.Join(defaultDataDir(), "skills",
		strings.ReplaceAll(pkg.ID, "/", "-"), pkg.Version)
	if err := unzipTo(artifact, dest); err != nil {
		fatal(err)
	}
	fmt.Printf("\ninstalled to %s\n", dest)
	fmt.Printf("  run it:  aura run %s\n", pkg.ID)
}

// cmdRun — start an installed skill against the local kernel. A `source`
// skill is spawned as its own process, dialing back over WS; a `wasm`
// skill is hosted inside the running kernel itself — see runWasmSkill.
//
//	aura run <org/cat/name> [--port 9080]
func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) > 1 {
		_ = fs.Parse(rest[1:])
		rest = rest[:1]
	}
	if len(rest) != 1 {
		fatal(fmt.Errorf("usage: aura run <org/cat/name> [--port 9080]"))
	}
	base := filepath.Join(defaultDataDir(), "skills", strings.ReplaceAll(rest[0], "/", "-"))
	versions, err := os.ReadDir(base)
	if err != nil || len(versions) == 0 {
		fatal(fmt.Errorf("skill not installed: %s (aura add %s)", rest[0], rest[0]))
	}
	names := make([]string, 0, len(versions))
	for _, v := range versions {
		names = append(names, v.Name())
	}
	sort.Strings(names)
	dir := filepath.Join(base, names[len(names)-1])

	manifestRaw, err := os.ReadFile(filepath.Join(dir, "skill.yaml"))
	if err != nil {
		fatal(fmt.Errorf("no skill.yaml in %s: %w", dir, err))
	}
	var manifest map[string]any
	if err := yaml.Unmarshal(manifestRaw, &manifest); err != nil {
		fatal(fmt.Errorf("invalid skill.yaml: %w", err))
	}

	// A wasm skill is never a separate OS process — it is hosted inside the
	// running kernel (see kernel/internal/gateway/wasm.go), so `aura run`
	// asks the kernel to host it instead of spawning anything.
	if manifest["format"] == "wasm" {
		runWasmSkill(*port, rest[0], dir, manifest)
		return
	}

	cmd := exec.Command("python", "main.py")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("AURA_WS_URL=ws://localhost:%d/ws/skill", *port),
		fmt.Sprintf("AURA_HTTP_URL=http://localhost:%d", *port))
	fmt.Printf("running %s (%s) against :%d — Ctrl+C to stop\n", rest[0], dir, *port)
	if err := cmd.Run(); err != nil {
		fatal(fmt.Errorf("skill exited: %w (is python + aura-sdk installed?)", err))
	}
}

// runWasmSkill POSTs an installed format:wasm package's manifest + compiled
// module to the running kernel's POST /v1/skills/wasm, which compiles and
// registers it in-process (kernel/internal/gateway/wasm.go). The packaging
// convention is the same rigidity skill.yaml already has: a wasm package's
// compiled binary is a file literally named skill.wasm at the package root.
func runWasmSkill(port int, id, dir string, manifest map[string]any) {
	wasmBytes, err := os.ReadFile(filepath.Join(dir, "skill.wasm"))
	if err != nil {
		fatal(fmt.Errorf("no skill.wasm in %s (a format:wasm package must include one): %w", dir, err))
	}
	body, _ := json.Marshal(map[string]any{
		"manifest": manifest,
		"wasm_b64": base64.StdEncoding.EncodeToString(wasmBytes),
	})
	code, resp := postJSON(port, "/v1/skills/wasm", body)
	if code != 201 {
		fatal(fmt.Errorf("the kernel refused to host %s (%d): %s", id, code, resp["error"]))
	}
	fmt.Printf("hosted %s inside the kernel at :%d (%s)\n", id, port, dir)
}

// ── helpers ──────────────────────────────────────────────────────

func discoverByCapability(registryURL, capability string) string {
	var out struct {
		Packages []struct {
			ID string `json:"id"`
		} `json:"packages"`
	}
	fetchJSON(registryURL+"/r1/packages?capability="+capability, &out)
	if len(out.Packages) == 0 {
		fatal(fmt.Errorf("no package in the registry provides capability %q", capability))
	}
	return out.Packages[0].ID
}

func fetchJSON(url string, v any) {
	resp, err := http.Get(url)
	if err != nil {
		fatal(fmt.Errorf("registry not reachable (%w)", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&e)
		fatal(fmt.Errorf("registry error (%d): %s", resp.StatusCode, e["error"]))
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		fatal(err)
	}
}

var zipSkip = map[string]bool{
	"__pycache__": true, ".venv": true, "venv": true, "node_modules": true,
	".git": true, "dist": true,
}

func zipSkillDir(dir string) ([]byte, int, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := 0
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if zipSkip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".pyc") {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(w, f); err != nil {
			return err
		}
		files++
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if err := zw.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), files, nil
}

func unzipTo(artifact []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(artifact), int64(len(artifact)))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for _, f := range zr.File {
		// Zip-slip guard: reject any path escaping the destination.
		target := filepath.Join(dest, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("artifact contains an unsafe path: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		src, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			src.Close()
			return err
		}
		_, err = io.Copy(out, io.LimitReader(src, maxExtractFile))
		src.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

const maxExtractFile = 256 << 20
