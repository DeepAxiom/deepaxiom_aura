package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanKeyRefusesEscapes(t *testing.T) {
	bad := []string{
		"",
		"/absolute/key",
		"../outside",
		"a/../../outside",
		"a//b",
		"./",
		"a/b/..",
		"a/\x00/b",
	}
	for _, key := range bad {
		if got, err := CleanKey(key); err == nil {
			t.Errorf("CleanKey(%q) = %q, want an error", key, got)
		}
	}
	good := map[string]string{
		"src/01ABC":               "src/01ABC",
		"out/01ABC/720p/init.mp4": "out/01ABC/720p/init.mp4",
		`out\01ABC\master.m3u8`:   "out/01ABC/master.m3u8", // a Windows path still names one key
		"./src/x":                 "src/x",
	}
	for key, want := range good {
		got, err := CleanKey(key)
		if err != nil {
			t.Errorf("CleanKey(%q): %v", key, err)
			continue
		}
		if got != want {
			t.Errorf("CleanKey(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestFSPutIsAtomicAndHashes(t *testing.T) {
	s, err := NewFS(t.TempDir(), "", "/media")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx := context.Background()

	obj, err := s.Put(ctx, "src/one", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if obj.Bytes != 5 {
		t.Errorf("size %d, want 5", obj.Bytes)
	}
	// sha256("hello")
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if obj.SHA256 != want {
		t.Errorf("digest %q, want %q", obj.SHA256, want)
	}

	r, size, err := s.Open(ctx, "src/one")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	if size != 5 {
		t.Errorf("open size %d, want 5", size)
	}
	body, _ := io.ReadAll(r)
	if string(body) != "hello" {
		t.Errorf("read %q, want hello", body)
	}

	// No .part-* left behind: a half-written object must never survive a Put.
	entries, err := os.ReadDir(filepath.Join(s.Root, "src"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".part-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestFSPutRefusesToWriteOutsideItsRoot(t *testing.T) {
	root := t.TempDir()
	s, err := NewFS(filepath.Join(root, "store"), "", "/media")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := s.Put(context.Background(), "../escaped", strings.NewReader("x")); err == nil {
		t.Fatal("wrote outside the store root")
	}
	if _, err := os.Stat(filepath.Join(root, "escaped")); err == nil {
		t.Fatal("a file landed outside the store root")
	}
}

func TestFSPutTreeKeepsTheLayout(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "master.m3u8"), "#EXTM3U")
	mustWrite(t, filepath.Join(dir, "720p", "index.m3u8"), "#EXTM3U 720")
	mustWrite(t, filepath.Join(dir, "720p", "seg_00001.m4s"), "segment")

	s, err := NewFS(t.TempDir(), "", "/media")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	objs, err := s.PutTree(context.Background(), "out/asset1", dir)
	if err != nil {
		t.Fatalf("put tree: %v", err)
	}
	if len(objs) != 3 {
		t.Fatalf("stored %d objects, want 3", len(objs))
	}
	// The playlists reference their segments by relative path, so the layout is
	// the contract: flattening it breaks playback.
	r, _, err := s.Open(context.Background(), "out/asset1/720p/seg_00001.m4s")
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	r.Close()
}

func TestFSOpenSaysNotFound(t *testing.T) {
	s, err := NewFS(t.TempDir(), "", "/media")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, _, err := s.Open(context.Background(), "src/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error is %v, want ErrNotFound", err)
	}
}

func TestAddress(t *testing.T) {
	local, err := NewFS(t.TempDir(), "", "/media")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if got := local.Address("out/a/master.m3u8"); got != "/media/out/a/master.m3u8" {
		t.Errorf("address %q", got)
	}
	published, err := NewFS(t.TempDir(), "https://media.example/", "/media")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if got := published.Address("out/a/master.m3u8"); got != "https://media.example/media/out/a/master.m3u8" {
		t.Errorf("address %q", got)
	}
	if got := published.Address("../escape"); got != "" {
		t.Errorf("address of an invalid key is %q, want empty", got)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
