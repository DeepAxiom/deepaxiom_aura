package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// `aura generate connector` turns recorded traffic into a connector spec.
//
// The work is inferring which path segments are parameters. `/orders/1001` and
// `/orders/1002` are one operation, not two — and getting that wrong produces a
// connector with a skill per customer id, which is worse than useless.
//
// The output is written for review and never executed. Everything the
// generator inferred is a guess about someone else's system; a human confirms
// it before it can act, which is the same principle as writes starting
// disabled.

var (
	numericSeg = regexp.MustCompile(`^\d+$`)
	uuidSeg    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexSeg     = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)
)

// obviouslyVariable reports whether a segment is an identifier on sight. This
// is the first pass: it groups `/orders/1001` with `/orders/1002` even when
// each was seen only once, which is the common case in a short recording.
func obviouslyVariable(segment string) bool {
	return numericSeg.MatchString(segment) ||
		uuidSeg.MatchString(segment) ||
		hexSeg.MatchString(segment)
}

type opGroup struct {
	method    string
	segments  []string // with "{}" marking parameter positions
	distinct  []map[string]bool
	query     map[string]bool
	bodyShape map[string]string
	statuses  map[int]int
	calls     int
	authSeen  bool
}

func cmdGenerate(args []string) {
	if len(args) == 0 || args[0] != "connector" {
		fmt.Fprintln(os.Stderr, "usage: aura generate connector --from <recording> --name <name>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("generate connector", flag.ExitOnError)
	from := fs.String("from", "observed.jsonl", "recording produced by `aura observe`")
	name := fs.String("name", "", "connector name (required)")
	baseURL := fs.String("base-url", "", "the system's real base URL")
	out := fs.String("out", "", "output file (default: <name>-connector.yaml)")
	_ = fs.Parse(args[1:])

	if *name == "" {
		fmt.Fprintln(os.Stderr, "error: --name is required")
		os.Exit(2)
	}
	slug := strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(*name), "-"), "-")
	if *out == "" {
		*out = slug + "-connector.yaml"
	}

	observations, err := readObservations(*from)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if len(observations) == 0 {
		fmt.Fprintf(os.Stderr, "error: %s has no usable observations — "+
			"run `aura observe` and exercise the app first\n", *from)
		os.Exit(1)
	}

	groups := group(observations)
	yaml := render(slug, *baseURL, groups)

	if err := os.WriteFile(*out, []byte(yaml), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot write %s: %v\n", *out, err)
		os.Exit(1)
	}

	reads, writes := 0, 0
	for _, g := range groups {
		if isWrite(g.method) {
			writes++
		} else {
			reads++
		}
	}
	fmt.Printf(`wrote %s

  %d call(s) observed → %d operation(s): %d read, %d write
  reads start live · writes start disabled

Review it before using it — every operation below is inferred from traffic,
not declared by the system. Then:

  python skills/connector/main.py %s
`, *out, len(observations), len(groups), reads, writes, *out)
}

func readObservations(path string) ([]observation, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	defer file.Close()

	var out []observation
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var o observation
		if json.Unmarshal([]byte(line), &o) != nil || o.Method == "" || o.Path == "" {
			continue // a malformed line is not worth failing the whole recording
		}
		// Calls the system itself rejected say nothing about its shape.
		if o.Status >= 400 && o.Status != 404 {
			continue
		}
		out = append(out, o)
	}
	return out, scanner.Err()
}

// group folds observations into operations, parameterising only the path
// segments that are identifiers *on sight* — numeric, UUID, or long hex.
//
// It is tempting to also merge templates that differ in exactly one segment,
// which would catch slug ids like /users/alice and /users/bob. That rule
// cannot tell them apart from /orders and /invoices, and the two mistakes are
// not equally bad. Leaving a slug id unmerged produces visible duplicate
// operations a reviewer immediately spots and fixes. Merging two real
// resources produces a single plausible-looking operation that quietly points
// at the wrong endpoint. So this errs toward the mistake that is obvious, and
// the generated file tells the reviewer to look for the other one.
func group(observations []observation) []*opGroup {
	byKey := map[string]*opGroup{}

	for _, o := range observations {
		segments := splitPath(o.Path)
		masked := make([]string, len(segments))
		for i, s := range segments {
			if obviouslyVariable(s) {
				masked[i] = "{}"
			} else {
				masked[i] = s
			}
		}
		key := o.Method + " " + strings.Join(masked, "/")

		g := byKey[key]
		if g == nil {
			g = &opGroup{
				method: o.Method, segments: masked,
				distinct: make([]map[string]bool, len(segments)),
				query:    map[string]bool{}, statuses: map[int]int{},
			}
			for i := range g.distinct {
				g.distinct[i] = map[string]bool{}
			}
			byKey[key] = g
		}
		g.calls++
		g.statuses[o.Status]++
		g.authSeen = g.authSeen || o.AuthSeen
		for i, s := range segments {
			g.distinct[i][s] = true
		}
		for _, q := range o.Query {
			g.query[q] = true
		}
		if o.BodyShape != nil && g.bodyShape == nil {
			g.bodyShape = o.BodyShape
		}
	}

	groups := make([]*opGroup, 0, len(byKey))
	for _, g := range byKey {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if pathOf(a) != pathOf(b) {
			return pathOf(a) < pathOf(b)
		}
		return a.method < b.method
	})
	return groups
}

