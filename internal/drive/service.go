// Package drive covers campus drives: the placement cell schedules them and
// eligible, unplaced students register.
package drive

import (
	"context"
	"fmt"
	"strings"
	"time"

	"placementhub/internal/audit"
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

// Drive matches the frontend's Drive type, plus criteria fields.
type Drive struct {
	ID            uuid.UUID  `json:"id"`
	CompanyID     uuid.UUID  `json:"companyId"`
	Company       string     `json:"company"`
	Color         string     `json:"color"`
	JobID         *uuid.UUID `json:"jobId"`
	Title         string     `json:"title"`
	Date          string     `json:"date"`
	Time          string     `json:"time"`
	StartsAt      time.Time  `json:"startsAt"`
	Duration      int        `json:"durationMinutes"`
	Mode          string     `json:"mode"`
	Venue         string     `json:"venue"`
	Registered    int        `json:"registered"`
	Eligible      int        `json:"eligible"`
	Status        string     `json:"status"`
	MinCGPA       float64    `json:"minCgpa"`
	Branches      []string   `json:"branches"`
	AllowBacklogs bool       `json:"allowBacklogs"`
}

// StudentDrive adds the caller's own state.
type StudentDrive struct {
	Drive
	IsRegistered bool                `json:"isRegistered"`
	CanRegister  bool                `json:"canRegister"`
	BlockReason  *eligibility.Reason `json:"blockReason"`
}

// $1 = timezone, $2 = now.
const driveCols = `
d.id, c.id, c.name, c.color, d.job_id, d.title,
to_char(d.starts_at AT TIME ZONE $1, 'YYYY-MM-DD'), to_char(d.starts_at AT TIME ZONE $1, 'HH12:MI AM'),
d.starts_at, d.duration_minutes, d.mode, d.venue,
(SELECT count(*) FROM drive_registrations dr WHERE dr.drive_id = d.id)::int,
(SELECT count(*) FROM students s JOIN users u ON u.id = s.user_id
  WHERE u.active AND ` + `%s` + `)::int,
%s,
d.min_cgpa, d.branches, d.allow_backlogs`

// statusSQL derives Upcoming / Ongoing / Completed from the clock ($2 = now),
// so no scheduler has to keep a status column current.
const statusSQL = `(CASE WHEN $2 < d.starts_at THEN 'Upcoming'
     WHEN $2 < d.starts_at + make_interval(mins => d.duration_minutes) THEN 'Ongoing'
     ELSE 'Completed' END)`

var driveSelect = fmt.Sprintf(driveCols, eligibility.Eligible("s", "d.min_cgpa", "d.branches", "d.allow_backlogs"), statusSQL)

const driveFrom = ` FROM drives d JOIN companies c ON c.id = d.company_id`

func scanDrive(row pgx.Row, extra ...any) (*Drive, error) {
	var d Drive
	dest := append([]any{&d.ID, &d.CompanyID, &d.Company, &d.Color, &d.JobID, &d.Title, &d.Date, &d.Time, &d.StartsAt,
		&d.Duration, &d.Mode, &d.Venue, &d.Registered, &d.Eligible, &d.Status, &d.MinCGPA, &d.Branches, &d.AllowBacklogs}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	if d.Branches == nil {
		d.Branches = []string{}
	}
	return &d, nil
}

func (s *Service) get(ctx context.Context, id uuid.UUID) (*Drive, error) {
	d, err := scanDrive(s.pool.QueryRow(ctx, `SELECT `+driveSelect+driveFrom+` WHERE d.id = $3`, s.loc.String(), s.Now(), id))
	if db.IsNoRows(err) {
		return nil, httpx.NotFound("drive")
	}
	return d, err
}

// ---- admin ----------------------------------------------------------------

type Input struct {
	CompanyID     uuid.UUID  `json:"companyId"`
	JobID         *uuid.UUID `json:"jobId"`
	Title         string     `json:"title"`
	Date          string     `json:"date"` // YYYY-MM-DD
	Time          string     `json:"time"` // HH:MM, 24-hour
	Duration      int        `json:"durationMinutes"`
	Mode          string     `json:"mode"`
	Venue         string     `json:"venue"`
	MinCGPA       *float64   `json:"minCgpa"`
	Branches      *[]string  `json:"branches"`
	AllowBacklogs *bool      `json:"allowBacklogs"`
}

type resolved struct {
	Input
	startsAt      time.Time
	minCGPA       float64
	branches      []string
	allowBacklogs bool
}

// resolve validates the input and fills criteria from the linked job when omitted.
func (s *Service) resolve(ctx context.Context, q db.DBTX, in Input) (*resolved, error) {
	in.Title = strings.TrimSpace(in.Title)
	in.Venue = strings.TrimSpace(in.Venue)
	if in.Duration == 0 {
		in.Duration = 180
	}
	var v httpx.V
	v.Text("title", in.Title, 2, 160)
	v.Text("venue", in.Venue, 0, 160)
	v.OneOf("mode", in.Mode, domain.DriveModes...)
	v.Range("durationMinutes", float64(in.Duration), 15, 24*60)
	if in.CompanyID == uuid.Nil {
		v.Add("companyId", "is required")
	}
	r := &resolved{Input: in, minCGPA: 0, branches: []string{}, allowBacklogs: true}
	if d, err := time.ParseInLocation("2006-01-02 15:04", in.Date+" "+in.Time, s.loc); err != nil {
		v.Add("date", "date must be YYYY-MM-DD and time HH:MM (24-hour)")
	} else {
		r.startsAt = d
	}
	if err := v.Err(); err != nil {
		return nil, err
	}

	var ok bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM companies WHERE id = $1)`, in.CompanyID).Scan(&ok); err != nil {
		return nil, err
	}
	if !ok {
		return nil, httpx.Unprocessable("bad_company", "company not found")
	}
	if in.JobID != nil { // criteria default to the job's, and the job must belong to the company
		var jc uuid.UUID
		var minC float64
		var br []string
		var ab bool
		err := q.QueryRow(ctx, `SELECT company_id, min_cgpa, branches, allow_backlogs FROM jobs WHERE id = $1`, *in.JobID).Scan(&jc, &minC, &br, &ab)
		if err != nil {
			if db.IsNoRows(err) {
				return nil, httpx.Unprocessable("bad_job", "job not found")
			}
			return nil, err
		}
		if jc != in.CompanyID {
			return nil, httpx.Unprocessable("bad_job", "the job does not belong to this company")
		}
		r.minCGPA, r.branches, r.allowBacklogs = minC, br, ab
	}
	if in.MinCGPA != nil {
		if *in.MinCGPA < 0 || *in.MinCGPA > 10 {
			return nil, httpx.Unprocessable("validation_failed", "minCgpa must be between 0 and 10")
		}
		r.minCGPA = *in.MinCGPA
	}
	if in.Branches != nil {
		for _, b := range *in.Branches {
			if !domain.ValidBranch(b) {
				return nil, httpx.Unprocessable("validation_failed", "unknown branch: "+b)
			}
		}
		r.branches = *in.Branches
	}
	if in.AllowBacklogs != nil {
		r.allowBacklogs = *in.AllowBacklogs
	}
	return r, nil
}

func (s *Service) Create(ctx context.Context, in Input, actor uuid.UUID, ip string) (*Drive, error) {
	r, err := s.resolve(ctx, s.pool, in)
	if err != nil {
		return nil, err
	}
	if !r.startsAt.After(s.Now()) {
		return nil, httpx.Unprocessable("validation_failed", "a new drive must start in the future")
	}
	var id uuid.UUID
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO drives (company_id, job_id, title, starts_at, duration_minutes, mode, venue, min_cgpa, branches, allow_backlogs, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
			r.CompanyID, r.JobID, r.Title, r.startsAt, r.Duration, r.Mode, r.Venue, r.minCGPA, r.branches, r.allowBacklogs, actor).Scan(&id); err != nil {
			return err
		}
		return s.announce(ctx, tx, id)
	})
	if err != nil {
		return nil, err
	}
	audit.Log(ctx, s.pool, &actor, "drive.created", "drive", id.String(), ip, nil)
	return s.get(ctx, id)
}

// announce tells every eligible, unplaced student about a new drive.
func (s *Service) announce(ctx context.Context, q db.DBTX, id uuid.UUID) error {
	d, err := scanDrive(q.QueryRow(ctx, `SELECT `+driveSelect+driveFrom+` WHERE d.id = $3`, s.loc.String(), s.Now(), id))
	if err != nil {
		return err
	}
	_, err = s.notifier.ToUsers(ctx, q,
		`SELECT s.user_id FROM students s JOIN users u ON u.id = s.user_id
		  WHERE u.active AND `+eligibility.Eligible("s", "$1", "$2", "$3"),
		[]any{d.MinCGPA, d.Branches, d.AllowBacklogs}, notify.Spec{
			Type:      domain.NotifDriveNew,
			Title:     fmt.Sprintf("New drive: %s", d.Title),
			Body:      fmt.Sprintf("%s · %s at %s · %s", d.Company, d.Date, d.Time, d.Venue),
			Link:      "/students",
			Data:      map[string]any{"driveId": d.ID},
			DedupeKey: "drive_new:" + d.ID.String(),
		})
	return err
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, in Input, actor uuid.UUID, ip string) (*Drive, error) {
	r, err := s.resolve(ctx, s.pool, in)
	if err != nil {
		return nil, err
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var oldStart time.Time
		var oldMode, oldVenue string
		var version int
		if err := tx.QueryRow(ctx, `SELECT starts_at, mode, venue, version FROM drives WHERE id = $1 FOR UPDATE`, id).
			Scan(&oldStart, &oldMode, &oldVenue, &version); err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("drive")
			}
			return err
		}
		if !oldStart.Equal(r.startsAt) && !r.startsAt.After(s.Now()) {
			return httpx.Unprocessable("validation_failed", "the new start time must be in the future")
		}
		logistics := !oldStart.Equal(r.startsAt) || oldMode != r.Mode || oldVenue != r.Venue
		if logistics {
			version++
		}
		if _, err := tx.Exec(ctx,
			`UPDATE drives SET company_id = $2, job_id = $3, title = $4, starts_at = $5, duration_minutes = $6, mode = $7, venue = $8,
			        min_cgpa = $9, branches = $10, allow_backlogs = $11, version = $12, updated_at = now() WHERE id = $1`,
			id, r.CompanyID, r.JobID, r.Title, r.startsAt, r.Duration, r.Mode, r.Venue, r.minCGPA, r.branches, r.allowBacklogs, version); err != nil {
			return err
		}
		if !logistics {
			return nil
		}
		_, err := s.notifier.ToUsers(ctx, tx,
			`SELECT s.user_id FROM drive_registrations dr JOIN students s ON s.id = dr.student_id WHERE dr.drive_id = $1`,
			[]any{id}, notify.Spec{
				Type:      domain.NotifDriveUpdated,
				Title:     fmt.Sprintf("Drive updated: %s", r.Title),
				Body:      fmt.Sprintf("Now %s at %s · %s (%s)", r.startsAt.In(s.loc).Format("2 Jan"), r.startsAt.In(s.loc).Format("3:04 PM"), r.Venue, r.Mode),
				Link:      "/students",
				Data:      map[string]any{"driveId": id},
				DedupeKey: fmt.Sprintf("drive_update:%s:%d", id, version),
			})
		return err
	})
	if err != nil {
		return nil, err
	}
	audit.Log(ctx, s.pool, &actor, "drive.updated", "drive", id.String(), ip, nil)
	return s.get(ctx, id)
}

// Delete cancels a drive, telling registered students first.
func (s *Service) Delete(ctx context.Context, id, actor uuid.UUID, ip string) error {
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var title string
		var startsAt time.Time
		if err := tx.QueryRow(ctx, `SELECT title, starts_at FROM drives WHERE id = $1 FOR UPDATE`, id).Scan(&title, &startsAt); err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("drive")
			}
			return err
		}
		if startsAt.After(s.Now()) {
			if _, err := s.notifier.ToUsers(ctx, tx,
				`SELECT s.user_id FROM drive_registrations dr JOIN students s ON s.id = dr.student_id WHERE dr.drive_id = $1`,
				[]any{id}, notify.Spec{
					Type:      domain.NotifDriveUpdated,
					Title:     fmt.Sprintf("Drive cancelled: %s", title),
					Body:      "The placement cell has cancelled this drive.",
					Link:      "/students",
					DedupeKey: "drive_cancel:" + id.String(),
				}); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM drives WHERE id = $1`, id)
		return err
	})
	if err == nil {
		audit.Log(ctx, s.pool, &actor, "drive.deleted", "drive", id.String(), ip, nil)
	}
	return err
}

