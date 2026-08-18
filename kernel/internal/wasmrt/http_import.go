package wasmrt

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// env.http_fetch — the one custom host import a v1 wasm skill may use, and
// what makes `permissions.egress_http` real rather than merely declared
// WASI preview1 has no sockets at all, so this is
// necessarily a bespoke ABI, not a standard one — kept as small as the job
// allows: the guest owns both buffers itself (a package-level array is a
// stable address in its own linear memory), so the host never needs to call
// back into a guest-exported allocator.
//
// Guest-side contract (`//go:wasmimport env http_fetch`):
//
//	func httpFetch(reqPtr, reqLen, respBufPtr, respBufCap uint32) int32
//
// reqPtr/reqLen point at a JSON {"method":"GET","url":"https://…","body":"…"?}
// the guest wrote itself. The return value is either the number of response
// body bytes written to respBufPtr (capped at respBufCap — a response too
// large is truncated to fit, the same way a short read() is, not silently
// dropped), or one of three negative sentinels:
const (
	httpFetchDenied     int32 = -1 // the request's host is not in permissions.egress_http
	httpFetchFailed     int32 = -2 // the fetch itself failed: network, timeout, non-2xx is NOT here — only transport failure
	httpFetchBadRequest int32 = -3 // the guest's own request was malformed
)

type httpFetchRequest struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Body   string `json:"body,omitempty"`
}

// httpFetchScope is what one Invoke call's http_fetch calls are allowed to
// do — carried on context.Context (see withHTTPFetchContext) rather than as
// a field on Runtime/Module, because it varies per skill and per call, and
// ctx is the one thing wazero already threads from Invoke's argument
// through to a host import the running guest calls.
type httpFetchScope struct {
	domains []string
	client  *http.Client
}

type httpFetchScopeKey struct{}

func withHTTPFetchContext(ctx context.Context, perm Permission, client *http.Client) context.Context {
	return context.WithValue(ctx, httpFetchScopeKey{}, &httpFetchScope{domains: perm.HTTPDomains, client: client})
}

// registerHTTPFetch links "env".http_fetch once, for the Runtime's whole
// lifetime — every compiled Module shares this one definition. What differs
// per invocation is read back from ctx inside hostHTTPFetch, not baked into
// the function itself, which is what lets two concurrent invocations with
// different egress_http grants never cross-contaminate (see http_import_test.go).
func registerHTTPFetch(ctx context.Context, rt wazero.Runtime) error {
	_, err := rt.NewHostModuleBuilder("env").
		NewFunctionBuilder().
		WithFunc(hostHTTPFetch).
		Export("http_fetch").
		Instantiate(ctx)
	return err
}

func hostHTTPFetch(ctx context.Context, mod api.Module, reqPtr, reqLen, respBufPtr, respBufCap uint32) int32 {
	scope, _ := ctx.Value(httpFetchScopeKey{}).(*httpFetchScope)
	if scope == nil {
		return httpFetchDenied // Invoke always sets this; nil means something is badly wrong — fail closed
	}

	reqBytes, ok := mod.Memory().Read(reqPtr, reqLen)
	if !ok {
		return httpFetchBadRequest
	}
	var req httpFetchRequest
	if err := json.Unmarshal(reqBytes, &req); err != nil || req.URL == "" || req.Method == "" {
		return httpFetchBadRequest
	}
	target, err := url.Parse(req.URL)
	if err != nil || target.Hostname() == "" {
		return httpFetchBadRequest
	}
	if !allowedHost(target.Hostname(), scope.domains) {
		return httpFetchDenied
	}

	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		return httpFetchBadRequest
	}
	resp, err := scope.client.Do(httpReq)
	if err != nil {
		return httpFetchFailed
	}
	defer resp.Body.Close()

	// Read one byte past the cap so a truncation can be told apart from an
	// exact fit — irrelevant to the return value today (both cases return
	// respBufCap) but kept because silently conflating them is the kind of
	// thing that becomes a real bug the day this ABI grows a "was truncated"
	// signal.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, int64(respBufCap)+1))
	if err != nil {
		return httpFetchFailed
	}
	n := len(respBody)
	if uint32(n) > respBufCap {
		n = int(respBufCap)
	}
	if n > 0 && !mod.Memory().Write(respBufPtr, respBody[:n]) {
		return httpFetchFailed
	}
	return int32(n)
}

func allowedHost(host string, domains []string) bool {
	for _, d := range domains {
		if d == host {
			return true
		}
	}
	return false
}
