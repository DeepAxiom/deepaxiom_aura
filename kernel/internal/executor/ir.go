// Package executor implements kernel primitive P4 — the graph IR executor.
package executor

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"aura/kernel/internal/spec"
)

// IRMajor is the C2 format major this executor understands.
const IRMajor = "1"

// ClientRef is the pseudo-node representing the external client (C2).
const ClientRef = "client"

// GateHumanApproval holds the message until a human confirms it.
const GateHumanApproval = "human-approval"

// GateNone is an explicit statement that an edge into a motor skill needs no
// approval. It exists because the motor-gate invariant is aimed at *omission*
// — a graph reaching a skill that acts on the world without the author ever
// considering it. Writing this down is the opposite of that: a deliberate,
// greppable, reviewable decision.
//
// Some motor skills are unusable otherwise. Speech synthesis is `motor`
// because it produces an effect in the world, and a voice assistant that
// asked permission before every clause would not be one.
const GateNone = "none"

// TypeMotor is the skill type that acts on the world. Edges delivering into
// one always carry a human-approval gate — see Session.applyMotorGate.
const TypeMotor = spec.EffectType

// Graph is contract C2.
type Graph struct {
	IR      string `json:"ir"`
	GraphID string `json:"graph_id"`
	Origin  struct {
		Kind  string `json:"kind"`
		Skill string `json:"skill,omitempty"`
		Cause string `json:"cause,omitempty"`
	} `json:"origin"`
	Nodes []Node     `json:"nodes"`
	Edges []Edge     `json:"edges"`
	Waves [][]string `json:"waves,omitempty"`
}

type Node struct {
	Ref         string         `json:"ref"`
	Resolve     string         `json:"resolve,omitempty"`
	Use         string         `json:"use,omitempty"`
	Constraints map[string]any `json:"constraints,omitempty"`
}

type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Gate string `json:"gate,omitempty"`
	QoS  string `json:"qos,omitempty"`
}

var endpointRe = regexp.MustCompile(`^[a-z0-9_-]+\.[a-z0-9_]+$`)

// ParseGraph decodes and validates an IR document (C2 rule 1, static part).
func ParseGraph(raw []byte) (*Graph, error) {
	var g Graph
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, fmt.Errorf("invalid IR json: %w", err)
	}
	if g.IR != IRMajor {
		return nil, fmt.Errorf("unsupported IR major %q (executor speaks %s)", g.IR, IRMajor)
	}
	if g.GraphID == "" {
		return nil, fmt.Errorf("missing graph_id")
	}
	if g.Origin.Kind != "declared" && g.Origin.Kind != "planner" {
		return nil, fmt.Errorf("origin.kind must be declared|planner")
	}
	if len(g.Nodes) == 0 || len(g.Edges) == 0 {
		return nil, fmt.Errorf("graph needs at least one node and one edge")
	}
	refs := map[string]bool{ClientRef: true}
	for _, n := range g.Nodes {
		if n.Ref == "" || n.Ref == ClientRef {
			return nil, fmt.Errorf("invalid node ref %q", n.Ref)
		}
		if refs[n.Ref] {
			return nil, fmt.Errorf("duplicate node ref %q", n.Ref)
		}
		if n.Resolve == "" && n.Use == "" {
			return nil, fmt.Errorf("node %q declares neither resolve nor use", n.Ref)
		}
		refs[n.Ref] = true
	}
	for _, e := range g.Edges {
		if !endpointRe.MatchString(e.From) || !endpointRe.MatchString(e.To) {
			return nil, fmt.Errorf("invalid edge endpoint %q -> %q", e.From, e.To)
		}
		for _, ep := range []string{e.From, e.To} {
			ref := strings.SplitN(ep, ".", 2)[0]
			if !refs[ref] {
				return nil, fmt.Errorf("edge references unknown node %q", ref)
			}
		}
		if e.Gate != "" && e.Gate != GateHumanApproval && e.Gate != GateNone {
			return nil, fmt.Errorf("unknown gate %q", e.Gate)
		}
	}
	return &g, nil
}
