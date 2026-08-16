// Package sandbox turns a skill's declared C1 `permissions` into something
// the runtime enforces when it starts that skill.
//
// # The gap this addresses, and how much of it
//
// Until now `aura run` spawned a skill with the full privileges of whoever
// started the node, and `permissions` was a line the registry printed at
// install time. The README has said so plainly for several releases, calling
// it the largest remaining security gap. It still is — this narrows it rather
// than closing it, and the distinction matters enough to state up front.
//
// The 2026 consensus on running model-adjacent or third-party code is
// unambiguous: containers share a kernel and are not an isolation boundary,
// and microVMs (Firecracker, Cloud Hypervisor, Kata) or a syscall-interposing
// kernel (gVisor) are what production uses. That consensus is right, and this
// package does not pretend otherwise. What it does:
//
//	Backend      Isolation                       Portable   Status
//	none         none — today's behaviour        yes        implemented
//	process      OS-level, best effort           yes        implemented
//	wasm         WASI, genuinely enforced        yes        implemented (wasmrt)
//	microvm      a real boundary                 Linux+KVM  INTERFACE ONLY
//
// `microvm` is declared and refused at startup with an explanation, not
// stubbed. A backend that silently degraded to something weaker would be worse
// than no backend, because an operator would believe they had isolation they
// did not. When it lands it slots in behind Backend without touching callers.
//
// # What `process` actually buys
//
// A scrubbed environment, a working-directory jail, no inherited handles, and
// a declared-egress check performed before launch. That stops accidental
// credential inheritance and casual filesystem wandering. It does **not** stop
// hostile code: a determined process can still reach the network and the
// filesystem through syscalls no portable Go runtime can intercept. It is a
// seatbelt, and it is described as one everywhere it appears.
//
// For code you have reason to distrust, the honest options today are
// `format: wasm` — where `permissions` really is enforced, by wazero — or
// running the node itself inside a boundary you trust.
package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Backend names an isolation strategy.
type Backend string

const (
	// BackendNone runs the skill with the node's own privileges. Explicit
	// rather than implicit, so a data directory records that nobody asked
	// for isolation.
	BackendNone Backend = "none"
	// BackendProcess applies OS-level hardening: scrubbed environment, cwd
	// jail, no inherited handles. Portable, best-effort, not a boundary
	// against hostile code.
	BackendProcess Backend = "process"
	// BackendWasm hosts the skill inside the kernel's wazero runtime, where
	// filesystem and egress permissions are genuinely enforced. Requires
	// `format: wasm` in the manifest.
	BackendWasm Backend = "wasm"
	// BackendMicroVM is a real isolation boundary and is not implemented.
	// See the package comment.
	BackendMicroVM Backend = "microvm"
)

// Backends lists every backend name, for validation and for `--help`.
var Backends = []Backend{BackendNone, BackendProcess, BackendWasm, BackendMicroVM}

// Permissions is the C1 `permissions` block, parsed.
type Permissions struct {
	// EgressHTTP is the exact hostname allowlist. Empty means no egress was
	// declared; nil and empty are the same thing.
	EgressHTTP []string
	// Filesystem is "none", "read:<path>" or "write:<path>".
	Filesystem string
	// Channels is "declared-only" or empty.
	Channels string
}

// ParsePermissions reads the manifest's `permissions` map.
func ParsePermissions(raw map[string]any) Permissions {
	var p Permissions
	if raw == nil {
		return p
	}
	if v, ok := raw["filesystem"].(string); ok {
		p.Filesystem = strings.TrimSpace(v)
	}
	if v, ok := raw["channels"].(string); ok {
		p.Channels = strings.TrimSpace(v)
	}
	switch v := raw["egress_http"].(type) {
	case []any:
		for _, item := range v {
			if host, ok := item.(string); ok && strings.TrimSpace(host) != "" {
				p.EgressHTTP = append(p.EgressHTTP, strings.TrimSpace(host))
			}
		}
	case []string:
		p.EgressHTTP = append(p.EgressHTTP, v...)
	}
	sort.Strings(p.EgressHTTP)
	return p
}

// FilesystemGrant splits "read:/data" into its mode and path. Returns
// ("", "", false) when nothing was granted.
func (p Permissions) FilesystemGrant() (mode, path string, granted bool) {
	if p.Filesystem == "" || p.Filesystem == "none" {
		return "", "", false
	}
	mode, path, found := strings.Cut(p.Filesystem, ":")
	if !found || (mode != "read" && mode != "write") || strings.TrimSpace(path) == "" {
		return "", "", false
	}
	return mode, filepath.Clean(strings.TrimSpace(path)), true
}

// Spec is a resolved launch plan: which backend, under which permissions, in
// which directory.
type Spec struct {
	Backend     Backend
	Permissions Permissions
	// WorkDir is where the skill runs. For BackendProcess this is also the
	// only directory it is pointed at.
	WorkDir string
	// SkillID is carried for error messages, which are the main product of
	// this package when something is refused.
	SkillID string
}

