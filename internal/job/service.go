package job

import (
	"context"
	"fmt"
	"strings"
	"time"

	"placementhub/internal/audit"
	"placementhub/internal/company"
	"placementhub/internal/config"
	"placementhub/internal/db"
	"placementhub/internal/domain"
	"placementhub/internal/eligibility"
	"placementhub/internal/httpx"
	"placementhub/internal/notify"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool     *pgxpool.Pool
	notifier *notify.Notifier
	loc      *time.Location
	Now      func() time.Time
}

func NewService(pool *pgxpool.Pool, n *notify.Notifier, cfg config.Config) *Service {
	return &Service{pool: pool, notifier: n, loc: cfg.Location, Now: time.Now}
}

func (s *Service) today() time.Time { return Today(s.Now(), s.loc) }

const jobCols = `
j.id, c.id, c.name, c.color, j.role, j.type, j.location, j.ctc, j.min_cgpa, j.branches, j.skills,
to_char(j.deadline, 'YYYY-MM-DD'), j.openings,
(SELECT count(*) FROM applications a WHERE a.job_id = j.id)::int,
j.status, j.description, j.allow_backlogs, j.reject_reason`

const jobFrom = ` FROM jobs j JOIN companies c ON c.id = j.company_id`

func scanJob(row pgx.Row, extra ...any) (*Job, error) {
	var j Job
	dest := append([]any{&j.ID, &j.CompanyID, &j.Company, &j.Color, &j.Role, &j.Type, &j.Location, &j.CTC,
		&j.MinCGPA, &j.Branches, &j.Skills, &j.Deadline, &j.Openings, &j.Applicants, &j.Status,
		&j.Description, &j.AllowBacklogs, &j.RejectReason}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	if j.Skills == nil {
		j.Skills = []string{}
	}
	return &j, nil
}

// ---- student view ---------------------------------------------------------

type ListFilter struct {
	Type         string
	Query        string
	EligibleOnly bool
	Sort         string // deadline | ctc
	Limit        int
	Offset       int
}

// studentCols adds the per-student eligibility flags, computed by the shared
// eligibility fragments so they match the apply check exactly.
func studentCols() string {
	return jobCols + `,
  ` + eligibility.Placed("me") + `,
  ` + eligibility.CGPAOK("me", "j.min_cgpa") + `,
  ` + eligibility.BranchOK("me", "j.branches") + `,
  ` + eligibility.BacklogsOK("me", "j.allow_backlogs") + `,
  me_app.id, me.cgpa`
}

const studentFrom = jobFrom + `
  JOIN students me ON me.user_id = $1
  LEFT JOIN applications me_app ON me_app.job_id = j.id AND me_app.student_id = me.id`

func scanStudentJob(row pgx.Row) (*StudentJob, error) {
	var (
		placed, cgpaOK, branchOK, backlogsOK bool
		appID                                *uuid.UUID
		cgpa                                 float64
	)
	j, err := scanJob(row, &placed, &cgpaOK, &branchOK, &backlogsOK, &appID, &cgpa)
	if err != nil {
		return nil, err
	}
	sj := &StudentJob{Job: *j, Applied: appID != nil, ApplicationID: appID}
	reason := eligibility.Explain(placed, cgpaOK, branchOK, backlogsOK, cgpa, j.MinCGPA)
	sj.Eligible = reason == nil
	switch {
	case reason != nil:
		sj.BlockReason = reason
	case sj.Applied:
		sj.BlockReason = &eligibility.Reason{Code: "already_applied", Message: "You have already applied to this job."}
	case j.Status != domain.JobOpen:
		sj.BlockReason = &eligibility.Reason{Code: "job_closed", Message: "This job is no longer accepting applications."}
	}
	sj.CanApply = sj.BlockReason == nil
	return sj, nil
}

// ListForStudent returns open jobs whose deadline has not passed, with the
// student's eligibility and application state per job.
func (s *Service) ListForStudent(ctx context.Context, userID uuid.UUID, f ListFilter) ([]StudentJob, int, error) {
	pat := ""
	if q := strings.TrimSpace(f.Query); q != "" {
		pat = "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	}
	where := ` WHERE j.status = 'Open' AND j.deadline >= $2::date
	  AND ($3 = '' OR j.type = $3)
	  AND ($4 = '' OR (j.role || ' ' || c.name || ' ' || j.location || ' ' || array_to_string(j.skills, ' ')) ILIKE $4)
	  AND (NOT $5 OR ` + eligibility.Eligible("me", "j.min_cgpa", "j.branches", "j.allow_backlogs") + `)`
	args := []any{userID, s.today().Format(dateLayout), f.Type, pat, f.EligibleOnly}

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*)`+studentFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "j.deadline, j.id"
	if f.Sort == "ctc" {
		order = "j.ctc DESC, j.deadline, j.id"
	}
	rows, err := s.pool.Query(ctx, `SELECT `+studentCols()+studentFrom+where+
		fmt.Sprintf(` ORDER BY %s LIMIT $6 OFFSET $7`, order), append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []StudentJob{}
	for rows.Next() {
		sj, err := scanStudentJob(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *sj)
	}
	return out, total, rows.Err()
}

// GetForStudent returns one job. Jobs that are not open stay visible to
// students who already applied, so their history still resolves.
func (s *Service) GetForStudent(ctx context.Context, userID, id uuid.UUID) (*StudentJob, error) {
	sj, err := scanStudentJob(s.pool.QueryRow(ctx, `SELECT `+studentCols()+studentFrom+
		` WHERE j.id = $2 AND (j.status = 'Open' OR me_app.id IS NOT NULL)`, userID, id))
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("job")
		}
		return nil, err
	}
	if sj.Status == domain.JobOpen {
		if end, err := DeadlineEnd(sj.Deadline, s.loc); err == nil && !s.Now().Before(end) && sj.BlockReason == nil {
			sj.CanApply = false
			sj.BlockReason = &eligibility.Reason{Code: "deadline_passed", Message: "The application deadline has passed."}
		}
	}
	return sj, nil
}

// ---- company workflow -----------------------------------------------------

func (s *Service) ListForCompany(ctx context.Context, userID uuid.UUID) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+jobCols+jobFrom+
		` WHERE c.user_id = $1 ORDER BY j.created_at DESC, j.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func (s *Service) getOwned(ctx context.Context, q db.DBTX, userID, id uuid.UUID, forUpdate bool) (*Job, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE OF j"
	}
	j, err := scanJob(q.QueryRow(ctx, `SELECT `+jobCols+jobFrom+` WHERE j.id = $1 AND c.user_id = $2`+lock, id, userID))
	if err != nil {
		if db.IsNoRows(err) { // also covers "someone else's job": never reveal it exists
			return nil, httpx.NotFound("job")
		}
		return nil, err
	}
	return j, nil
}

func (s *Service) GetOwned(ctx context.Context, userID, id uuid.UUID) (*Job, error) {
	return s.getOwned(ctx, s.pool, userID, id, false)
}

// Create saves a new draft.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, in Input) (*Job, error) {
	in.normalise()
	if err := in.validate(s.today()); err != nil {
		return nil, err
	}
	cid, _, err := company.IDByUser(ctx, s.pool, userID)
	if err != nil {
		return nil, err
	}
	var id uuid.UUID
	err = s.pool.QueryRow(ctx,
		`INSERT INTO jobs (company_id, role, type, location, ctc, min_cgpa, branches, skills, deadline, openings, description, allow_backlogs)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::date, $10, $11, $12) RETURNING id`,
		cid, in.Role, in.Type, in.Location, in.CTC, in.MinCGPA, in.Branches, in.Skills, in.Deadline,
		in.Openings, in.Description, in.AllowBacklogs).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.GetOwned(ctx, userID, id)
}

