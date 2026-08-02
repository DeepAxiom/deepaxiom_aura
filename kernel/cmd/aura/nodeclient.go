package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gorilla/websocket"
)

// Talking to a local node from the CLI.
//
// Every subcommand that is a *client* of a running node goes through here, so
// there is one answer to "where does the token come from" rather than one per
// command. Two sources, in order:
//
//  1. AURA_TOKEN in the environment — for CI, containers, and any node whose
//     data dir this machine cannot read.
//  2. <data-dir>/node.token — the ordinary case, where the CLI and the node
//     run as the same user on the same machine, and neither the operator nor a
//     script should have to pass anything.
//
// A node started with --no-auth has no token file and needs none: the empty
// string means "send nothing", and the node will not ask.

// nodeToken resolves the bearer token for a local node.
func nodeToken(dataDir string) string {
	if t := strings.TrimSpace(os.Getenv("AURA_TOKEN")); t != "" {
		return t
	}
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, "node.token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// nodeClient is an authenticated client for one running node.
type nodeClient struct {
	base  string // http://localhost:9080
	ws    string // ws://localhost:9080
	token string
}

func newNodeClient(port int) *nodeClient {
	return &nodeClient{
		base:  fmt.Sprintf("http://localhost:%d", port),
		ws:    fmt.Sprintf("ws://localhost:%d", port),
		token: nodeToken(""),
	}
}

func (c *nodeClient) authorize(h http.Header) {
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
}

// do performs an authenticated request and returns the body, turning a 401
// into an explanation rather than a bare status code — "401" tells an operator
// nothing about which of the several possible causes they have hit.
func (c *nodeClient) do(method, path string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authorize(req.Header)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kernel not reachable on %s — is it up? (%w)", c.base, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, c.unauthorized()
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s %s: %s", method, path, e.Error)
		}
		return nil, fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	return data, nil
}

// raw performs an authenticated request and hands back the status and body
// without judging them, for the callers that branch on the status themselves.
// A 401 is still turned into a fatal explanation here, because no caller has
// anything useful to do with it and every one of them would otherwise print a
// confusing downstream error instead.
func (c *nodeClient) raw(method, path string, body []byte) (int, []byte) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authorize(req.Header)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fatal(fmt.Errorf("kernel not reachable on %s — run `aura up` first (%w)", c.base, err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		fatal(err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		fatal(c.unauthorized())
	}
	return resp.StatusCode, data
}

// dial opens an authenticated WebSocket to the node.
func (c *nodeClient) dial(path string) (*websocket.Conn, error) {
	h := http.Header{}
	c.authorize(h)
	conn, resp, err := websocket.DefaultDialer.Dial(c.ws+path, h)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return nil, c.unauthorized()
		}
		return nil, fmt.Errorf("cannot open %s%s — is the kernel up? (%w)", c.ws, path, err)
	}
	return conn, nil
}

func (c *nodeClient) unauthorized() error {
	if c.token == "" {
		return fmt.Errorf("the node requires a token and none was found — " +
			"set AURA_TOKEN, or run the CLI as the user that owns <data-dir>/node.token")
	}
	return fmt.Errorf("the node rejected this token — it may belong to a different " +
		"data dir; check AURA_TOKEN or <data-dir>/node.token")
}
