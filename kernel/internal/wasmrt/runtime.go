// Package wasmrt is the sandbox for kernel primitive... no — it is
// deliberately NOT a kernel primitive. It is what makes C1 rule 3
// ("permissions" is enforced, not merely declared) true for skills of
// `format: wasm`. Everything in here is wazero and
// nothing else: no registry, no executor, no HTTP — the same purity
// discipline the ledger package holds itself to, for the same reason. A
// skill of `format: source` never touches this package at all.
package wasmrt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// Runtime hosts every wasm skill on this node — one wazero.Runtime shared
// across every compiled module, the way one *http.Client is shared across
// requests rather than built per call. Compiling a module (parsing and
// validating the binary) is the expensive step; instantiating a compiled
// module is cheap, which is what makes "one instantiation per delivery" —
// see Module.Invoke — an acceptable cost rather than a bottleneck.
type Runtime struct {
	rt         wazero.Runtime
	httpClient *http.Client // shared by every Invoke's env.http_fetch calls — see http_import.go
}

// New builds a Runtime and links two host modules into it: WASI preview1
// (every v1 wasm skill's stdin/stdout/filesystem) and "env" (just
// http_fetch — see http_import.go), the only custom host ABI this package
// defines. http_fetch is always linked; what makes egress_http real is that
// it always refuses a domain that Permission.HTTPDomains does not name (see
// Invoke), not whether the import exists.
//
// WithCloseOnContextDone makes every later Invoke's ctx argument
// load-bearing: a module still running when ctx is done — including one
// blocked inside an http_fetch call — gets closed out from under it rather
// than left to run forever, which is what turns a runaway or hostile guest
// into a bounded failure instead of a hung kernel.
func New(ctx context.Context) (*Runtime, error) {
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true))
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("link WASI preview1: %w", err)
	}
	if err := registerHTTPFetch(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("link env.http_fetch: %w", err)
	}
	// CheckRedirect is what keeps permissions.egress_http true of every hop
	// rather than only of the first — see checkRedirect in http_import.go.
	// The per-invocation grant travels on the request context, so one client
	// is still correct for every concurrent Invoke.
	return &Runtime{rt: rt, httpClient: &http.Client{CheckRedirect: checkRedirect}}, nil
}

// Close releases the runtime and every module compiled against it.
func (r *Runtime) Close(ctx context.Context) error { return r.rt.Close(ctx) }

// Compile parses and validates a wasm binary once. The result should be
// kept and reused for every future delivery to that skill — recompiling
// per delivery would turn the cheap half of this design into the expensive
// half.
func (r *Runtime) Compile(ctx context.Context, wasmBytes []byte) (*Module, error) {
	compiled, err := r.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("compile wasm module: %w", err)
	}
	return &Module{rt: r.rt, compiled: compiled, httpClient: r.httpClient}, nil
}

// Module is one compiled wasm binary, instantiated fresh for every delivery.
type Module struct {
	rt         wazero.Runtime
	compiled   wazero.CompiledModule
	httpClient *http.Client
}

// Invoke runs one WASI "command" instantiation of the module (the same
// model as a CGI script, or `_start` in any WASI-targeting language,
// including Go's own GOOS=wasip1 output): payload goes in on stdin and the
// reader is exhausted before the guest is run, so a guest reading to EOF
// sees exactly this delivery's bytes and nothing from any other; stdout and
// stderr are captured whole, since v1 has no streaming guest contract (see
// perm is applied as a WASI directory preopen, or not applied
// at all when perm.FSPath is empty — wazero's own default with no FSConfig
// is already "no file access," so denial costs this function nothing extra.
//
// A fresh instantiation per call, rather than one long-lived instance
// reused across deliveries, is deliberate: it is what the WASI command
// model is built for, and it guarantees one delivery's state — a global
// variable, an open file descriptor — can never leak into the next one.
//
// exitCode is only meaningful when err is nil: a *sys.ExitError from the
// guest itself (calling proc_exit, which is how Go's wasip1 runtime always
// exits, success included) is unwrapped into exitCode rather than
// propagated as err, because a non-zero exit is the guest's own decision,
// not a failure of the sandbox running it. Any other non-nil err means the
// module never got to run its own logic at all — a link failure, a trap, or
// ctx firing mid-execution.
func (m *Module) Invoke(ctx context.Context, payload []byte, perm Permission) (stdout, stderr []byte, exitCode uint32, err error) {
	ctx = withHTTPFetchContext(ctx, perm, m.httpClient)

	var stdoutBuf, stderrBuf bytes.Buffer
	cfg := wazero.NewModuleConfig().
		WithName(""). // anonymous: the same CompiledModule instantiates concurrently, once per delivery
		WithStdin(bytes.NewReader(payload)).
		WithStdout(&stdoutBuf).
		WithStderr(&stderrBuf)

	if perm.FSPath != "" {
		fsCfg := wazero.NewFSConfig()
		if perm.FSWritable {
			fsCfg = fsCfg.WithDirMount(perm.FSPath, "/")
		} else {
			fsCfg = fsCfg.WithReadOnlyDirMount(perm.FSPath, "/")
		}
		cfg = cfg.WithFSConfig(fsCfg)
	}

	mod, instErr := m.rt.InstantiateModule(ctx, m.compiled, cfg)
	if mod != nil {
		defer mod.Close(ctx)
	}
	stdout, stderr = stdoutBuf.Bytes(), stderrBuf.Bytes()

	if instErr == nil {
		return stdout, stderr, 0, nil
	}
	var exitErr *sys.ExitError
	if errors.As(instErr, &exitErr) {
		// wazero reserves two exit codes for its own ctx integration
		// (sys.ExitCodeContextCanceled / ExitCodeDeadlineExceeded) — a
		// forced close because ctx fired is not the guest's own decision
		// the way any other exit code is, and a caller that cannot tell
		// "the guest exited 0xefffffff" apart from "the sandbox killed it"
		// would misread a runaway guest as a well-behaved one. Surface it
		// as ctx.Err() instead of folding it into exitCode.
		switch exitErr.ExitCode() {
		case sys.ExitCodeContextCanceled, sys.ExitCodeDeadlineExceeded:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return stdout, stderr, 0, ctxErr
			}
			return stdout, stderr, 0, instErr
		default:
			return stdout, stderr, exitErr.ExitCode(), nil
		}
	}
	return stdout, stderr, 0, instErr
}