// Update edits a job. Drafts and rejected jobs are fully editable. Once a job
// is live only the deadline, openings, skills and description may change,
// because students were told about (and applied under) the original criteria.
func (s *Service) Update(ctx context.Context, userID, id uuid.UUID, in Input) (*Job, error) {
	in.normalise()
	if err := in.validate(s.today()); err != nil {
		return nil, err
	}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := s.getOwned(ctx, tx, userID, id, true)
		if err != nil {
			return err
		}
		switch cur.Status {
		case domain.JobDraft, domain.JobRejected:
		case domain.JobOpen:
			var locked []string
			if in.Role != cur.Role {
				locked = append(locked, "role")
			}
			if in.Type != cur.Type {
				locked = append(locked, "type")
			}
			if in.Location != cur.Location {
				locked = append(locked, "location")
			}
			if in.CTC != cur.CTC {
				locked = append(locked, "ctc")
			}
			if in.MinCGPA != cur.MinCGPA {
				locked = append(locked, "minCgpa")
			}
			if in.AllowBacklogs != cur.AllowBacklogs {
				locked = append(locked, "allowBacklogs")
			}
			if strings.Join(in.Branches, ",") != strings.Join(cur.Branches, ",") {
				locked = append(locked, "branches")
			}
			if len(locked) > 0 {
				return httpx.Conflict("criteria_locked",
					"these fields cannot change once a job is live: "+strings.Join(locked, ", "))
			}
		default:
			return httpx.Conflict("not_editable", fmt.Sprintf("a %s job cannot be edited", strings.ToLower(cur.Status)))
		}
		_, err = tx.Exec(ctx,
			`UPDATE jobs SET role = $2, type = $3, location = $4, ctc = $5, min_cgpa = $6, branches = $7, skills = $8,
			        deadline = $9::date, openings = $10, description = $11, allow_backlogs = $12, updated_at = now()
			  WHERE id = $1`,
			id, in.Role, in.Type, in.Location, in.CTC, in.MinCGPA, in.Branches, in.Skills, in.Deadline,
			in.Openings, in.Description, in.AllowBacklogs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetOwned(ctx, userID, id)
}

// Submit sends a draft (or a rejected job) for placement-cell approval.
func (s *Service) Submit(ctx context.Context, userID, id uuid.UUID) (*Job, error) {
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := s.getOwned(ctx, tx, userID, id, true)
		if err != nil {
			return err
		}
		if cur.Status != domain.JobDraft && cur.Status != domain.JobRejected {
			return httpx.Conflict("bad_state", fmt.Sprintf("a %s job cannot be submitted", strings.ToLower(cur.Status)))
		}
		var cstatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM companies WHERE id = $1`, cur.CompanyID).Scan(&cstatus); err != nil {
			return err
		}
		if cstatus != domain.CompanyApproved {
			return httpx.Unprocessable("company_not_approved",
				"your company must be approved by the placement cell before you can submit jobs")
		}
		var hasRounds bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM job_rounds WHERE job_id = $1)`, id).Scan(&hasRounds); err != nil {
			return err
		}
		if !hasRounds {
			return httpx.Unprocessable("rounds_required", "define at least one selection round before submitting this job for approval")
		}
		in := Input{Role: cur.Role, Type: cur.Type, Location: cur.Location, CTC: cur.CTC, MinCGPA: cur.MinCGPA,
			Branches: cur.Branches, Skills: cur.Skills, Deadline: cur.Deadline, Openings: cur.Openings,
			Description: cur.Description, AllowBacklogs: cur.AllowBacklogs}
		if err := in.validate(s.today()); err != nil { // e.g. the draft's deadline has since passed
			return err
		}
		_, err = tx.Exec(ctx,
			`UPDATE jobs SET status = 'Pending', reject_reason = '', submitted_at = now(), updated_at = now() WHERE id = $1`, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetOwned(ctx, userID, id)
}

// Close stops a live job from taking more applications.
func (s *Service) Close(ctx context.Context, userID, id uuid.UUID) (*Job, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs j SET status = 'Closed', closed_at = now(), updated_at = now()
		   FROM companies c WHERE c.id = j.company_id AND c.user_id = $1 AND j.id = $2 AND j.status = 'Open'`, userID, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		cur, err := s.GetOwned(ctx, userID, id)
		if err != nil {
			return nil, err
		}
		return nil, httpx.Conflict("bad_state", fmt.Sprintf("a %s job cannot be closed", strings.ToLower(cur.Status)))
	}
	return s.GetOwned(ctx, userID, id)
}

// Delete removes a draft that has never been submitted.
func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM jobs j USING companies c
		  WHERE c.id = j.company_id AND c.user_id = $1 AND j.id = $2 AND j.status = 'Draft'`, userID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		cur, err := s.GetOwned(ctx, userID, id)
		if err != nil {
			return err
		}
		return httpx.Conflict("bad_state", fmt.Sprintf("only drafts can be deleted, this job is %s", strings.ToLower(cur.Status)))
	}
	return nil
}

