package eligibility

import (
	"strings"
	"testing"
)

func TestExplainReportsPlacementFirst(t *testing.T) {
	// A placed student who would also fail every other rule is told they are placed.
	r := Explain(true, false, false, false, 5, 8)
	if r == nil || r.Code != "already_placed" {
		t.Fatalf("got %v, want already_placed", r)
	}
	for _, c := range []struct {
		cgpa, branch, backlogs bool
		want                   string
	}{
		{false, true, true, "cgpa_too_low"},
		{true, false, true, "branch_not_eligible"},
		{true, true, false, "backlogs_not_allowed"},
		{false, false, false, "cgpa_too_low"},
	} {
		if r := Explain(false, c.cgpa, c.branch, c.backlogs, 7.1, 7.5); r == nil || r.Code != c.want {
			t.Errorf("Explain(%v) = %v, want %s", c, r, c.want)
		}
	}
	if r := Explain(false, true, true, true, 8, 7); r != nil {
		t.Errorf("eligible student got a reason: %v", r)
	}
	if r := Explain(false, false, true, true, 7.123, 7.5); !strings.Contains(r.Message, "7.12") || !strings.Contains(r.Message, "7.50") {
		t.Errorf("message should quote both CGPAs: %q", r.Message)
	}
}

func TestEligibleExcludesPlacedStudents(t *testing.T) {
	sql := Eligible("s", "$1", "$2", "$3")
	for _, must := range []string{"s.cgpa >= $1", "cardinality($2::text[])", "s.backlogs = 0 OR $3::boolean", "NOT EXISTS", "'Accepted'"} {
		if !strings.Contains(sql, must) {
			t.Errorf("Eligible() is missing %q:\n%s", must, sql)
		}
	}
	// The status expression reports Placed before In process.
	st := StatusExpr("s")
	if strings.Index(st, "'Placed'") > strings.Index(st, "'In process'") {
		t.Errorf("Placed must take precedence:\n%s", st)
	}
}
