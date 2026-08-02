package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Deriving a connector from traffic instead of writing one.
//
// `skills/connector` already reduces connecting a system to a few lines of
// YAML. This removes those lines too, for the case where the system exists and
// is already being used: put a proxy in front of it, use the app normally, and
// the shapes of its real calls become the connector.
//
// A derived-from-observation description beats a hand-written one for the same
// reason `aura replay` exists: recorded traffic is what the system actually
// does, not what someone believed it did.
//
//	aura observe --port 8080 --target http://localhost:3000
//	aura generate connector --from observed.jsonl --name my-app
//
// What is recorded is deliberately narrow: method, path, query parameter
// names, the *shape* of the request body (key names and JSON types), and the
// status. Never header values, never body values. A recording is something you
// might paste into an issue, so it must not be able to carry a token or a
// customer's name in the first place.

// observation is one recorded call, reduced to what a connector needs.
type observation struct {
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Query     []string          `json:"query,omitempty"`
	BodyShape map[string]string `json:"body_shape,omitempty"`
	Status    int               `json:"status"`
	AuthSeen  bool              `json:"auth_seen,omitempty"`
}

var sensitiveHeaders = map[string]bool{
	"authorization": true, "cookie": true, "set-cookie": true,
	"x-api-key": true, "proxy-authorization": true,
}

func cmdObserve(args []string) {
	fs := flag.NewFlagSet("observe", flag.ExitOnError)
	port := fs.Int("port", 8080, "port to listen on — point your app at this")
	target := fs.String("target", "", "the system to forward to, e.g. http://localhost:3000")
	out := fs.String("out", "observed.jsonl", "where to write observations")
	_ = fs.Parse(args)

	if *target == "" {
		fmt.Fprintln(os.Stderr, "error: --target is required (e.g. --target http://localhost:3000)")
		os.Exit(2)
	}
	upstream, err := url.Parse(*target)
	if err != nil || upstream.Host == "" {
		fmt.Fprintf(os.Stderr, "error: invalid --target %q\n", *target)
		os.Exit(2)
	}

	file, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot write %s: %v\n", *out, err)
		os.Exit(1)
	}
	defer file.Close()

	var mu sync.Mutex
	seen := 0
	record := func(o observation) {
		mu.Lock()
		defer mu.Unlock()
		line, _ := json.Marshal(o)
		_, _ = file.Write(append(line, '\n'))
		seen++
		fmt.Printf("  %-6s %-40s → %d\n", o.Method, o.Path, o.Status)
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := observation{Method: r.Method, Path: r.URL.Path}
		for name := range r.URL.Query() {
			o.Query = append(o.Query, name)
		}
		sort.Strings(o.Query)
		for name := range r.Header {
			if sensitiveHeaders[strings.ToLower(name)] {
				o.AuthSeen = true
			}
		}
		// Read the body to learn its shape, then put it back so the proxy can
		// forward the request unchanged — observing must not alter behaviour.
		if r.Body != nil {
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			_ = r.Body.Close()
			if err == nil {
				o.BodyShape = jsonShape(body)
				r.Body = io.NopCloser(strings.NewReader(string(body)))
				r.ContentLength = int64(len(body))
			}
		}

		rec := &statusRecorder{ResponseWriter: w, status: 200}
		proxy.ServeHTTP(rec, r)
		o.Status = rec.status
		record(o)
	})

	fmt.Printf(`observing %s → %s

Point your app at http://localhost:%d and use it normally. Requests are
forwarded unchanged; their shapes are appended to %s.
Header values and body values are never recorded — only names and types.

Then:  aura generate connector --from %s --name <name>

`, fmt.Sprintf("http://localhost:%d", *port), *target, *port, *out, *out)

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", *port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "\nproxy stopped: %v\n", err)
	}
	fmt.Printf("\nrecorded %d call(s) to %s\n", seen, *out)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// jsonShape reduces a JSON object to its top-level keys and their types. The
// values are deliberately dropped: a recording must not be able to carry a
// customer's name or an API token.
func jsonShape(body []byte) map[string]string {
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		return nil
	}
	shape := map[string]string{}
	for key, value := range parsed {
		switch value.(type) {
		case string:
			shape[key] = "string"
		case float64:
			shape[key] = "number"
		case bool:
			shape[key] = "bool"
		case []any:
			shape[key] = "array"
		case map[string]any:
			shape[key] = "object"
		default:
			shape[key] = "null"
		}
	}
	return shape
}
