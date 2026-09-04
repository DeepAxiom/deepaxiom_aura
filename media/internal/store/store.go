// Package store is where the bytes live: sources in, packaged output out.
//
// The interface is small on purpose. What a consumer is handed is an address,
// and the only thing that changes when this moves from a directory on disk to a
// bucket behind a CDN is which implementation is wired in — not the queue, not
// the pipeline, and nothing downstream.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Object is a stored blob.
type Object struct {
	Key    string
	Bytes  int64
	SHA256 string
}

// Store holds objects under keys and can say where a key is fetchable.
type Store interface {
	// Put writes a key, returning its size and digest. It overwrites.
	Put(ctx context.Context, key string, r io.Reader) (Object, error)
	// PutTree copies a local directory in under a key prefix, keeping the
	// directory layout: an HLS package is a tree of playlists and segments
	// whose relative paths are what the playlists reference.
	PutTree(ctx context.Context, prefix, dir string) ([]Object, error)
	// Open reads a key back.
	Open(ctx context.Context, key string) (io.ReadCloser, int64, error)
	// Address is where a player fetches this key. It is public: the output of
	// this subsystem is public-zone bytes, which is the whole reason it does
	// not run inside the process that handles PHI.
	Address(key string) string
}

// ErrNotFound is what Open returns for a key that is not there.
var ErrNotFound = errors.New("no such object")

// CleanKey validates a key and returns its normalised form.
//
// A key comes from a request in the end, so this is a security check rather
// than tidiness: without it, "../../etc/passwd" is a valid place to write.
func CleanKey(key string) (string, error) {
	key = strings.ReplaceAll(strings.TrimSpace(key), "\\", "/")
	key = strings.TrimPrefix(key, "./")
	if key == "" {
		return "", errors.New("empty key")
	}
	if strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("key %q is absolute", key)
	}
	if strings.Contains(key, "//") {
		return "", fmt.Errorf("key %q has an empty path segment", key)
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("key %q escapes the store", key)
		}
		if strings.ContainsAny(segment, "\x00:*?\"<>|") {
			return "", fmt.Errorf("key %q has a segment no filesystem or bucket will take", key)
		}
	}
	return key, nil
}
