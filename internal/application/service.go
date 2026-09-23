// Package application covers applying to jobs, the company's stage pipeline,
// and offers — including the rule that a student who accepts an offer is
// PLACED and can no longer apply to jobs or drives.
package application

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"placementhub/internal/config"
	"placementhub/internal/db"
	"placementhub/internal/domain"
	"placementhub/internal/eligibility"
	"placementhub/internal/email"
	"placementhub/internal/httpx"
	"placementhub/internal/job"
	"placementhub/internal/notify"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool          *pgxpool.Pool
	notifier      *notify.Notifier
	loc           *time.Location
	offerValidity time.Duration
	Now           func() time.Time
}

func NewService(pool *pgxpool.Pool, n *notify.Notifier, cfg config.Config) *Service {
	return &Service{pool: pool, notifier: n, loc: cfg.Location, offerValidity: cfg.OfferValidity, Now: time.Now}
}

// errPlaced is returned whenever a placed student attempts something they may no longer do.
func errPlaced() *httpx.Error {
	return httpx.NewError(403, "already_placed",
		"You have accepted a placement offer, so you cannot apply to further jobs or drives.")
}

// lockStudent serialises every operation that can change or depend on a
// student's placement (apply, accept, offer creation, drive registration).
func lockStudent(ctx context.Context, tx pgx.Tx, studentID uuid.UUID) error {
	var id uuid.UUID
	return tx.QueryRow(ctx, `SELECT id FROM students WHERE id = $1 FOR UPDATE`, studentID).Scan(&id)
}

func isPlaced(ctx context.Context, q db.DBTX, studentID uuid.UUID) (bool, error) {
	var placed bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM offers WHERE student_id = $1 AND status = 'Accepted')`, studentID).Scan(&placed)
	return placed, err
}

func addEvent(ctx context.Context, tx pgx.Tx, appID uuid.UUID, from *string, to string, actor *uuid.UUID, note string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx,
		`INSERT INTO application_events (application_id, from_stage, to_stage, actor_user_id, note)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`, appID, from, to, actor, note).Scan(&id)
	return id, err
}

// ---- views ----------------------------------------------------------------

type OfferInfo struct {
	ID         uuid.UUID `json:"id"`
	Status     string    `json:"status"`
	CTC        float64   `json:"ctc"`
	ValidUntil time.Time `json:"validUntil"`
}

// Application matches the frontend's Application type.
type Application struct {
	ID        uuid.UUID  `json:"id"`
	JobID     uuid.UUID  `json:"jobId"`
	Company   string     `json:"company"`
	Color     string     `json:"color"`
	Role      string     `json:"role"`
	AppliedOn string     `json:"appliedOn"`
	Stage     string     `json:"stage"`
	Note      string     `json:"note"`
	UpdatedAt time.Time  `json:"updatedAt"`
	Offer     *OfferInfo `json:"offer"`
}

const appSelect = `
SELECT a.id, a.job_id, c.name, c.color, j.role, to_char(a.applied_at AT TIME ZONE $1, 'YYYY-MM-DD'), a.stage, a.note, a.updated_at,
       o.id, o.status, o.ctc, o.valid_until
  FROM applications a
  JOIN jobs j ON j.id = a.job_id
  JOIN companies c ON c.id = j.company_id
  LEFT JOIN LATERAL (SELECT id, status, ctc, valid_until FROM offers
                      WHERE application_id = a.id ORDER BY created_at DESC LIMIT 1) o ON true`

func scanApp(row pgx.Row) (*Application, error) {
	var (
		a   Application
		oid *uuid.UUID
		os  *string
		oc  *float64
		ov  *time.Time
	)
	if err := row.Scan(&a.ID, &a.JobID, &a.Company, &a.Color, &a.Role, &a.AppliedOn, &a.Stage, &a.Note, &a.UpdatedAt,
		&oid, &os, &oc, &ov); err != nil {
		return nil, err
	}
	if oid != nil {
		a.Offer = &OfferInfo{ID: *oid, Status: *os, CTC: *oc, ValidUntil: *ov}
	}
	return &a, nil
}

// ListForStudent returns every application a student has ever made, newest
// first. Withdrawn and rejected ones stay visible, so history is never lost.
func (s *Service) ListForStudent(ctx context.Context, studentID uuid.UUID) ([]Application, error) {
	rows, err := s.pool.Query(ctx, appSelect+` WHERE a.student_id = $2 ORDER BY a.applied_at DESC, a.id`, s.loc.String(), studentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Application{}
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *Service) studentIDByUser(ctx context.Context, q db.DBTX, userID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM students WHERE user_id = $1`, userID).Scan(&id)
	if db.IsNoRows(err) {
		return uuid.Nil, httpx.NotFound("student profile")
	}
	return id, err
}

