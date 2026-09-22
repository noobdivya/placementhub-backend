// Package eligibility is the single definition of "which students may apply".
//
// The rule is expressed once, as SQL fragments over a students alias. The same
// fragments drive the jobs list ("eligible only"), the apply check, drive
// eligible counts and notification fan-out, so they cannot disagree.
//
// A student is eligible for criteria (minCgpa, branches, allowBacklogs) when:
//
//	cgpa >= minCgpa
//	AND (branches is empty OR branch IN branches)
//	AND (backlogs = 0 OR allowBacklogs)
//	AND the student is not PLACED (holds no Accepted offer)
package eligibility

import "fmt"

// Placed is true for a student with an accepted offer.
func Placed(alias string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM offers _po WHERE _po.student_id = %s.id AND _po.status = 'Accepted')`, alias)
}

// CGPAOK, BranchOK and BacklogsOK take SQL expressions (usually $n placeholders
// or job columns) for the criteria.
func CGPAOK(alias, minCgpa string) string {
	return fmt.Sprintf(`(%s.cgpa >= %s)`, alias, minCgpa)
}

func BranchOK(alias, branches string) string {
	return fmt.Sprintf(`(cardinality(%[2]s::text[]) = 0 OR %[1]s.branch = ANY(%[2]s::text[]))`, alias, branches)
}

func BacklogsOK(alias, allowBacklogs string) string {
	return fmt.Sprintf(`(%s.backlogs = 0 OR %s::boolean)`, alias, allowBacklogs)
}

// Meets is the criteria test without the placement check.
func Meets(alias, minCgpa, branches, allowBacklogs string) string {
	return fmt.Sprintf(`(%s AND %s AND %s)`,
		CGPAOK(alias, minCgpa), BranchOK(alias, branches), BacklogsOK(alias, allowBacklogs))
}

// Eligible is Meets AND NOT Placed: the full "may apply" predicate.
func Eligible(alias, minCgpa, branches, allowBacklogs string) string {
	return fmt.Sprintf(`(%s AND NOT %s)`, Meets(alias, minCgpa, branches, allowBacklogs), Placed(alias))
}

// Reason explains why a student cannot apply. Empty means eligible.
type Reason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Explain turns the component checks (evaluated by the SQL fragments above)
// into a specific, user-facing reason. Placement is reported first because it
// overrides everything else.
func Explain(placed, cgpaOK, branchOK, backlogsOK bool, cgpa, minCgpa float64) *Reason {
	switch {
	case placed:
		return &Reason{"already_placed", "You have accepted a placement offer, so you cannot apply to further jobs or drives."}
	case !cgpaOK:
		return &Reason{"cgpa_too_low", fmt.Sprintf("Your CGPA (%.2f) is below the required minimum of %.2f.", cgpa, minCgpa)}
	case !branchOK:
		return &Reason{"branch_not_eligible", "Your branch is not eligible for this role."}
	case !backlogsOK:
		return &Reason{"backlogs_not_allowed", "Students with active backlogs are not eligible for this role."}
	}
	return nil
}

// StatusExpr yields the student's placement status as a SQL expression:
// Placed (holds an accepted offer), In process (an application is live) or Unplaced.
func StatusExpr(alias string) string {
	return fmt.Sprintf(`(CASE
  WHEN %[2]s THEN 'Placed'
  WHEN EXISTS (SELECT 1 FROM applications _sa WHERE _sa.student_id = %[1]s.id
               AND _sa.stage IN ('Applied', 'Shortlisted', 'Interview', 'Offered')) THEN 'In process'
  ELSE 'Unplaced' END)`, alias, Placed(alias))
}