// ---- admin ----------------------------------------------------------------

func (s *Service) ListForAdmin(ctx context.Context, status string) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+jobCols+jobFrom+
		` WHERE ($1 = '' OR j.status = $1) AND j.status <> 'Draft' ORDER BY j.submitted_at DESC NULLS LAST, j.id`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// Approve makes a pending job live and, in the same transaction, notifies
// every student who is eligible for it and nobody else.
func (s *Service) Approve(ctx context.Context, id, actor uuid.UUID, ip string) (job *Job, notified int64, err error) {
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobCols+jobFrom+` WHERE j.id = $1 FOR UPDATE OF j`, id))
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("job")
			}
			return err
		}
		if cur.Status != domain.JobPending {
			return httpx.Conflict("bad_state", fmt.Sprintf("only pending jobs can be approved, this job is %s", strings.ToLower(cur.Status)))
		}
		var cstatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM companies WHERE id = $1`, cur.CompanyID).Scan(&cstatus); err != nil {
			return err
		}
		if cstatus != domain.CompanyApproved {
			return httpx.Unprocessable("company_not_approved", "approve the company before approving its jobs")
		}
		if end, err := DeadlineEnd(cur.Deadline, s.loc); err != nil || !s.Now().Before(end) {
			return httpx.Unprocessable("deadline_passed", "the application deadline has already passed")
		}
		if _, err := tx.Exec(ctx,
			`UPDATE jobs SET status = 'Open', approved_at = now(), reject_reason = '', updated_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		notified, err = s.notifyNewJob(ctx, tx, cur)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	audit.Log(ctx, s.pool, &actor, "job.approved", "job", id.String(), ip, map[string]any{"notified": notified})
	job, err = s.getAny(ctx, id)
	return job, notified, err
}