func (s *Service) ListMine(ctx context.Context, userID uuid.UUID) ([]Application, error) {
	sid, err := s.studentIDByUser(ctx, s.pool, userID)
	if err != nil {
		return nil, err
	}
	return s.ListForStudent(ctx, sid)
}

func (s *Service) getApp(ctx context.Context, id uuid.UUID) (*Application, error) {
	a, err := scanApp(s.pool.QueryRow(ctx, appSelect+` WHERE a.id = $2`, s.loc.String(), id))
	if db.IsNoRows(err) {
		return nil, httpx.NotFound("application")
	}
	return a, err
}

// ---- apply ----------------------------------------------------------------

// Apply submits an application. Every rule is enforced here, in one
// transaction, under a lock on the student row:
//
//	placed students cannot apply; the job must be open and before its deadline;
//	the student must meet the criteria and have a resume; one application per job.
func (s *Service) Apply(ctx context.Context, userID, jobID uuid.UUID) (*Application, error) {
	var appID uuid.UUID
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var sid uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM students WHERE user_id = $1 FOR UPDATE`, userID).Scan(&sid); err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("student profile")
			}
			return err
		}

		var (
			placed, cgpaOK, branchOK, backlogsOK, hasResume bool
			cgpa, minCgpa                                   float64
			status, deadline                                string
		)
		err := tx.QueryRow(ctx, `
SELECT `+eligibility.Placed("s")+`,
       `+eligibility.CGPAOK("s", "j.min_cgpa")+`,
       `+eligibility.BranchOK("s", "j.branches")+`,
       `+eligibility.BacklogsOK("s", "j.allow_backlogs")+`,
       EXISTS (SELECT 1 FROM resumes r WHERE r.student_id = s.id),
       s.cgpa, j.min_cgpa, j.status, to_char(j.deadline, 'YYYY-MM-DD')
  FROM students s, jobs j
 WHERE s.id = $1 AND j.id = $2`, sid, jobID).
			Scan(&placed, &cgpaOK, &branchOK, &backlogsOK, &hasResume, &cgpa, &minCgpa, &status, &deadline)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("job")
			}
			return err
		}
		// Placement is checked first: it overrides every other consideration.
		if placed {
			return errPlaced()
		}
		if status != domain.JobOpen {
			return httpx.Unprocessable("job_closed", "This job is not accepting applications.")
		}
		if end, err := job.DeadlineEnd(deadline, s.loc); err != nil || !s.Now().Before(end) {
			return httpx.Unprocessable("deadline_passed", "The application deadline has passed.")
		}
		if r := eligibility.Explain(false, cgpaOK, branchOK, backlogsOK, cgpa, minCgpa); r != nil {
			return httpx.Unprocessable(r.Code, r.Message)
		}
		if !hasResume {
			return httpx.Unprocessable("resume_required", "Upload your resume before applying.")
		}

		if err := tx.QueryRow(ctx,
			`INSERT INTO applications (job_id, student_id) VALUES ($1, $2) RETURNING id`, jobID, sid).Scan(&appID); err != nil {
			return err
		}
		_, err = addEvent(ctx, tx, appID, nil, domain.StageApplied, &userID, "")
		return err
	})
	if err != nil {
		switch db.PgCode(err) {
		case db.CodeUniqueViolation:
			return nil, httpx.Conflict("already_applied", "You have already applied to this job.")
		case db.CodeStudentPlaced: // the trigger backstop
			return nil, errPlaced()
		}
		return nil, err
	}
	return s.getApp(ctx, appID)
}

// Withdraw lets a student pull out of a live application. An offered
// application is answered by declining the offer instead.
func (s *Service) Withdraw(ctx context.Context, userID, appID uuid.UUID) (*Application, error) {
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		sid, err := s.studentIDByUser(ctx, tx, userID)
		if err != nil {
			return err
		}
		if err := lockStudent(ctx, tx, sid); err != nil {
			return err
		}
		var stage string
		err = tx.QueryRow(ctx, `SELECT stage FROM applications WHERE id = $1 AND student_id = $2 FOR UPDATE`, appID, sid).Scan(&stage)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("application")
			}
			return err
		}
		switch stage {
		case domain.StageApplied, domain.StageShortlisted, domain.StageInterview:
		case domain.StageOffered:
			return httpx.Conflict("has_offer", "This application has an offer. Accept or decline the offer instead.")
		default:
			return httpx.Conflict("bad_state", fmt.Sprintf("a %s application cannot be withdrawn", strings.ToLower(stage)))
		}
		if _, err := tx.Exec(ctx, `UPDATE applications SET stage = 'Withdrawn', updated_at = now() WHERE id = $1`, appID); err != nil {
			return err
		}
		_, err = addEvent(ctx, tx, appID, &stage, domain.StageWithdrawn, &userID, "Withdrawn by student")
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.getApp(ctx, appID)
}

// ---- company pipeline -----------------------------------------------------

// Candidate matches the frontend's Candidate type, plus what a recruiter needs.
type Candidate struct {
	ID        uuid.UUID  `json:"id"` // the application id
	StudentID uuid.UUID  `json:"studentId"`
	Name      string     `json:"name"`
	Branch    string     `json:"branch"`
	CGPA      float64    `json:"cgpa"`
	JobID     uuid.UUID  `json:"jobId"`
	Role      string     `json:"role"`
	Stage     string     `json:"stage"`
	Skills    []string   `json:"skills"`
	AppliedOn string     `json:"appliedOn"`
	Note      string     `json:"note"`
	HasResume bool       `json:"hasResume"`
	Offer     *OfferInfo `json:"offer"`
}

const candSelect = `
SELECT a.id, s.id, u.name, s.branch, s.cgpa, j.id, j.role, a.stage, s.skills,
       to_char(a.applied_at AT TIME ZONE $2, 'YYYY-MM-DD'), a.note,
       EXISTS (SELECT 1 FROM resumes r WHERE r.student_id = s.id),
       o.id, o.status, o.ctc, o.valid_until
  FROM applications a
  JOIN jobs j ON j.id = a.job_id
  JOIN companies c ON c.id = j.company_id
  JOIN students s ON s.id = a.student_id
  JOIN users u ON u.id = s.user_id
  LEFT JOIN LATERAL (SELECT id, status, ctc, valid_until FROM offers
                      WHERE application_id = a.id ORDER BY created_at DESC LIMIT 1) o ON true
 WHERE c.user_id = $1`

func scanCandidate(row pgx.Row) (*Candidate, error) {
	var (
		c   Candidate
		oid *uuid.UUID
		os  *string
		oc  *float64
		ov  *time.Time
	)
	if err := row.Scan(&c.ID, &c.StudentID, &c.Name, &c.Branch, &c.CGPA, &c.JobID, &c.Role, &c.Stage, &c.Skills,
		&c.AppliedOn, &c.Note, &c.HasResume, &oid, &os, &oc, &ov); err != nil {
		return nil, err
	}
	if c.Skills == nil {
		c.Skills = []string{}
	}
	if oid != nil {
		c.Offer = &OfferInfo{ID: *oid, Status: *os, CTC: *oc, ValidUntil: *ov}
	}
	return &c, nil
}

// ListCandidates returns applicants to the caller's jobs. Withdrawn
// applications are hidden unless includeWithdrawn is set.
func (s *Service) ListCandidates(ctx context.Context, userID uuid.UUID, jobID *uuid.UUID, stage string, includeWithdrawn bool) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx, candSelect+`
   AND ($3::uuid IS NULL OR j.id = $3)
   AND ($4 = '' OR a.stage = $4)
   AND ($5 OR a.stage <> 'Withdrawn')
 ORDER BY a.applied_at DESC, a.id`, userID, s.loc.String(), jobID, stage, includeWithdrawn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Candidate{}
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// StudentForCandidate resolves the student behind an application the caller
// owns, so a recruiter can download that one candidate's resume.
func (s *Service) StudentForCandidate(ctx context.Context, userID, appID uuid.UUID) (uuid.UUID, error) {
	var sid uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT a.student_id FROM applications a JOIN jobs j ON j.id = a.job_id JOIN companies c ON c.id = j.company_id
		  WHERE a.id = $1 AND c.user_id = $2`, appID, userID).Scan(&sid)
	if db.IsNoRows(err) {
		return uuid.Nil, httpx.NotFound("application")
	}
	return sid, err
}

