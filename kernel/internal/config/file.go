// Package config loads the optional, git-friendly config file that seeds
// skill parameters at startup ("aura up --config aura.config.yaml").
//
// It is a separate, lower-priority layer from the runtime overrides a user
// sets live via the UI/API (see gateway's skill-config endpoints and
// registry.Manifest.EffectiveConfig for the precedence rule). The file is
// read once, at startup; picking up an edit needs a restart — no live
// file-watching, on purpose, to keep this simple and predictable.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// File is the shape of the config file:
//
//	skills:
//	  "org/category/name":
//	    some_key: some_value
type File struct {
	Skills map[string]map[string]any `yaml:"skills"`
}

// Load reads and parses a config file. A missing path is not an error at
// the call site's discretion — callers pass an empty path to mean "none".
func Load(path string) (*File, error) {
	if path == "" {
		return &File{Skills: map[string]map[string]any{}}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse config file %q: %w", path, err)
	}
	if f.Skills == nil {
		f.Skills = map[string]map[string]any{}
	}
	return &f, nil
}
