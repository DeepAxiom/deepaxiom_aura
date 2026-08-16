package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The property this package exists for: a backend that cannot deliver what it
// promises is refused, never quietly downgraded.
func TestMicroVMIsRefusedNotDowngraded(t *testing.T) {
	_, err := Resolve(BackendMicroVM, "acme/logical/x", "source", t.TempDir(), nil)
	if err == nil {
		t.Fatal("the microvm backend resolved; an operator would believe they had " +
			"isolation the runtime cannot provide")
	}
	for _, want := range []string{"not implemented", "wasm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q so the reader knows what to do instead:\n%v",
				want, err)
		}
	}
}

func TestUnknownBackendIsRefused(t *testing.T) {
	if _, err := Resolve("firecracker-ish", "acme/logical/x", "source", t.TempDir(), nil); err == nil {
		t.Fatal("an unknown backend resolved")
	}
}

// The wasm backend is the only one where `permissions` is genuinely enforced,
// so pointing it at a source skill has to fail rather than imply otherwise.
func TestWasmBackendRequiresWasmFormat(t *testing.T) {
	if _, err := Resolve(BackendWasm, "acme/logical/x", "source", t.TempDir(), nil); err == nil {
		t.Fatal("the wasm sandbox accepted a format:source skill")
	}
	if _, err := Resolve(BackendWasm, "acme/logical/x", "wasm", t.TempDir(), nil); err != nil {
		t.Fatalf("the wasm sandbox refused a format:wasm skill: %v", err)
	}
}

// A grant to a path that does not exist is a grant that silently does
// nothing — refused, so the mistake surfaces at launch rather than as
// confusing behaviour later.
func TestFilesystemGrantMustExist(t *testing.T) {
	perms := map[string]any{"filesystem": "read:" + filepath.Join(t.TempDir(), "absent")}
	if _, err := Resolve(BackendProcess, "acme/logical/x", "source", t.TempDir(), perms); err == nil {
		t.Fatal("a grant to a nonexistent path was accepted")
	}

	dir := t.TempDir()
	ok := map[string]any{"filesystem": "read:" + dir}
	if _, err := Resolve(BackendProcess, "acme/logical/x", "source", dir, ok); err != nil {
		t.Fatalf("a grant to an existing path was refused: %v", err)
	}
}

// The environment is an allowlist. This is the concrete thing `process` buys:
// a skill installed from a registry does not inherit an operator's cloud
// credentials just by being started.
func TestEnvIsAnAllowlist(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"PYTHONPATH=/sdk",
		"AWS_SECRET_ACCESS_KEY=very-secret",
		"GITHUB_TOKEN=ghp_secret",
		"OPENAI_API_KEY=sk-secret",
		"SSH_AUTH_SOCK=/tmp/agent",
		"HOME=/home/dev",
	}
	spec, err := Resolve(BackendProcess, "acme/logical/x", "source", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(EnvFor(spec, parent, nil), "\n")

	for _, leaked := range []string{"AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN", "OPENAI_API_KEY", "SSH_AUTH_SOCK"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%s survived into a sandboxed skill's environment", leaked)
		}
	}
	for _, needed := range []string{"PATH=", "PYTHONPATH=", "HOME="} {
		if !strings.Contains(got, needed) {
			t.Errorf("%s was scrubbed but a skill cannot run without it", needed)
		}
	}
}

// A skill that genuinely needs a credential says so, by name, at launch.
func TestEnvPassthroughIsExplicit(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "PG_CDC_DSN=postgres://x", "AWS_SECRET_ACCESS_KEY=nope"}
	spec, _ := Resolve(BackendProcess, "acme/sensorial/cdc", "source", t.TempDir(), nil)
	got := strings.Join(EnvFor(spec, parent, []string{"PG_CDC_DSN"}), "\n")

	if !strings.Contains(got, "PG_CDC_DSN=") {
		t.Error("an explicitly passed-through variable was scrubbed")
	}
	if strings.Contains(got, "AWS_SECRET_ACCESS_KEY") {
		t.Error("passing one variable through let an unrelated one leak")
	}
}

// The `none` backend must behave exactly as the runtime did before this
// package existed, or upgrading changes behaviour nobody asked to change.
func TestNoneBackendIsUnchanged(t *testing.T) {
	spec, err := Resolve(BackendNone, "acme/logical/x", "source", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := os.Environ()
	if got := EnvFor(spec, parent, nil); len(got) != len(parent) {
		t.Fatalf("the none backend altered the environment: %d vars in, %d out",
			len(parent), len(got))
	}
}

// Describe is the operator-facing product of this package, and the `none`
// case has to be blunt about offering nothing.
func TestDescribeIsHonestAboutNone(t *testing.T) {
	spec, _ := Resolve(BackendNone, "acme/logical/x", "source", t.TempDir(), nil)
	d := Describe(spec)
	if !strings.Contains(d, "NO isolation") {
		t.Fatalf("the none backend must say plainly that it isolates nothing:\n%s", d)
	}

	spec, _ = Resolve(BackendProcess, "acme/logical/x", "source", t.TempDir(), nil)
	d = Describe(spec)
	if !strings.Contains(d, "does NOT contain hostile code") {
		t.Fatalf("the process backend must not read as a security boundary:\n%s", d)
	}
}

func TestParsePermissions(t *testing.T) {
	p := ParsePermissions(map[string]any{
		"egress_http": []any{"api.example.com", "  b.example.com  ", ""},
		"filesystem":  "write:/data/out",
		"channels":    "declared-only",
	})
	if len(p.EgressHTTP) != 2 || p.EgressHTTP[0] != "api.example.com" {
		t.Fatalf("egress parsed as %v", p.EgressHTTP)
	}
	mode, path, granted := p.FilesystemGrant()
	if !granted || mode != "write" || filepath.ToSlash(path) != "/data/out" {
		t.Fatalf("filesystem grant parsed as %q %q %v", mode, path, granted)
	}

	for _, none := range []string{"", "none", "garbage", "read:"} {
		if _, _, granted := (Permissions{Filesystem: none}).FilesystemGrant(); granted {
			t.Errorf("%q parsed as a grant", none)
		}
	}
}