func (s *Service) getAny(ctx context.Context, id uuid.UUID) (*Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobCols+jobFrom+` WHERE j.id = $1`, id))
	if db.IsNoRows(err) {
		return nil, httpx.NotFound("job")
	}
	return j, err
}

// eligibleRecipients selects the user ids of students who may apply to a job.
// $1 = min cgpa, $2 = branches, $3 = allow backlogs.
func eligibleRecipients() string {
	return `SELECT s.user_id FROM students s JOIN users u ON u.id = s.user_id
	         WHERE u.active AND ` + eligibility.Eligible("s", "$1", "$2", "$3")
}

func (s *Service) notifyNewJob(ctx context.Context, q db.DBTX, j *Job) (int64, error) {
	return s.notifier.ToUsers(ctx, q, eligibleRecipients(), []any{j.MinCGPA, j.Branches, j.AllowBacklogs}, notify.Spec{
		Type:      domain.NotifNewJob,
		Title:     fmt.Sprintf("New job: %s at %s", j.Role, j.Company),
		Body:      fmt.Sprintf("%s · %s · %s · apply by %s", formatCTC(j.CTC, j.Type), j.Location, j.Type, j.Deadline),
		Link:      "/students/jobs?job=" + j.ID.String(),
		Data:      map[string]any{"jobId": j.ID, "company": j.Company},
		DedupeKey: "job_new:" + j.ID.String(),
	})
}

func formatCTC(ctc float64, jobType string) string {
	if jobType == domain.JobTypes[1] {
		return fmt.Sprintf("₹%.1f LPA (annualised stipend)", ctc)
	}
	return fmt.Sprintf("₹%.1f LPA", ctc)
}

