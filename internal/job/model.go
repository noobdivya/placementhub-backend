// Package job covers postings: the company's draft/submit workflow, admin
// approval, the student-facing eligible listing, and deadline handling.
package job

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"placementhub/internal/domain"
	"placementhub/internal/eligibility"
	"placementhub/internal/httpx"

	"github.com/google/uuid"
)

// Job matches the frontend's Job type, plus a few fields the forms need.
type Job struct {
	ID            uuid.UUID `json:"id"`
	CompanyID     uuid.UUID `json:"companyId"`
	Company       string    `json:"company"`
	Color         string    `json:"color"`
	Role          string    `json:"role"`
	Type          string    `json:"type"`
	Location      string    `json:"location"`
	CTC           float64   `json:"ctc"`
	MinCGPA       float64   `json:"minCgpa"`
	Branches      []string  `json:"branches"`
	Skills        []string  `json:"skills"`
	Deadline      string    `json:"deadline"` // YYYY-MM-DD
	Openings      int       `json:"openings"`
	Applicants    int       `json:"applicants"`
	Status        string    `json:"status"`
	Description   string    `json:"description"`
	AllowBacklogs bool      `json:"allowBacklogs"`
	RejectReason  string    `json:"rejectReason,omitempty"`
}

// StudentJob is a job as seen by a specific student.
type StudentJob struct {
	Job
	Eligible      bool                `json:"eligible"`
	Applied       bool                `json:"applied"`
	ApplicationID *uuid.UUID          `json:"applicationId"`
	CanApply      bool                `json:"canApply"`
	BlockReason   *eligibility.Reason `json:"blockReason"`
}

// Input is the create/update body.
type Input struct {
	Role          string   `json:"role"`
	Type          string   `json:"type"`
	Location      string   `json:"location"`
	CTC           float64  `json:"ctc"`
	MinCGPA       float64  `json:"minCgpa"`
	Branches      []string `json:"branches"`
	Skills        []string `json:"skills"`
	Deadline      string   `json:"deadline"`
	Openings      int      `json:"openings"`
	Description   string   `json:"description"`
	AllowBacklogs bool     `json:"allowBacklogs"`
}

const dateLayout = "2006-01-02"

var skillCleanRE = regexp.MustCompile(`\s+`)

// normalise trims and de-duplicates lists so equal inputs compare equal.
func (in *Input) normalise() {
	in.Role = strings.TrimSpace(in.Role)
	in.Location = strings.TrimSpace(in.Location)
	in.Description = strings.TrimSpace(in.Description)
	in.Branches = uniq(in.Branches, false)
	in.Skills = uniq(in.Skills, true)
	slices.SortStableFunc(in.Branches, func(a, b string) int {
		return slices.Index(domain.Branches, a) - slices.Index(domain.Branches, b)
	})
}

func uniq(list []string, collapse bool) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range list {
		s = strings.TrimSpace(s)
		if collapse {
			s = skillCleanRE.ReplaceAllString(s, " ")
		}
		k := strings.ToLower(s)
		if s == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

// validate checks the input. today is the current date in the college timezone;
// the deadline may not be earlier than it.
func (in *Input) validate(today time.Time) error {
	var v httpx.V
	v.Text("role", in.Role, 2, 120)
	v.OneOf("type", in.Type, domain.JobTypes...)
	v.Text("location", in.Location, 1, 120)
	v.Range("ctc", in.CTC, 0, 1000)
	v.Range("minCgpa", in.MinCGPA, 0, 10)
	v.Range("openings", float64(in.Openings), 1, 10000)
	v.Text("description", in.Description, 10, 5000)
	if len(in.Branches) == 0 {
		v.Add("branches", "select at least one branch")
	}
	for _, b := range in.Branches {
		if !domain.ValidBranch(b) {
			v.Add("branches", "contains an unknown branch: "+b)
			break
		}
	}
	if len(in.Skills) > 20 {
		v.Add("skills", "at most 20 skills")
	}
	for _, s := range in.Skills {
		if len([]rune(s)) > 40 {
			v.Add("skills", "each skill must be at most 40 characters")
			break
		}
	}
	d, err := time.Parse(dateLayout, in.Deadline)
	switch {
	case err != nil:
		v.Add("deadline", "must be a date in YYYY-MM-DD format")
	case d.Before(today):
		v.Add("deadline", "cannot be in the past")
	}
	return v.Err()
}

// DeadlineEnd is the instant applications close: the end of the deadline day
// in the college timezone.
func DeadlineEnd(deadline string, loc *time.Location) (time.Time, error) {
	d, err := time.ParseInLocation(dateLayout, deadline, loc)
	if err != nil {
		return time.Time{}, err
	}
	return d.AddDate(0, 0, 1), nil
}

// Today returns the current date, at midnight UTC, in the college timezone.
func Today(now time.Time, loc *time.Location) time.Time {
	y, m, d := now.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