// transitions lists the moves a recruiter may make. Forward moves go one step
// at a time; rejecting is allowed from any live stage; a candidate can be
// reset to Applied, mirroring the board's actions.
var transitions = map[string][]string{
	domain.StageApplied:     {domain.StageShortlisted, domain.StageRejected},
	domain.StageShortlisted: {domain.StageInterview, domain.StageRejected, domain.StageApplied},
	domain.StageInterview:   {domain.StageOffered, domain.StageRejected, domain.StageApplied},
	domain.StageOffered:     {domain.StageRejected, domain.StageApplied},
	domain.StageRejected:    {domain.StageApplied},
	domain.StageWithdrawn:   {},
}

// CanTransition reports whether a recruiter may move an application between stages.
func CanTransition(from, to string) bool { return slices.Contains(transitions[from], to) }

type StageChange struct {
	Stage      string     `json:"stage"`
	Note       string     `json:"note"`
	CTC        *float64   `json:"ctc"`        // Offered only; defaults to the job's CTC
	ValidUntil *time.Time `json:"validUntil"` // Offered only; defaults to now + offer validity
}

const maxOfferWindow = 90 * 24 * time.Hour

// MoveStage changes an application's stage on behalf of the recruiter who owns the job.
func (s *Service) MoveStage(ctx context.Context, userID, appID uuid.UUID, in StageChange) (*Candidate, error) {
	in.Note = strings.TrimSpace(in.Note)
	var v httpx.V
	v.OneOf("stage", in.Stage, domain.StageApplied, domain.StageShortlisted, domain.StageInterview, domain.StageOffered, domain.StageRejected)
	v.Text("note", in.Note, 0, 300)
	if in.CTC != nil {
		v.Range("ctc", *in.CTC, 0, 1000)
	}
	if err := v.Err(); err != nil {
		return nil, err
	}

	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// Find the application, verifying the caller owns its job.
		var (
			sid      uuid.UUID
			jobID    uuid.UUID
			role     string
			company  string
			jobCTC   float64
			stage    string
			note     string
			companyS string
		)
		err := tx.QueryRow(ctx,
			`SELECT a.student_id, j.id, j.role, c.name, j.ctc, a.stage, a.note, c.status
			   FROM applications a JOIN jobs j ON j.id = a.job_id JOIN companies c ON c.id = j.company_id
			  WHERE a.id = $1 AND c.user_id = $2`, appID, userID).
			Scan(&sid, &jobID, &role, &company, &jobCTC, &stage, &note, &companyS)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("application")
			}
			return err
		}
		// Lock order is always student → application to avoid deadlocks with Accept.
		if err := lockStudent(ctx, tx, sid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT stage, note FROM applications WHERE id = $1 FOR UPDATE`, appID).Scan(&stage, &note); err != nil {
			return err
		}

		if stage == domain.StageWithdrawn {
			return httpx.Conflict("application_withdrawn", "The student has withdrawn this application.")
		}
		var accepted bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM offers WHERE application_id = $1 AND status = 'Accepted')`, appID).Scan(&accepted); err != nil {
			return err
		}
		if accepted {
			return httpx.Conflict("offer_accepted", "The student has accepted this offer; the application can no longer be changed.")
		}
		if companyS != domain.CompanyApproved {
			return httpx.Forbidden("your company is not approved")
		}

		noteOnly := in.Stage == stage
		if noteOnly {
			if in.Note == "" || (stage != domain.StageShortlisted && stage != domain.StageInterview) {
				return httpx.Conflict("no_change", fmt.Sprintf("the application is already %s", stage))
			}
		} else if !CanTransition(stage, in.Stage) {
			return httpx.Unprocessable("invalid_transition", fmt.Sprintf("cannot move an application from %s to %s", stage, in.Stage))
		}

		newNote := note
		if in.Note != "" {
			newNote = in.Note
		}
		if in.Stage == domain.StageApplied && !noteOnly {
			newNote = in.Note // a reset starts clean
		}

		// Leaving Offered withdraws the offer that was still waiting.
		if stage == domain.StageOffered && !noteOnly {
			if _, err := tx.Exec(ctx,
				`UPDATE offers SET status = 'Rescinded', responded_at = now() WHERE application_id = $1 AND status = 'Pending'`, appID); err != nil {
				return err
			}
		}

		var offerID *uuid.UUID
		var offerCTC float64
		var validUntil time.Time
		if in.Stage == domain.StageOffered {
			placed, err := isPlaced(ctx, tx, sid)
			if err != nil {
				return err
			}
			if placed {
				return httpx.Conflict("student_placed", "This student has already accepted another offer.")
			}
			offerCTC = jobCTC
			if in.CTC != nil {
				offerCTC = *in.CTC
			}
			validUntil = s.Now().Add(s.offerValidity)
			if in.ValidUntil != nil {
				validUntil = *in.ValidUntil
			}
			if !validUntil.After(s.Now()) || validUntil.After(s.Now().Add(maxOfferWindow)) {
				return httpx.Unprocessable("bad_valid_until", "validUntil must be in the future and within 90 days")
			}
			var oid uuid.UUID
			if err := tx.QueryRow(ctx,
				`INSERT INTO offers (application_id, student_id, job_id, company_id, ctc, valid_until)
				 SELECT $1, $2, j.id, j.company_id, $3, $4 FROM jobs j WHERE j.id = $5 RETURNING id`,
				appID, sid, offerCTC, validUntil, jobID).Scan(&oid); err != nil {
				return err
			}
			offerID = &oid
		}

		if _, err := tx.Exec(ctx,
			`UPDATE applications SET stage = $2, note = $3, updated_at = now() WHERE id = $1`, appID, in.Stage, newNote); err != nil {
			return err
		}
		evID, err := addEvent(ctx, tx, appID, &stage, in.Stage, &userID, in.Note)
		if err != nil {
			return err
		}
		return s.notifyStage(ctx, tx, sid, appID, evID, in.Stage, company, role, in.Note, offerID, offerCTC, validUntil)
	})
	if err != nil {
		if db.PgCode(err) == db.CodeStudentPlaced {
			return nil, httpx.Conflict("student_placed", "This student has already accepted another offer.")
		}
		return nil, err
	}
	c, err := scanCandidate(s.pool.QueryRow(ctx, candSelect+` AND a.id = $3`, userID, s.loc.String(), appID))
	if err != nil {
		return nil, err
	}
	return c, nil
}