// Reject sends a pending job back to the company with a reason.
func (s *Service) Reject(ctx context.Context, id uuid.UUID, reason string, actor uuid.UUID, ip string) (*Job, error) {
	reason = strings.TrimSpace(reason)
	var v httpx.V
	v.Text("reason", reason, 3, 500)
	if err := v.Err(); err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status = 'Rejected', reject_reason = $2, updated_at = now() WHERE id = $1 AND status = 'Pending'`, id, reason)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		cur, err := s.getAny(ctx, id)
		if err != nil {
			return nil, err
		}
		return nil, httpx.Conflict("bad_state", fmt.Sprintf("only pending jobs can be rejected, this job is %s", strings.ToLower(cur.Status)))
	}
	audit.Log(ctx, s.pool, &actor, "job.rejected", "job", id.String(), ip, nil)
	return s.getAny(ctx, id)
}

// AdminClose lets the placement cell close any live job.
func (s *Service) AdminClose(ctx context.Context, id, actor uuid.UUID, ip string) (*Job, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status = 'Closed', closed_at = now(), updated_at = now() WHERE id = $1 AND status = 'Open'`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		cur, err := s.getAny(ctx, id)
		if err != nil {
			return nil, err
		}
		return nil, httpx.Conflict("bad_state", fmt.Sprintf("a %s job cannot be closed", strings.ToLower(cur.Status)))
	}
	audit.Log(ctx, s.pool, &actor, "job.closed", "job", id.String(), ip, nil)
	return s.getAny(ctx, id)
}

// ---- scheduler hooks ------------------------------------------------------

// CloseExpired closes open jobs whose deadline day has ended.
func (s *Service) CloseExpired(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status = 'Closed', closed_at = now(), updated_at = now()
		  WHERE status = 'Open' AND deadline < $1::date`, s.today().Format(dateLayout))
	return tag.RowsAffected(), err
}

// SendDeadlineReminders notifies eligible students who have not applied when a
// live job is within 48h or 24h of closing. A window only fires for jobs that
// were already live when it began, so a job approved with a day to go does not
// send a "closing soon" straight after its "new job" alert. Dedupe keys make
// repeated runs harmless.
func (s *Service) SendDeadlineReminders(ctx context.Context) (int64, error) {
	var total int64
	for _, hours := range []int{48, 24} {
		rows, err := s.pool.Query(ctx, `SELECT `+jobCols+jobFrom+`
		  WHERE j.status = 'Open'
		    AND ((j.deadline + 1)::timestamp AT TIME ZONE $1) - make_interval(hours => $2) <= $3
		    AND ((j.deadline + 1)::timestamp AT TIME ZONE $1) > $3
		    AND j.approved_at <= ((j.deadline + 1)::timestamp AT TIME ZONE $1) - make_interval(hours => $2)`,
			s.loc.String(), hours, s.Now())
		if err != nil {
			return total, err
		}
		var jobs []Job
		for rows.Next() {
			j, err := scanJob(rows)
			if err != nil {
				rows.Close()
				return total, err
			}
			jobs = append(jobs, *j)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, err
		}
		for _, j := range jobs {
			n, err := s.notifier.ToUsers(ctx, s.pool,
				eligibleRecipients()+` AND NOT EXISTS (SELECT 1 FROM applications a WHERE a.job_id = $4 AND a.student_id = s.id)`,
				[]any{j.MinCGPA, j.Branches, j.AllowBacklogs, j.ID}, notify.Spec{
					Type:      domain.NotifDeadline,
					Title:     fmt.Sprintf("Closing soon: %s at %s", j.Role, j.Company),
					Body:      fmt.Sprintf("Applications close on %s. Apply before the deadline.", j.Deadline),
					Link:      "/students/jobs?job=" + j.ID.String(),
					Data:      map[string]any{"jobId": j.ID, "window": hours},
					DedupeKey: fmt.Sprintf("deadline_%dh:%s", hours, j.ID),
				})
			if err != nil {
				return total, err
			}
			total += n
		}
	}
	return total, nil
}
