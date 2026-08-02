package gateway

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Admission implements the R15 resource budget: skills declare memory in
// their manifest (requirements.memory) and the node refuses to admit what
// does not fit — with an explained error, never a silent OOM.
type Admission struct {
	mu      sync.Mutex
	budget  int64 // bytes; 0 = unlimited
	used    int64
	perConn map[string]int64
}

func NewAdmission(budget int64) *Admission {
	return &Admission{budget: budget, perConn: map[string]int64{}}
}

// Admit reserves the declared memory for a connection, or explains why not.
func (a *Admission) Admit(connID, skillID, declared string) error {
	need, err := ParseMemory(declared)
	if err != nil {
		return fmt.Errorf("invalid requirements.memory %q: %w", declared, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.budget > 0 && a.used+need > a.budget {
		return fmt.Errorf(
			"admission rejected: %s declares %s but only %s of the %s budget remains "+
				"(free capacity by stopping a skill, or raise --memory-budget)",
			skillID, FormatBytes(need), FormatBytes(a.budget-a.used), FormatBytes(a.budget))
	}
	a.used += need
	a.perConn[connID] = need
	return nil
}

// Release frees a connection's reservation (on disconnect).
func (a *Admission) Release(connID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.used -= a.perConn[connID]
	delete(a.perConn, connID)
}

// Snapshot returns (budget, used) in bytes.
func (a *Admission) Snapshot() (int64, int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.budget, a.used
}

var memRe = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*(B|Ki|Mi|Gi|Ti|K|M|G|T)?B?$`)

// ParseMemory parses "512Mi", "4Gi", "1.5G", "1024" (bytes) into bytes.
// Empty declares nothing (0).
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	m := memRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("want forms like 512Mi, 4Gi, 1024")
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	mult := map[string]float64{
		"": 1, "B": 1,
		"K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12,
		"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40,
	}[m[2]]
	return int64(value * mult), nil
}

// FormatBytes renders bytes with binary units.
func FormatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKi", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
