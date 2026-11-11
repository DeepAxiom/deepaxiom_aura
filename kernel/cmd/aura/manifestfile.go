package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The C1 manifest file, under both of its names.
//
// What this runtime runs was called a *skill* when the file was named, and
// the manifest was `skill.yaml`. The word has since come to mean a prompt
// handed to a language model, which is not what these are: an Expertise is a
// typed, running function that is expert at one thing, with declared ports
// and permissions. So the file is now `expertise.yaml`.
//
// The old name keeps working, in both directions, and that is deliberate
// rather than a transition period with an end date:
//
//   - Reading: every place that loads a manifest tries `expertise.yaml` and
//     falls back to `skill.yaml`, so a package written before the rename
//     publishes, installs and runs unchanged.
//   - Writing: `aura publish` puts a `skill.yaml` copy in the artifact of a
//     package that only has the new name, so a node or an SDK that predates
//     the rename can still install and run what a current one published.
//
// The file's contents did not change at all. C1 is a frozen contract; this is
// the name on the envelope, not what is in it.
const (
	manifestFile       = "expertise.yaml"
	legacyManifestFile = "skill.yaml"
)

// readManifestFile reads a package directory's manifest and reports which
// file it came from.
func readManifestFile(dir string) (raw []byte, name string, err error) {
	for _, candidate := range []string{manifestFile, legacyManifestFile} {
		raw, err = os.ReadFile(filepath.Join(dir, candidate))
		if err == nil {
			return raw, candidate, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, candidate, err
		}
	}
	return nil, manifestFile, fmt.Errorf("no %s (or %s) in %s: %w",
		manifestFile, legacyManifestFile, dir, fs.ErrNotExist)
}