// notifyStage tells the student about a stage change, inside the same transaction.
func (s *Service) notifyStage(ctx context.Context, tx pgx.Tx, studentID, appID uuid.UUID, eventID int64,
	stage, company, role, note string, offerID *uuid.UUID, ctc float64, validUntil time.Time) error {

	spec := notify.Spec{
		Link:      "/students/applications",
		Data:      map[string]any{"applicationId": appID, "stage": stage},
		DedupeKey: fmt.Sprintf("app_stage:%d", eventID),
	}
	switch stage {
	case domain.StageShortlisted:
		spec.Type = domain.NotifStageUpdate
		spec.Title = fmt.Sprintf("Shortlisted by %s", company)
		spec.Body = fmt.Sprintf("You've been shortlisted for %s.", role)
		if note != "" {
			spec.Body += " " + note
		}
		spec.Email = &email.Content{CompanyName: company, JobRole: role, ApplicationStatus: "Shortlisted", Note: note}
	case domain.StageInterview:
		spec.Type = domain.NotifInterview
		spec.Title = fmt.Sprintf("Interview: %s at %s", role, company)
		spec.Body = "You've been moved to the interview stage."
		if note != "" {
			spec.Body = note
		}
		spec.Email = &email.Content{CompanyName: company, JobRole: role, ApplicationStatus: "Interview", Note: note}
	case domain.StageOffered:
		spec.Type = domain.NotifOffer
		spec.Title = fmt.Sprintf("Offer from %s", company)
		spec.Body = fmt.Sprintf("%s offered you %s at ₹%.1f LPA. Respond by %s.", company, role, ctc,
			validUntil.In(s.loc).Format("2 Jan 2006"))
		spec.Data["offerId"] = offerID
		spec.DedupeKey = "offer:" + offerID.String()
		spec.Email = &email.Content{CompanyName: company, JobRole: role, ApplicationStatus: "Offered",
			OfferCTC: ctc, OfferValidUntil: validUntil.In(s.loc).Format("2 Jan 2006")}
	case domain.StageRejected:
		spec.Type = domain.NotifStageUpdate
		spec.Title = fmt.Sprintf("Update from %s", company)
		spec.Body = fmt.Sprintf("%s will not be moving forward with your application for %s.", company, role)
		spec.Email = &email.Content{CompanyName: company, JobRole: role, ApplicationStatus: "Rejected"}
	default: // Applied (reset): nothing to announce
		return nil
	}
	uid, err := userIDOf(ctx, tx, studentID)
	if err != nil {
		return err
	}
	_, err = s.notifier.ToUser(ctx, tx, uid, spec)
	return err
}

