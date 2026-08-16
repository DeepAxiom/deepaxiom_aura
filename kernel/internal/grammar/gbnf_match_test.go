package grammar

// A minimal GBNF evaluator, so the grammar tests assert on *behaviour* rather
// than on generated text.
//
// Without this, a test can only check that the compiler produced some string
// containing the right-looking tokens — which is exactly the kind of test that
// passes while the grammar permits `{, "b": 1}`. The whole claim of this
// package is "a model decoding under this grammar cannot emit something the
// port rejects", and that claim is only tested by actually running the grammar
// over inputs.
//
// It covers precisely the constructs Compile emits: rule references,
// alternation, sequence, grouping, `?`, `*`, quoted terminals and character
// classes. Anything else is a test failure, which keeps the evaluator honest
// about what it is verifying.

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// --- grammar AST -------------------------------------------------------------

type gnode interface{}

type gRef struct{ name string }
type gLit struct{ text string }
type gSeq struct{ items []gnode }
type gAlt struct{ items []gnode }
type gOpt struct{ item gnode }
type gStar struct{ item gnode }

type gClass struct {
	negated bool
	ranges  [][2]rune
}

// --- parsing -----------------------------------------------------------------

type gParser struct {
	src []rune
	i   int
}

func parseGBNF(t testingT, src string) map[string]gnode {
	rules := map[string]gnode{}
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, body, ok := strings.Cut(line, "::=")
		if !ok {
			t.Fatalf("grammar line is not a rule: %q", line)
		}
		p := &gParser{src: []rune(strings.TrimSpace(body))}
		node := p.alt(t)
		p.ws()
		if p.i != len(p.src) {
			t.Fatalf("rule %s: trailing input at %d: %q",
				strings.TrimSpace(name), p.i, string(p.src[p.i:]))
		}
		rules[strings.TrimSpace(name)] = node
	}
	return rules
}

func (p *gParser) ws() {
	for p.i < len(p.src) && (p.src[p.i] == ' ' || p.src[p.i] == '\t') {
		p.i++
	}
}

func (p *gParser) alt(t testingT) gnode {
	items := []gnode{p.seq(t)}
	for {
		p.ws()
		if p.i < len(p.src) && p.src[p.i] == '|' {
			p.i++
			items = append(items, p.seq(t))
			continue
		}
		break
	}
	if len(items) == 1 {
		return items[0]
	}
	return gAlt{items}
}

func (p *gParser) seq(t testingT) gnode {
	var items []gnode
	for {
		p.ws()
		if p.i >= len(p.src) || p.src[p.i] == '|' || p.src[p.i] == ')' {
			break
		}
		items = append(items, p.postfix(t))
	}
	if len(items) == 1 {
		return items[0]
	}
	return gSeq{items}
}

func (p *gParser) postfix(t testingT) gnode {
	node := p.atom(t)
	for p.i < len(p.src) {
		switch p.src[p.i] {
		case '?':
			p.i++
			node = gOpt{node}
		case '*':
			p.i++
			node = gStar{node}
		case '+':
			// One-or-more, desugared so the matcher needs one repetition rule
			// rather than two.
			p.i++
			node = gSeq{[]gnode{node, gStar{node}}}
		default:
			return node
		}
	}
	return node
}

func (p *gParser) atom(t testingT) gnode {
	p.ws()
	if p.i >= len(p.src) {
		t.Fatalf("unexpected end of rule body")
	}
	switch c := p.src[p.i]; {
	case c == '(':
		p.i++
		node := p.alt(t)
		p.ws()
		if p.i >= len(p.src) || p.src[p.i] != ')' {
			t.Fatalf("unclosed group at %d", p.i)
		}
		p.i++
		return node
	case c == '"':
		return gLit{p.quoted(t)}
	case c == '[':
		return p.class(t)
	default:
		start := p.i
		for p.i < len(p.src) && (isIdent(p.src[p.i])) {
			p.i++
		}
		if start == p.i {
			t.Fatalf("unexpected character %q at %d", string(c), p.i)
		}
		return gRef{string(p.src[start:p.i])}
	}
}

