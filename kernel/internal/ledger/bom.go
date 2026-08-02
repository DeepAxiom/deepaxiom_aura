package ledger

// ML-BOM generation — a CycloneDX bill of materials for what a session
// actually ran.
//
// Every AI-SBOM tool on the market builds its inventory from a manifest, a
// lockfile, or a deployment descriptor: a statement of what was *supposed* to
// be there. That is a genuinely useful thing, and it is a different thing from
// what this produces. A node's ledger records which models were cited by which
// effects, under which policy, at which revision — observed, not declared. A
// model that was configured but never invoked does not appear here; a model
// that was swapped in at runtime does.
//
// The EU AI Act's high-risk obligations (Articles 9-17 for providers, 26 for
// deployers) have applied since 2 August 2026, and the record-keeping they
// ask for is closer to this than to a lockfile: what the system did, with
// what, under whose authority. Emitting CycloneDX rather than a bespoke shape
// is the whole point — it drops into tooling that already exists.
//
// Scope, stated plainly for the same reason it is stated in attest.go: the
// model components below come from C5 attestations, which are assertions by
// the skills that made them. A BOM generated here inherits exactly that
// standing — it is an accurate record of what was claimed and what it caused,
// not an independent measurement of what executed.

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"aura/kernel/internal/store"
)

// CycloneDX 1.6 is the first version with a settled `machine-learning-model`
// component type and `modelCard`, which is what makes a model expressible as
// a first-class component rather than as a library with an odd name.
const (
	bomFormat      = "CycloneDX"
	bomSpecVersion = "1.6"
)

// BOM is the subset of CycloneDX this emits. Hand-rolled rather than pulled
// from a library because the surface used here is small and stable, and a
// dependency that exists to marshal twelve fields is a dependency that will
// one day need a CVE response.
type BOM struct {
	BOMFormat    string          `json:"bomFormat"`
	SpecVersion  string          `json:"specVersion"`
	SerialNumber string          `json:"serialNumber"`
	Version      int             `json:"version"`
	Metadata     BOMMetadata     `json:"metadata"`
	Components   []BOMComponent  `json:"components"`
	Dependencies []BOMDependency `json:"dependencies,omitempty"`
}

type BOMMetadata struct {
	Timestamp  string        `json:"timestamp"`
	Tools      []BOMTool     `json:"tools"`
	Component  *BOMComponent `json:"component,omitempty"`
	Properties []BOMProperty `json:"properties,omitempty"`
}