// Resolve decides how to launch a skill, refusing combinations that would
// promise isolation the runtime cannot deliver.
func Resolve(backend Backend, skillID, format, workDir string, perms map[string]any) (Spec, error) {
	s := Spec{
		Backend: backend, SkillID: skillID, WorkDir: workDir,
		Permissions: ParsePermissions(perms),
	}
	if s.Backend == "" {
		s.Backend = BackendNone
	}

	known := false
	for _, b := range Backends {
		if s.Backend == b {
			known = true
			break
		}
	}
	if !known {
		return Spec{}, fmt.Errorf("unknown sandbox backend %q (want one of %v)", s.Backend, Backends)
	}

	switch s.Backend {
	case BackendMicroVM:
		// Refused, not degraded. An operator who asked for a microVM and got a
		// scrubbed environment instead would be running third-party code under
		// a belief the runtime manufactured for them.
		return Spec{}, fmt.Errorf(
			"sandbox backend %q is declared but not implemented in this build.\n\n"+
				"  A microVM boundary (Firecracker, Cloud Hypervisor, Kata) needs Linux with\n"+
				"  KVM, and shipping an untested one would be worse than shipping none: you\n"+
				"  would believe you had isolation you did not.\n\n"+
				"  Today: use `format: wasm` for code you distrust — there `permissions` is\n"+
				"  genuinely enforced by wazero — or run the node itself inside a boundary\n"+
				"  you already trust.", BackendMicroVM)

	case BackendWasm:
		if format != "wasm" {
			return Spec{}, fmt.Errorf(
				"skill %q declares format %q but the %q sandbox hosts only `format: wasm`",
				skillID, format, BackendWasm)
		}

	case BackendProcess:
		if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
			return Spec{}, fmt.Errorf("the %q sandbox has no implementation for %s",
				BackendProcess, runtime.GOOS)
		}
		if _, path, granted := s.Permissions.FilesystemGrant(); granted {
			if _, err := os.Stat(path); err != nil {
				return Spec{}, fmt.Errorf(
					"skill %q declares filesystem access to %q, which does not exist — "+
						"refusing rather than launching with a grant that silently does nothing",
					skillID, path)
			}
		}
	}
	return s, nil
}

// EnvFor builds the environment a sandboxed skill starts with.
//
// An allowlist, not a denylist. A skill inherits the handful of variables it
// genuinely needs to find the kernel and a Python interpreter, plus whatever
// its manifest asked for by name. Everything else — cloud credentials, SSH
// agents, API keys for services this skill never heard of — stays behind.
//
// Inheriting the parent environment is how a skill installed from a registry
// ends up holding an operator's production credentials without anyone
// deciding that it should.
func EnvFor(s Spec, parent []string, passthrough []string) []string {
	if s.Backend == BackendNone {
		return parent
	}

	keep := map[string]bool{
		// Finding the kernel and speaking to it.
		"AURA_WS_URL": true, "AURA_TOKEN": true,
		// Making a language runtime work at all.
		"PATH": true, "PYTHONPATH": true, "PYTHONUNBUFFERED": true,
		"HOME": true, "USERPROFILE": true, "TMPDIR": true, "TEMP": true, "TMP": true,
		"SYSTEMROOT": true, "COMSPEC": true, "PATHEXT": true,
		"LANG": true, "LC_ALL": true, "TZ": true,
	}
	for _, name := range passthrough {
		keep[strings.ToUpper(strings.TrimSpace(name))] = true
	}

	out := make([]string, 0, len(keep))
	for _, kv := range parent {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if keep[strings.ToUpper(name)] {
			out = append(out, kv)
		}
	}
	sort.Strings(out)
	return out
}

// Describe renders the launch plan for an operator, so what was and was not
// enforced is visible at the moment of starting rather than in documentation.
func Describe(s Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "sandbox: %s", s.Backend)
	switch s.Backend {
	case BackendNone:
		b.WriteString("  — NO isolation: this skill runs with the privileges of whoever " +
			"started it, and `permissions` is a declaration, not a boundary")
	case BackendProcess:
		b.WriteString("  — scrubbed environment, cwd jail, no inherited handles.")
		b.WriteString("\n  Best effort: this stops accidental credential inheritance and " +
			"casual filesystem access.\n  It does NOT contain hostile code — use " +
			"`format: wasm` for that.")
	case BackendWasm:
		b.WriteString("  — WASI: filesystem and egress genuinely enforced by the runtime")
	}
	if hosts := s.Permissions.EgressHTTP; len(hosts) > 0 {
		fmt.Fprintf(&b, "\n  egress_http: %s", strings.Join(hosts, ", "))
	} else {
		b.WriteString("\n  egress_http: none declared")
	}
	if mode, path, granted := s.Permissions.FilesystemGrant(); granted {
		fmt.Fprintf(&b, "\n  filesystem:  %s %s", mode, path)
	} else {
		b.WriteString("\n  filesystem:  none")
	}
	return b.String()
}