func splitPath(p string) []string {
	trimmed := strings.Trim(p, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// paramNames names each parameter after the segment before it, so a reviewer
// can tell what it is: /orders/{id}/lines/{id} would be unreadable.
func paramNames(segments []string) []string {
	var names []string
	used := map[string]int{}
	for i, s := range segments {
		if s != "{}" {
			continue
		}
		base := "id"
		if i > 0 && segments[i-1] != "{}" {
			base = strings.TrimSuffix(sanitize(segments[i-1]), "s") + "_id"
		}
		used[base]++
		if used[base] > 1 {
			base = fmt.Sprintf("%s%d", base, used[base])
		}
		names = append(names, base)
	}
	return names
}

func sanitize(s string) string {
	return strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(s), "_"), "_")
}

func pathOf(g *opGroup) string {
	if len(g.segments) == 0 {
		return "/"
	}
	names := paramNames(g.segments)
	next := 0
	parts := make([]string, len(g.segments))
	for i, s := range g.segments {
		if s == "{}" {
			parts[i] = "{" + names[next] + "}"
			next++
		} else {
			parts[i] = s
		}
	}
	return "/" + strings.Join(parts, "/")
}

func opIDOf(g *opGroup) string {
	var words []string
	for _, s := range g.segments {
		if s != "{}" {
			words = append(words, sanitize(s))
		}
	}
	if len(words) == 0 {
		words = []string{"root"}
	}
	verb := map[string]string{
		"GET": "get", "HEAD": "head", "POST": "create",
		"PUT": "replace", "PATCH": "update", "DELETE": "delete",
	}[g.method]
	if verb == "" {
		verb = strings.ToLower(g.method)
	}
	return strings.ReplaceAll(verb+"-"+strings.Join(words, "-"), "_", "-")
}

func isWrite(method string) bool {
	return method != "GET" && method != "HEAD"
}

func render(name, baseURL string, groups []*opGroup) string {
	var b strings.Builder
	b.WriteString("# Generated by `aura generate connector` from observed traffic.\n")
	b.WriteString("#\n")
	b.WriteString("# Everything here is INFERRED from real calls, not declared by the system.\n")
	b.WriteString("# Read it before using it: check the path templates, the operation names,\n")
	b.WriteString("# and especially which operations are writes.\n")
	b.WriteString("#\n")
	b.WriteString("# Reads are live immediately. Writes start disabled and stay that way until\n")
	b.WriteString("# you promote them deliberately.\n")
	b.WriteString("#\n")
	b.WriteString("# Known limitation: only numeric, UUID and long-hex path segments are turned\n")
	b.WriteString("# into parameters. An id that looks like a word (/users/alice) appears as a\n")
	b.WriteString("# separate operation per value — if you see near-duplicate operations below,\n")
	b.WriteString("# collapse them by hand into one with a {param}. Guessing that automatically\n")
	b.WriteString("# would risk merging two genuinely different resources instead.\n\n")

	fmt.Fprintf(&b, "name: %s\n", name)
	if baseURL == "" {
		b.WriteString("base_url: http://localhost:3000   # TODO: the system's real base URL\n")
	} else {
		fmt.Fprintf(&b, "base_url: %s\n", baseURL)
	}

	authSeen := false
	for _, g := range groups {
		authSeen = authSeen || g.authSeen
	}
	if authSeen {
		// The value was never recorded — only that the app sent one — so the
		// reviewer has to supply it, from the environment.
		b.WriteString("\nheaders:\n")
		b.WriteString("  # The observed traffic carried an auth header. Its value was not\n")
		b.WriteString("  # recorded; put the real one in an environment variable:\n")
		b.WriteString("  # Authorization: \"Bearer ${API_TOKEN}\"\n")
	}

	b.WriteString("\noperations:\n")
	for _, g := range groups {
		path := pathOf(g)
		names := paramNames(g.segments)

		fmt.Fprintf(&b, "  - op_id: %s\n", opIDOf(g))
		fmt.Fprintf(&b, "    method: %s\n", g.method)
		fmt.Fprintf(&b, "    path: %s\n", path)
		fmt.Fprintf(&b, "    summary: %q\n", summaryOf(g, path))
		if isWrite(g.method) {
			b.WriteString("    write: true\n")
		}
		if len(names) > 0 || len(g.query) > 0 {
			b.WriteString("    params:\n")
			for _, n := range names {
				fmt.Fprintf(&b, "      - { name: %s, in: path }\n", n)
			}
			for _, q := range sortedKeys(g.query) {
				fmt.Fprintf(&b, "      - { name: %s, in: query }\n", sanitize(q))
			}
		}
		if len(g.bodyShape) > 0 {
			b.WriteString("    # observed body keys:")
			for _, k := range sortedKeys(g.bodyShape) {
				fmt.Fprintf(&b, " %s(%s)", k, g.bodyShape[k])
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func summaryOf(g *opGroup, path string) string {
	action := map[string]string{
		"GET": "Fetch", "HEAD": "Check", "POST": "Create",
		"PUT": "Replace", "PATCH": "Update", "DELETE": "Delete",
	}[g.method]
	if action == "" {
		action = g.method
	}
	return fmt.Sprintf("%s %s (seen %d time(s) while observing)", action, path, g.calls)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
