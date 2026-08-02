package wasmrt

import (
	"fmt"
	"strings"
)

// Permission is what one wasm skill invocation is allowed to touch on the
// host, parsed from C1's `permissions` block exactly as
// spec/c1-manifest.md documents it.
//
// Denial needs no code of its own here for either field — wazero's own
// default, with no FSConfig applied, is "no file access"; http_fetch (see
// http_import.go) is imported unconditionally but refuses every domain when
// HTTPDomains is empty. Permission{} *is* the fully-denied default for both,
// not a case this package has to implement specially. Only granting access
// takes code — see Invoke and http_import.go.
type Permission struct {
	FSPath      string
	FSWritable  bool
	HTTPDomains []string // exact hostnames only — no wildcard/suffix matching
}

// ParsePermissions reads C1's `permissions.filesystem` and
// `permissions.egress_http` fields. An unrecognised value fails closed —
// returned as an error, never silently treated as either "no access" or
// "full access" — because a permissions parser that guesses is the one
// place in a sandbox that must never guess generously.
func ParsePermissions(m map[string]any) (Permission, error) {
	fsPath, fsWritable, err := parseFilesystem(m["filesystem"])
	if err != nil {
		return Permission{}, err
	}
	domains, err := parseHTTPDomains(m["egress_http"])
	if err != nil {
		return Permission{}, err
	}
	return Permission{FSPath: fsPath, FSWritable: fsWritable, HTTPDomains: domains}, nil
}

func parseFilesystem(raw any) (path string, writable bool, err error) {
	if raw == nil {
		return "", false, nil
	}
	v, ok := raw.(string)
	if !ok {
		return "", false, fmt.Errorf("permissions.filesystem: want a string, got %T", raw)
	}
	switch {
	case v == "" || v == "none":
		return "", false, nil
	case strings.HasPrefix(v, "read:"):
		path := strings.TrimPrefix(v, "read:")
		if path == "" {
			return "", false, fmt.Errorf("permissions.filesystem: %q names no path", v)
		}
		return path, false, nil
	case strings.HasPrefix(v, "write:"):
		path := strings.TrimPrefix(v, "write:")
		if path == "" {
			return "", false, fmt.Errorf("permissions.filesystem: %q names no path", v)
		}
		return path, true, nil
	default:
		return "", false, fmt.Errorf(
			"permissions.filesystem: %q is not one of none|read:<path>|write:<path>", v)
	}
}

// parseHTTPDomains reads permissions.egress_http — a list of exact
// hostnames, per spec/c1-manifest.md ("allowed outbound HTTP domains"), not
// a boolean. Absent or an empty list both mean no domain is reachable;
// they are not distinguished, because there is no behavioral difference
// between "the author never considered egress_http" and "the author wrote
// egress_http: []" — both leave the skill with nothing granted.
func parseHTTPDomains(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("permissions.egress_http: want a list of domains, got %T", raw)
	}
	domains := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("permissions.egress_http: every entry must be a non-empty domain string, got %#v", v)
		}
		domains = append(domains, s)
	}
	return domains, nil
}
