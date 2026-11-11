// Package config resolves how this node runs, and refuses combinations that
// would be unsafe.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"aura/media/internal/pipeline"
)

// Config is one node's settings.
type Config struct {
	DSN        string
	Bind       string
	DataDir    string
	BaseURL    string
	Workers    int
	Lease      time.Duration
	MaxBytes   int64
	FFmpeg     string
	FFprobe    string
	NoAuth     bool
	FetchHTTP  bool
	FrameEvery float64
	MaxFrames  int
	NodeWS     string
	NodeToken  string

	// The encoder. Every one of these left unset is the encode 0.1.0 did.
	VideoEncoder string
	Preset       string
	RateMode     string
	CRF          int
	// LegacyEncode runs every job the way 0.1.0 did and nothing else.
	LegacyEncode bool

	// RTMPBind is where broadcasters connect. Empty means this node does not
	// take live streams at all.
	RTMPBind string
	// RTMPURL is what broadcasters are told to push to, when it is not simply
	// the bind address: a hostname, or whatever terminates TLS in front.
	RTMPURL    string
	LiveGrace  time.Duration
	LiveWindow time.Duration
}

// Encoding is the encoder choice in the form the pipeline takes.
func (c Config) Encoding() pipeline.Encoding {
	return pipeline.Encoding{
		VideoEncoder: strings.TrimSpace(c.VideoEncoder),
		Preset:       strings.TrimSpace(c.Preset),
		RateMode:     pipeline.RateMode(strings.TrimSpace(c.RateMode)),
		CRF:          c.CRF,
	}
}

// LiveRoot is scratch space for live encodes, apart from the job workers'.
func (c Config) LiveRoot() string { return filepath.Join(c.DataDir, "live") }

// IngestURL is the address a broadcaster is given, without the stream key.
//
// It is derived when it is not configured, and the derivation is only right
// for a node being tried out on one machine: a bind on every interface says
// nothing about what name the outside world knows this machine by, so it
// falls back to loopback rather than guess. Anything real sets --rtmp-url.
func (c Config) IngestURL() string {
	if u := strings.TrimRight(strings.TrimSpace(c.RTMPURL), "/"); u != "" {
		return u
	}
	host, port, err := net.SplitHostPort(c.RTMPBind)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	if port == "1935" {
		return "rtmp://" + host + "/live"
	}
	return "rtmp://" + net.JoinHostPort(host, port) + "/live"
}

// ObjectRoot is where the store keeps its bytes.
func (c Config) ObjectRoot() string { return filepath.Join(c.DataDir, "objects") }

// WorkRoot is scratch space for encodes: local disk, wiped per job.
func (c Config) WorkRoot() string { return filepath.Join(c.DataDir, "work") }

// TokenPath is where the control-surface credential lives.
func (c Config) TokenPath() string { return filepath.Join(c.DataDir, "media.token") }

// Validate refuses the combinations that would be a hole rather than a choice.
func (c Config) Validate() error {
	if c.DSN == "" {
		return errors.New("no database: pass --dsn or set AURA_MEDIA_DSN (this subsystem's queue is Postgres)")
	}
	if c.DataDir == "" {
		return errors.New("no data directory: pass --data")
	}
	host, _, err := net.SplitHostPort(c.Bind)
	if err != nil {
		return fmt.Errorf("--bind %q is not host:port", c.Bind)
	}
	// A node reachable from the network with its control surface open would let
	// anyone on that network upload work onto a machine that encodes video.
	// Refusing at start-up is the only place this is cheap to notice.
	if c.NoAuth && !isLoopback(host) {
		return fmt.Errorf("--no-auth is only allowed on a loopback bind; %q is reachable from the network", c.Bind)
	}
	if err := c.Encoding().Validate(); err != nil {
		return err
	}
	if c.RTMPBind != "" {
		if _, _, err := net.SplitHostPort(c.RTMPBind); err != nil {
			return fmt.Errorf("--rtmp-bind %q is not host:port", c.RTMPBind)
		}
		if u := strings.TrimSpace(c.RTMPURL); u != "" && !strings.HasPrefix(u, "rtmp://") && !strings.HasPrefix(u, "rtmps://") {
			return fmt.Errorf("--rtmp-url %q is not an rtmp:// or rtmps:// URL", c.RTMPURL)
		}
	}
	if c.LiveGrace < 0 || c.LiveWindow < 0 {
		return errors.New("--live-grace and --live-window cannot be negative")
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "" {
		return false // an empty host binds every interface
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ResolveToken returns the control-surface credential, minting one on first
// start. Same shape as the kernel: a node mints its own and leaves it in the
// data directory, so a local operator needs to configure nothing and a remote
// caller cannot guess it.
//
// The environment wins, for containers and CI where the data directory may not
// be readable by whoever needs the token.
func ResolveToken(c Config) (string, error) {
	if c.NoAuth {
		return "", nil
	}
	if env := strings.TrimSpace(os.Getenv("AURA_MEDIA_TOKEN")); env != "" {
		return env, nil
	}
	path := c.TokenPath()
	existing, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(existing))
		if token == "" {
			return "", fmt.Errorf("%s is empty; delete it and start again to mint a new token", path)
		}
		if err := checkPermissions(path); err != nil {
			return "", err
		}
		return token, nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return token, nil
}

// ResolveNodeToken is the credential this service presents to the KERNEL, which
// is a different credential from the one it demands of its own callers.
//
// Same two places the Python SDK and the CLI look, deliberately: one answer to
// "where does the kernel token come from", not one per language.
func ResolveNodeToken(explicit string) string {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return explicit
	}
	if env := strings.TrimSpace(os.Getenv("AURA_TOKEN")); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".aura", "node.token"))
	if err != nil {
		// Empty is correct rather than an error: a kernel started with
		// --no-auth has no token file and asks for no credential.
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// checkPermissions refuses a credential anyone on the box can read. A token is
// a credential; a credential with the permissions of a log file is a shared one.
func checkPermissions(path string) error {
	if runtime.GOOS == "windows" {
		// Windows permissions are ACLs, and a mode read here says nothing
		// useful about them. Checking the mode would report false comfort.
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("%s is readable by other users (mode %04o); run: chmod 600 %s", path, mode, path)
	}
	return nil
}
