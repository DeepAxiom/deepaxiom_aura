// Package media is the module root, and exists to hold one thing: the C1
// manifest, embedded.
//
// skill.yaml stays a file at the root of this module — where a reader looks for
// it, and where every other skill in this repository keeps its own — rather than
// being a Go string. It is parsed at start-up, so the ports this service serves
// and the ports it declares cannot drift apart.
package media

import _ "embed"

// SkillYAML is this service's C1 manifest, verbatim.
//
//go:embed skill.yaml
var SkillYAML []byte