func isIdent(r rune) bool {
	return r == '-' || r == '_' ||
		(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// quoted reads a Go-style quoted terminal, which is what quoteGBNF emits.
func (p *gParser) quoted(t testingT) string {
	if p.src[p.i] != '"' {
		t.Fatalf("expected a quoted terminal at %d", p.i)
	}
	p.i++
	var b strings.Builder
	for p.i < len(p.src) {
		c := p.src[p.i]
		if c == '"' {
			p.i++
			return b.String()
		}
		if c == '\\' {
			p.i++
			if p.i >= len(p.src) {
				t.Fatalf("dangling escape")
			}
			b.WriteRune(unescape(p.src[p.i]))
			p.i++
			continue
		}
		b.WriteRune(c)
		p.i++
	}
	t.Fatalf("unterminated terminal")
	return ""
}

func unescape(r rune) rune {
	switch r {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	case 'r':
		return '\r'
	}
	return r
}

func (p *gParser) class(t testingT) gnode {
	p.i++ // consume [
	cl := gClass{}
	if p.i < len(p.src) && p.src[p.i] == '^' {
		cl.negated = true
		p.i++
	}
	for p.i < len(p.src) && p.src[p.i] != ']' {
		lo := p.classRune(t)
		if p.i+1 < len(p.src) && p.src[p.i] == '-' && p.src[p.i+1] != ']' {
			p.i++
			hi := p.classRune(t)
			cl.ranges = append(cl.ranges, [2]rune{lo, hi})
			continue
		}
		cl.ranges = append(cl.ranges, [2]rune{lo, lo})
	}
	if p.i >= len(p.src) {
		t.Fatalf("unterminated character class")
	}
	p.i++ // consume ]
	return cl
}

func (p *gParser) classRune(t testingT) rune {
	c := p.src[p.i]
	if c != '\\' {
		p.i++
		return c
	}
	p.i++
	if p.i >= len(p.src) {
		t.Fatalf("dangling escape in class")
	}
	esc := p.src[p.i]
	p.i++
	if esc == 'x' {
		// \xNN
		if p.i+1 >= len(p.src) {
			t.Fatalf("truncated \\x escape")
		}
		var v rune
		for k := 0; k < 2; k++ {
			v = v*16 + hexVal(p.src[p.i])
			p.i++
		}
		return v
	}
	return unescape(esc)
}

func hexVal(r rune) rune {
	switch {
	case r >= '0' && r <= '9':
		return r - '0'
	case r >= 'a' && r <= 'f':
		return r - 'a' + 10
	case r >= 'A' && r <= 'F':
		return r - 'A' + 10
	}
	return 0
}

// --- matching ----------------------------------------------------------------

// matcher walks a grammar over an input, returning every end position a node
// can consume to. Backtracking by construction, which is slow and completely
// adequate for the tiny documents these tests use.
type matcher struct {
	rules map[string]gnode
	in    string
	steps int
}

const maxSteps = 2_000_000

func (m *matcher) match(n gnode, pos int) []int {
	m.steps++
	if m.steps > maxSteps {
		return nil
	}
	switch v := n.(type) {
	case gRef:
		r, ok := m.rules[v.name]
		if !ok {
			return nil
		}
		return m.match(r, pos)

	case gLit:
		if strings.HasPrefix(m.in[pos:], v.text) {
			return []int{pos + len(v.text)}
		}
		return nil

	case gClass:
		if pos >= len(m.in) {
			return nil
		}
		r, size := utf8.DecodeRuneInString(m.in[pos:])
		inSet := false
		for _, rg := range v.ranges {
			if r >= rg[0] && r <= rg[1] {
				inSet = true
				break
			}
		}
		if inSet != v.negated {
			return []int{pos + size}
		}
		return nil

	case gSeq:
		ends := []int{pos}
		for _, item := range v.items {
			var next []int
			seen := map[int]bool{}
			for _, e := range ends {
				for _, e2 := range m.match(item, e) {
					if !seen[e2] {
						seen[e2] = true
						next = append(next, e2)
					}
				}
			}
			if len(next) == 0 {
				return nil
			}
			ends = next
		}
		return ends

	case gAlt:
		var out []int
		seen := map[int]bool{}
		for _, item := range v.items {
			for _, e := range m.match(item, pos) {
				if !seen[e] {
					seen[e] = true
					out = append(out, e)
				}
			}
		}
		return out

	case gOpt:
		out := []int{pos}
		seen := map[int]bool{pos: true}
		for _, e := range m.match(v.item, pos) {
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
		return out

	case gStar:
		out := []int{pos}
		seen := map[int]bool{pos: true}
		frontier := []int{pos}
		for len(frontier) > 0 {
			var next []int
			for _, p := range frontier {
				for _, e := range m.match(v.item, p) {
					if e == p || seen[e] {
						continue // zero-width: stop, or already explored
					}
					seen[e] = true
					out = append(out, e)
					next = append(next, e)
				}
			}
			frontier = next
		}
		return out
	}
	return nil
}

// accepts reports whether the grammar's `root` rule matches the whole input.
func accepts(t testingT, gbnf, input string) bool {
	rules := parseGBNF(t, gbnf)
	root, ok := rules["root"]
	if !ok {
		t.Fatalf("grammar has no root rule")
	}
	m := &matcher{rules: rules, in: input}
	for _, end := range m.match(root, 0) {
		if end == len(input) {
			return true
		}
	}
	if m.steps > maxSteps {
		t.Fatalf("matcher exceeded %d steps on %q — the grammar is pathological", maxSteps, input)
	}
	return false
}

// testingT is the slice of *testing.T this evaluator needs, so the helpers
// above read as ordinary code rather than as test scaffolding.
type testingT interface {
	Fatalf(format string, args ...any)
}

var _ = fmt.Sprintf