func userIDOf(ctx context.Context, q db.DBTX, studentID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT user_id FROM students WHERE id = $1`, studentID).Scan(&id)
	return id, err
}

// ---- detail / history -----------------------------------------------------

type Event struct {
	From      *string   `json:"from"`
	To        string    `json:"to"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"createdAt"`
}

// History returns the stage history of an application the caller may see.
func (s *Service) History(ctx context.Context, appID uuid.UUID) ([]Event, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT from_stage, to_stage, note, created_at FROM application_events WHERE application_id = $1 ORDER BY id`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.From, &e.To, &e.Note, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CandidateProfile is the fuller view a recruiter gets of one applicant.
type CandidateProfile struct {
	Candidate
	Email    string   `json:"email"`
	Phone    string   `json:"phone"`
	Roll     string   `json:"roll"`
	Year     string   `json:"year"`
	Backlogs int      `json:"backlogs"`
	Tenth    *float64 `json:"tenth"`
	Twelfth  *float64 `json:"twelfth"`
	GitHub   string   `json:"github"`
	LinkedIn string   `json:"linkedin"`
	About    string   `json:"about"`
	History  []Event  `json:"history"`
}

func (s *Service) CandidateDetail(ctx context.Context, userID, appID uuid.UUID) (*CandidateProfile, error) {
	c, err := scanCandidate(s.pool.QueryRow(ctx, candSelect+` AND a.id = $3`, userID, s.loc.String(), appID))
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("application")
		}
		return nil, err
	}
	p := &CandidateProfile{Candidate: *c}
	err = s.pool.QueryRow(ctx,
		`SELECT u.email, s.phone, s.roll, s.year, s.backlogs, s.tenth, s.twelfth, s.github, s.linkedin, s.about
		   FROM students s JOIN users u ON u.id = s.user_id WHERE s.id = $1`, c.StudentID).
		Scan(&p.Email, &p.Phone, &p.Roll, &p.Year, &p.Backlogs, &p.Tenth, &p.Twelfth, &p.GitHub, &p.LinkedIn, &p.About)
	if err != nil {
		return nil, err
	}
	if p.History, err = s.History(ctx, appID); err != nil {
		return nil, err
	}
	return p, nil
}