type BOMTool struct {
	Vendor  string `json:"vendor"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type BOMComponent struct {
	Type        string        `json:"type"`
	BOMRef      string        `json:"bom-ref"`
	Name        string        `json:"name"`
	Version     string        `json:"version,omitempty"`
	Description string        `json:"description,omitempty"`
	PURL        string        `json:"purl,omitempty"`
	Hashes      []BOMHash     `json:"hashes,omitempty"`
	ModelCard   *BOMModelCard `json:"modelCard,omitempty"`
	Properties  []BOMProperty `json:"properties,omitempty"`
}

type BOMHash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

// BOMModelCard carries what the attestation knew. CycloneDX's model card is
// far richer than this; only the fields a runtime can honestly fill are
// emitted, because a model card padded with unknowns is worse than a short
// one — it implies knowledge that was never captured.
type BOMModelCard struct {
	ModelParameters *BOMModelParameters `json:"modelParameters,omitempty"`
	Properties      []BOMProperty       `json:"properties,omitempty"`
}

type BOMModelParameters struct {
	Task              string `json:"task,omitempty"`
	ModelArchitecture string `json:"modelArchitecture,omitempty"`
}

type BOMProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type BOMDependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

// BuildBOM produces the bill of materials for one session, or for the whole
// ledger when session is empty.
//
// The node id is read out of the sealed entries themselves rather than from
// the identity on disk. That is deliberate: a data directory handed to an
// auditor is often a copy without the identity folder, and a BOM tool that
// minted or required a keypair to describe someone else's history would be
// both wrong and slightly alarming. Every entry names its sealing node, and
// the chain has already been verified by whoever cared to.
func BuildBOM(st *store.Store, session, version string) (BOM, error) {
	var raws []json.RawMessage
	var err error
	if session == "" {
		raws, err = st.LedgerEntries(1, 0)
	} else {
		raws, err = st.LedgerEntriesBySession(session)
	}
	if err != nil {
		return BOM{}, fmt.Errorf("read ledger: %w", err)
	}
	if len(raws) == 0 {
		scope := "this node's ledger"
		if session != "" {
			scope = "session " + session
		}
		return BOM{}, fmt.Errorf(
			"%s has no sealed effects — a bill of materials is built from what actually "+
				"ran, so there is nothing to report", scope)
	}

	nodeID := "unknown-node"
	var probe Entry
	if json.Unmarshal(raws[0], &probe) == nil && probe.Node != "" {
		nodeID = probe.Node
	}

	bom := BOM{
		BOMFormat: bomFormat, SpecVersion: bomSpecVersion, Version: 1,
		SerialNumber: "urn:uuid:" + deterministicUUID(nodeID, session, len(raws)),
		Metadata: BOMMetadata{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Tools:     []BOMTool{{Vendor: "Deep Axiom", Name: "aura", Version: version}},
			Component: &BOMComponent{
				Type: "application", BOMRef: "node:" + nodeID, Name: nodeID,
				Description: "Deep Axiom node",
			},
			Properties: []BOMProperty{
				{Name: "aura:scope", Value: bomScope(session)},
				{Name: "aura:effects_sealed", Value: fmt.Sprint(len(raws))},
				{Name: "aura:basis", Value: "observed from the C4 effect ledger, not declared configuration"},
			},
		},
	}

	// Collect distinct actors (skills that produced effects), models (from C5
	// attestations), and which effects cited which.
	actors := map[string]*BOMComponent{}
	models := map[string]*BOMComponent{}
	usedBy := map[string]map[string]bool{} // actor ref -> set of model refs
	policies := map[string]bool{}
	var totalMillijoules float64
	var energySources = map[string]bool{}

	for _, raw := range raws {
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		policies[e.Policy] = true

		actorRef := "skill:" + e.Actor
		if _, seen := actors[actorRef]; !seen {
			name, ver := splitActor(e.Actor)
			actors[actorRef] = &BOMComponent{
				Type: "application", BOMRef: actorRef, Name: name, Version: ver,
				Description: "skill that produced effects in this scope",
				PURL:        "pkg:aura/" + name + "@" + ver,
				Properties: []BOMProperty{
					{Name: "aura:skill_type", Value: "motor"},
					{Name: "aura:capability", Value: e.Capability},
				},
			}
			usedBy[actorRef] = map[string]bool{}
		}

		for _, hash := range e.Inference {
			a, err := LoadAttestation(st, hash)
			if err != nil {
				continue
			}
			modelRef := "model:" + hash
			if _, seen := models[modelRef]; !seen {
				models[modelRef] = modelComponent(modelRef, hash, a)
			}
			usedBy[actorRef][modelRef] = true
			if a.Energy != nil {
				totalMillijoules += a.Energy.Millijoules
				energySources[a.Energy.Source] = true
			}
		}
	}

	for _, c := range sortedComponents(actors) {
		bom.Components = append(bom.Components, *c)
	}
	for _, c := range sortedComponents(models) {
		bom.Components = append(bom.Components, *c)
	}

	for _, ref := range sortedKeys(usedBy) {
		deps := sortedSet(usedBy[ref])
		bom.Dependencies = append(bom.Dependencies, BOMDependency{Ref: ref, DependsOn: deps})
	}

	if len(policies) > 0 {
		bom.Metadata.Properties = append(bom.Metadata.Properties, BOMProperty{
			Name: "aura:policies_in_force", Value: joinSorted(policies),
		})
	}
	if totalMillijoules > 0 {
		bom.Metadata.Properties = append(bom.Metadata.Properties,
			BOMProperty{Name: "aura:energy_millijoules", Value: fmt.Sprintf("%.3f", totalMillijoules)},
			// The sources travel with the number, always. A joule total whose
			// provenance is unstated is indistinguishable from a guess, and
			// this figure is exactly the kind that ends up in a report.
			BOMProperty{Name: "aura:energy_sources", Value: joinSorted(energySources)},
		)
	}
	return bom, nil
}

func modelComponent(ref, attHash string, a Attestation) *BOMComponent {
	c := &BOMComponent{
		Type: "machine-learning-model", BOMRef: ref, Name: a.Model,
		Version:     a.ModelRevision,
		Description: fmt.Sprintf("invoked via %s", a.Engine),
		Properties: []BOMProperty{
			{Name: "aura:engine", Value: a.Engine},
			{Name: "aura:attestation", Value: attHash},
			// The standing of every claim in this component, inline, so it
			// travels even when the component is read out of context.
			{Name: "aura:evidence", Value: "asserted by the invoking skill; bound to sealed effects"},
		},
	}
	if a.EngineVersion != "" {
		c.Properties = append(c.Properties, BOMProperty{Name: "aura:engine_version", Value: a.EngineVersion})
	}
	if a.Quantization != "" {
		c.Properties = append(c.Properties, BOMProperty{Name: "aura:quantization", Value: a.Quantization})
	}
	if a.ModelFile != "" {
		c.Properties = append(c.Properties, BOMProperty{Name: "aura:model_file", Value: a.ModelFile})
	}
	// A Hub repo id plus a revision is resolvable, so emit a purl only when
	// both are present — a purl pointing at a moving target is worse than none.
	if a.ModelRevision != "" && looksLikeHubRepo(a.Model) {
		c.PURL = "pkg:huggingface/" + a.Model + "@" + a.ModelRevision
	}
	if a.ModelSHA256 != "" {
		c.Hashes = append(c.Hashes, BOMHash{Alg: "SHA-256", Content: a.ModelSHA256})
	}
	if len(a.Params) > 0 {
		c.ModelCard = &BOMModelCard{
			Properties: []BOMProperty{{Name: "aura:sampling_params", Value: string(a.Params)}},
		}
	}
	return c
}

// looksLikeHubRepo recognises the `org/name` shape a Hugging Face repo id has.
func looksLikeHubRepo(model string) bool {
	slashes := 0
	for _, c := range model {
		if c == '/' {
			slashes++
		}
	}
	return slashes == 1
}

func splitActor(actor string) (name, version string) {
	for i := len(actor) - 1; i >= 0; i-- {
		if actor[i] == '@' {
			return actor[:i], actor[i+1:]
		}
	}
	return actor, ""
}

func bomScope(session string) string {
	if session == "" {
		return "node (every sealed effect)"
	}
	return "session " + session
}

// deterministicUUID derives a stable urn from what the BOM covers, so
// regenerating the same BOM twice yields the same serial number. CycloneDX
// wants a UUID; it does not require randomness, and a random one would make
// two identical BOMs look like two different documents.
func deterministicUUID(nodeID, session string, n int) string {
	h := AttestationHash([]byte(fmt.Sprintf("aura-bom-v1:%s:%s:%d", nodeID, session, n)))
	x := h[len("sha256:"):]
	return fmt.Sprintf("%s-%s-%s-%s-%s", x[0:8], x[8:12], x[12:16], x[16:20], x[20:32])
}

func sortedComponents(m map[string]*BOMComponent) []*BOMComponent {
	out := make([]*BOMComponent, 0, len(m))
	for _, k := range sortedKeysC(m) {
		out = append(out, m[k])
	}
	return out
}

func sortedKeysC(m map[string]*BOMComponent) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinSorted(m map[string]bool) string {
	items := sortedSet(m)
	s := ""
	for i, it := range items {
		if i > 0 {
			s += ", "
		}
		s += it
	}
	return s
}