func (s *Service) ListAll(ctx context.Context, status string) ([]Drive, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+driveSelect+driveFrom+` WHERE ($3 = '' OR `+statusSQL+` = $3)`+
		` ORDER BY d.starts_at, d.id`, s.loc.String(), s.Now(), status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Drive{}
	for rows.Next() {
		d, err := scanDrive(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

type Registrant struct {
	StudentID    uuid.UUID `json:"studentId"`
	Name         string    `json:"name"`
	Roll         string    `json:"roll"`
	Email        string    `json:"email"`
	Branch       string    `json:"branch"`
	CGPA         float64   `json:"cgpa"`
	RegisteredAt time.Time `json:"registeredAt"`
}

func (s *Service) Registrations(ctx context.Context, id uuid.UUID) ([]Registrant, error) {
	if _, err := s.get(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT s.id, u.name, s.roll, u.email, s.branch, s.cgpa, dr.created_at
		   FROM drive_registrations dr JOIN students s ON s.id = dr.student_id JOIN users u ON u.id = s.user_id
		  WHERE dr.drive_id = $1 ORDER BY dr.created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Registrant{}
	for rows.Next() {
		var r Registrant
		if err := rows.Scan(&r.StudentID, &r.Name, &r.Roll, &r.Email, &r.Branch, &r.CGPA, &r.RegisteredAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- student --------------------------------------------------------------

func studentExtras() string {
	return `,
  ` + eligibility.Placed("me") + `,
  ` + eligibility.CGPAOK("me", "d.min_cgpa") + `,
  ` + eligibility.BranchOK("me", "d.branches") + `,
  ` + eligibility.BacklogsOK("me", "d.allow_backlogs") + `,
  EXISTS (SELECT 1 FROM drive_registrations r WHERE r.drive_id = d.id AND r.student_id = me.id), me.cgpa`
}

func (s *Service) toStudentDrive(d *Drive, placed, cgpaOK, branchOK, backlogsOK, registered bool, cgpa float64) StudentDrive {
	sd := StudentDrive{Drive: *d, IsRegistered: registered}
	reason := eligibility.Explain(placed, cgpaOK, branchOK, backlogsOK, cgpa, d.MinCGPA)
	switch {
	case registered:
		sd.BlockReason = &eligibility.Reason{Code: "already_registered", Message: "You are already registered for this drive."}
	case reason != nil:
		sd.BlockReason = reason
	case d.Status != "Upcoming":
		sd.BlockReason = &eligibility.Reason{Code: "drive_started", Message: "Registration is closed because the drive has started."}
	}
	sd.CanRegister = sd.BlockReason == nil
	return sd
}

// ListForStudent returns upcoming and ongoing drives (scope=past for finished
// ones) with the student's eligibility and registration state.
func (s *Service) ListForStudent(ctx context.Context, userID uuid.UUID, past bool) ([]StudentDrive, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+driveSelect+studentExtras()+driveFrom+`
	  JOIN students me ON me.user_id = $3
	 WHERE (`+statusSQL+` = 'Completed') = $4 ORDER BY d.starts_at, d.id`, s.loc.String(), s.Now(), userID, past)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StudentDrive{}
	for rows.Next() {
		var placed, cgpaOK, branchOK, backlogsOK, registered bool
		var cgpa float64
		d, err := scanDrive(rows, &placed, &cgpaOK, &branchOK, &backlogsOK, &registered, &cgpa)
		if err != nil {
			return nil, err
		}
		out = append(out, s.toStudentDrive(d, placed, cgpaOK, branchOK, backlogsOK, registered, cgpa))
	}
	return out, rows.Err()
}

func errPlaced() *httpx.Error {
	return httpx.NewError(403, "already_placed",
		"You have accepted a placement offer, so you cannot apply to further jobs or drives.")
}

// Register signs an eligible, unplaced student up for a drive that has not started.
func (s *Service) Register(ctx context.Context, userID, driveID uuid.UUID) (*StudentDrive, error) {
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var sid uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM students WHERE user_id = $1 FOR UPDATE`, userID).Scan(&sid); err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("student profile")
			}
			return err
		}
		sd, err := s.studentDrive(ctx, tx, userID, driveID)
		if err != nil {
			return err
		}
		if sd.IsRegistered {
			return nil // idempotent
		}
		if !sd.CanRegister {
			if sd.BlockReason.Code == "already_placed" {
				return errPlaced()
			}
			return httpx.Unprocessable(sd.BlockReason.Code, sd.BlockReason.Message)
		}
		_, err = tx.Exec(ctx, `INSERT INTO drive_registrations (drive_id, student_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, driveID, sid)
		return err
	})
	if err != nil {
		if db.PgCode(err) == db.CodeStudentPlaced {
			return nil, errPlaced()
		}
		return nil, err
	}
	return s.studentDrive(ctx, s.pool, userID, driveID)
}

func (s *Service) studentDrive(ctx context.Context, q db.DBTX, userID, driveID uuid.UUID) (*StudentDrive, error) {
	var placed, cgpaOK, branchOK, backlogsOK, registered bool
	var cgpa float64
	d, err := scanDrive(q.QueryRow(ctx, `SELECT `+driveSelect+studentExtras()+driveFrom+`
	  JOIN students me ON me.user_id = $3 WHERE d.id = $4`, s.loc.String(), s.Now(), userID, driveID),
		&placed, &cgpaOK, &branchOK, &backlogsOK, &registered, &cgpa)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("drive")
		}
		return nil, err
	}
	sd := s.toStudentDrive(d, placed, cgpaOK, branchOK, backlogsOK, registered, cgpa)
	return &sd, nil
}

// Unregister cancels a registration before the drive starts.
func (s *Service) Unregister(ctx context.Context, userID, driveID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM drive_registrations dr USING students s, drives d
		  WHERE s.user_id = $1 AND dr.student_id = s.id AND dr.drive_id = $2 AND d.id = dr.drive_id AND d.starts_at > $3`,
		userID, driveID, s.Now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("registration")
	}
	return nil
}

// ---- scheduler hook -------------------------------------------------------

// SendReminders notifies registered students of drives starting within 24 hours.
func (s *Service) SendReminders(ctx context.Context) (int64, error) {
	now := s.Now()
	rows, err := s.pool.Query(ctx,
		`SELECT d.id, d.title, c.name, d.starts_at, d.venue FROM drives d JOIN companies c ON c.id = d.company_id
		  WHERE d.starts_at > $1 AND d.starts_at <= $2`, now, now.Add(24*time.Hour))
	if err != nil {
		return 0, err
	}
	type due struct {
		id                    uuid.UUID
		title, company, venue string
		at                    time.Time
	}
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.title, &d.company, &d.at, &d.venue); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var total int64
	for _, d := range list {
		n, err := s.notifier.ToUsers(ctx, s.pool,
			`SELECT s.user_id FROM drive_registrations dr JOIN students s ON s.id = dr.student_id WHERE dr.drive_id = $1`,
			[]any{d.id}, notify.Spec{
				Type:      domain.NotifDriveReminder,
				Title:     fmt.Sprintf("Tomorrow: %s", d.title),
				Body:      fmt.Sprintf("%s · %s · %s", d.company, d.at.In(s.loc).Format("2 Jan, 3:04 PM"), d.venue),
				Link:      "/students",
				Data:      map[string]any{"driveId": d.id},
				DedupeKey: "drive_reminder:" + d.id.String(),
			})
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
