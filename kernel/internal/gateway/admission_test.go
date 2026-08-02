package gateway

import (
	"strings"
	"testing"
)

func TestParseMemory(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false}, // declaring nothing reserves nothing
		{"1024", 1024, false},
		{"512B", 512, false},
		{"1Ki", 1 << 10, false},
		{"512Mi", 512 << 20, false},
		{"4Gi", 4 << 30, false},
		{"1Ti", 1 << 40, false},
		{"1.5Gi", int64(1.5 * (1 << 30)), false},
		{"1M", 1e6, false}, // decimal units differ from binary ones
		{"1Mi", 1 << 20, false},
		{"512MiB", 512 << 20, false},
		{" 256Mi ", 256 << 20, false},
		{"lots", 0, true},
		{"512Xi", 0, true},
		{"-1Gi", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseMemory(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error for %q, got %d", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMemory(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseMemory(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512B"},
		{1 << 10, "1Ki"},
		{512 << 20, "512Mi"},
		{1 << 30, "1.0Gi"},
	}
	for _, tc := range cases {
		if got := FormatBytes(tc.in); got != tc.want {
			t.Fatalf("FormatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A budget of zero means unlimited: the default node must not start
// refusing skills just because it was given no --memory-budget.
func TestAdmitIsUnlimitedWithoutABudget(t *testing.T) {
	a := NewAdmission(0)
	for i := 0; i < 10; i++ {
		if err := a.Admit("conn", "acme/x/y", "4Gi"); err != nil {
			t.Fatalf("unlimited budget must admit everything, got %v", err)
		}
	}
}

// The scenario from the README: a 1.5Gi node admits one 1Gi skill and
// refuses the second with an explanation instead of an OOM kill.
func TestAdmitRefusesWhatDoesNotFitAndExplainsWhy(t *testing.T) {
	a := NewAdmission(1536 << 20) // 1.5Gi

	if err := a.Admit("c1", "acme/first/skill", "1Gi"); err != nil {
		t.Fatalf("first 1Gi skill should fit: %v", err)
	}

	err := a.Admit("c2", "acme/second/skill", "1Gi")
	if err == nil {
		t.Fatal("want the second 1Gi skill refused against a 1.5Gi budget")
	}
	for _, want := range []string{"acme/second/skill", "1.0Gi", "512Mi", "1.5Gi"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal should say %q so the operator can act on it; got: %v", want, err)
		}
	}
}

// The reservation is tied to the connection, so a disconnect gives the
// capacity back — otherwise a node leaks budget until it refuses everything.
func TestReleaseFreesTheReservation(t *testing.T) {
	a := NewAdmission(1 << 30) // 1Gi

	if err := a.Admit("c1", "acme/first/skill", "1Gi"); err != nil {
		t.Fatalf("first skill should fit: %v", err)
	}
	if err := a.Admit("c2", "acme/second/skill", "1Gi"); err == nil {
		t.Fatal("budget is full; the second skill must be refused")
	}

	a.Release("c1")

	if _, used := a.Snapshot(); used != 0 {
		t.Fatalf("want 0 bytes used after release, got %d", used)
	}
	if err := a.Admit("c2", "acme/second/skill", "1Gi"); err != nil {
		t.Fatalf("released capacity should be reusable: %v", err)
	}
}

func TestAdmitRejectsAnUnparseableDeclaration(t *testing.T) {
	a := NewAdmission(1 << 30)
	if err := a.Admit("c1", "acme/x/y", "a big one"); err == nil {
		t.Fatal("want an error for an unparseable requirements.memory")
	}
}

func TestSnapshotReportsBudgetAndUse(t *testing.T) {
	a := NewAdmission(2 << 30)
	if err := a.Admit("c1", "acme/x/y", "512Mi"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	budget, used := a.Snapshot()
	if budget != 2<<30 {
		t.Fatalf("budget = %d, want %d", budget, int64(2<<30))
	}
	if used != 512<<20 {
		t.Fatalf("used = %d, want %d", used, int64(512<<20))
	}
}
