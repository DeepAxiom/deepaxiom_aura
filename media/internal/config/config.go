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
