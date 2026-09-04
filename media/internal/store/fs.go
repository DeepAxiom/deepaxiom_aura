package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// FS stores objects as files under a root directory.
//
// This is the implementation a single node runs, and it is enough to serve an
// address: the API serves the same tree read-only. An S3-compatible store slots
// in beside it when output has to outlive the machine that made it.
type FS struct {
	Root string
	// BaseURL prefixes every address. Empty means addresses are relative
	// ("/media/<key>"), which is what a node serving its own output wants.
	BaseURL string
	// Prefix is the URL path the API serves this store under.
	Prefix string
}

// NewFS prepares the root directory.
func NewFS(root, baseURL, prefix string) (*FS, error) {
	if root == "" {
		return nil, errors.New("the object store needs a root directory")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("object store root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("object store root: %w", err)
	}
	if prefix == "" {
		prefix = "/media"
	}
	return &FS{Root: abs, BaseURL: strings.TrimSuffix(baseURL, "/"), Prefix: "/" + strings.Trim(prefix, "/")}, nil
}

// Path is the local file a key lives at. It refuses keys that escape the root.
func (s *FS) Path(key string) (string, error) {
	clean, err := CleanKey(key)
	if err != nil {
		return "", err
	}
	full := filepath.Join(s.Root, filepath.FromSlash(clean))
	// Belt and braces: CleanKey already rejects "..", and this catches anything
	// a symlink or a future edit to CleanKey lets through.
	if !strings.HasPrefix(full, s.Root+string(os.PathSeparator)) && full != s.Root {
		return "", fmt.Errorf("key %q resolves outside the store", key)
	}
	return full, nil
}

// Put writes one object, hashing as it goes so the digest costs no second read.
func (s *FS) Put(ctx context.Context, key string, r io.Reader) (Object, error) {
	full, err := s.Path(key)
	if err != nil {
		return Object{}, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return Object{}, fmt.Errorf("store %s: %w", key, err)
	}
	// A temp file in the destination directory, renamed on success: a reader
	// never sees a half-written object, and a crash leaves rubbish rather than
	// a truncated file that looks complete.
	tmp, err := os.CreateTemp(filepath.Dir(full), ".part-*")
	if err != nil {
		return Object{}, fmt.Errorf("store %s: %w", key, err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, digest), readerWithContext{ctx: ctx, r: r})
	if err != nil {
		return Object{}, fmt.Errorf("store %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return Object{}, fmt.Errorf("store %s: %w", key, err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return Object{}, fmt.Errorf("store %s: %w", key, err)
	}
	clean, _ := CleanKey(key)
	return Object{Key: clean, Bytes: written, SHA256: hex.EncodeToString(digest.Sum(nil))}, nil
}

// PutTree copies a directory in, keeping its layout.
func (s *FS) PutTree(ctx context.Context, prefix, dir string) ([]Object, error) {
	if _, err := CleanKey(prefix); err != nil {
		return nil, err
	}
	var out []Object
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		obj, err := s.Put(ctx, path.Join(prefix, filepath.ToSlash(rel)), f)
		if err != nil {
			return err
		}
		out = append(out, obj)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store tree %s: %w", prefix, err)
	}
	return out, nil
}

// Open reads an object back.
func (s *FS) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	full, err := s.Path(key)
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	if info.IsDir() {
		return nil, 0, ErrNotFound
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// Address is where a player fetches this key.
func (s *FS) Address(key string) string {
	clean, err := CleanKey(key)
	if err != nil {
		return ""
	}
	return s.BaseURL + s.Prefix + "/" + clean
}

// readerWithContext makes a long upload cancellable: without it, a client that
// disappears mid-upload keeps a goroutine copying until it finishes.
type readerWithContext struct {
	ctx context.Context
	r   io.Reader
}

func (r readerWithContext) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
