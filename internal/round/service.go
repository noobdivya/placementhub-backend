package round

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"placementhub/internal/config"
	"placementhub/internal/db"
	"placementhub/internal/domain"
	"placementhub/internal/email"
	"placementhub/internal/httpx"
	"placementhub/internal/notify"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool      *pgxpool.Pool
	notifier  *notify.Notifier
	loc       *time.Location
	Now       func() time.Time
	LeadTimes []time.Duration // configured reminder lead times, e.g. 24h/1h before a Scheduled round
}

func NewService(pool *pgxpool.Pool, n *notify.Notifier, cfg config.Config) *Service {
	return &Service{pool: pool, notifier: n, loc: cfg.Location, Now: time.Now, LeadTimes: cfg.EmailReminderLeadTimes}
}

const roundCols = `id, seq, name, mode, scheduled_at, duration_minutes, location, instructions`

func scanRound(row pgx.Row) (*Round, error) {
	var r Round
	if err := row.Scan(&r.ID, &r.Seq, &r.Name, &r.Mode, &r.ScheduledAt, &r.DurationMinutes, &r.Location, &r.Instructions); err != nil {
		return nil, err
	}
	return &r, nil
}

func listRounds(ctx context.Context, q db.DBTX, jobID uuid.UUID) ([]Round, error) {
	rows, err := q.Query(ctx, `SELECT `+roundCols+` FROM job_rounds WHERE job_id = $1 ORDER BY seq`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Round{}
	for rows.Next() {
		r, err := scanRound(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ---- student ---------------------------------------------------------------

// ForJob returns a job's ordered rounds, visible to a student under the same
// rule as the job itself: it's Open, or they've already applied to it.
func (s *Service) ForJob(ctx context.Context, userID, jobID uuid.UUID) ([]Round, error) {
	var visible bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM jobs j
			 WHERE j.id = $1 AND (j.status = 'Open' OR EXISTS (
				SELECT 1 FROM applications a JOIN students st ON st.id = a.student_id
				 WHERE a.job_id = j.id AND st.user_id = $2
			 ))
		)`, jobID, userID).Scan(&visible)
	if err != nil {
		return nil, err
	}
	if !visible {
		return nil, httpx.NotFound("job")
	}
	return listRounds(ctx, s.pool, jobID)
}

// applyCurrent marks the lowest-seq round that isn't Cleared as current.
// Rounds must already be in seq order.
func applyCurrent(items []Progress) {
	for i := range items {
		if items[i].Status != domain.RoundCleared {
			items[i].Current = true
			return
		}
	}
}

// progressForApp returns jobID's rounds joined with one application's status,
// defaulting to Upcoming for any round the application has no row for yet.
func progressForApp(ctx context.Context, q db.DBTX, jobID, appID uuid.UUID) ([]Progress, error) {
	rows, err := q.Query(ctx, `
		SELECT jr.id, jr.seq, jr.name, jr.mode, jr.scheduled_at, jr.duration_minutes, jr.location, jr.instructions,
		       COALESCE(ars.status, 'Upcoming'), COALESCE(ars.note, ''), COALESCE(ars.updated_at, jr.updated_at)
		  FROM job_rounds jr
		  LEFT JOIN application_round_status ars ON ars.round_id = jr.id AND ars.application_id = $2
		 WHERE jr.job_id = $1
		 ORDER BY jr.seq`, jobID, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Progress{}
	for rows.Next() {
		var p Progress
		if err := rows.Scan(&p.ID, &p.Seq, &p.Name, &p.Mode, &p.ScheduledAt, &p.DurationMinutes, &p.Location, &p.Instructions,
			&p.Status, &p.Note, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	applyCurrent(out)
	return out, nil
}

// ForApplication returns this application's round-by-round progress. userID
// must be the student who owns it.
func (s *Service) ForApplication(ctx context.Context, userID, appID uuid.UUID) ([]Progress, error) {
	var jobID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT a.job_id FROM applications a JOIN students st ON st.id = a.student_id
		 WHERE a.id = $1 AND st.user_id = $2`, appID, userID).Scan(&jobID)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("application")
		}
		return nil, err
	}
	return progressForApp(ctx, s.pool, jobID, appID)
}

// ---- company ----------------------------------------------------------------

func (s *Service) ownedJobID(ctx context.Context, q db.DBTX, userID, jobID uuid.UUID, forUpdate bool) error {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE OF j"
	}
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT j.id FROM jobs j JOIN companies c ON c.id = j.company_id
	                          WHERE j.id = $1 AND c.user_id = $2`+lock, jobID, userID).Scan(&id)
	if db.IsNoRows(err) {
		return httpx.NotFound("job") // also covers "someone else's job": never reveal it exists
	}
	return err
}

// ForCompanyJob returns a job's rounds for the editor, each flagged Locked if
// any candidate has already progressed past Upcoming on it.
func (s *Service) ForCompanyJob(ctx context.Context, userID, jobID uuid.UUID) ([]Round, error) {
	if err := s.ownedJobID(ctx, s.pool, userID, jobID, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT jr.id, jr.seq, jr.name, jr.mode, jr.scheduled_at, jr.duration_minutes, jr.location, jr.instructions,
		       EXISTS (SELECT 1 FROM application_round_status ars WHERE ars.round_id = jr.id AND ars.status <> 'Upcoming')
		  FROM job_rounds jr WHERE jr.job_id = $1 ORDER BY jr.seq`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Round{}
	for rows.Next() {
		var r Round
		if err := rows.Scan(&r.ID, &r.Seq, &r.Name, &r.Mode, &r.ScheduledAt, &r.DurationMinutes, &r.Location, &r.Instructions, &r.Locked); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type existingRound struct {
	seq    int
	name   string
	locked bool
}

// Replace fully replaces a job's ordered round list: add, remove, reorder and
// edit in one call. A round already in progress for some candidate (any
// application_round_status row with status <> Upcoming) may not be removed,
// renamed, or reordered relative to another locked round — but its own
// schedule/mode/location/instructions may still change freely, and it can be
// notified as a reschedule.
func (s *Service) Replace(ctx context.Context, userID, jobID uuid.UUID, items []RoundInput) ([]Round, error) {
	items, err := normaliseAndValidateItems(items)
	if err != nil {
		return nil, err
	}

	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.ownedJobID(ctx, tx, userID, jobID, true); err != nil {
			return err
		}

		rows, err := tx.Query(ctx, `
			SELECT jr.id, jr.seq, jr.name,
			       EXISTS (SELECT 1 FROM application_round_status ars WHERE ars.round_id = jr.id AND ars.status <> 'Upcoming')
			  FROM job_rounds jr WHERE jr.job_id = $1 ORDER BY jr.seq`, jobID)
		if err != nil {
			return err
		}
		cur := map[uuid.UUID]existingRound{}
		var lockedOldOrder []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			var e existingRound
			if err := rows.Scan(&id, &e.seq, &e.name, &e.locked); err != nil {
				rows.Close()
				return err
			}
			cur[id] = e
			if e.locked {
				lockedOldOrder = append(lockedOldOrder, id)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}

		newOrder := make([]uuid.UUID, 0, len(items))
		for _, it := range items {
			if it.ID != nil {
				newOrder = append(newOrder, *it.ID)
			}
		}
		var lockedNewOrder []uuid.UUID
		var badNames []string
		for id, e := range cur {
			if !e.locked {
				continue
			}
			idx := slices.Index(newOrder, id)
			if idx == -1 {
				badNames = append(badNames, e.name)
				continue
			}
			if items[slices.IndexFunc(items, func(it RoundInput) bool { return it.ID != nil && *it.ID == id })].Name != e.name {
				badNames = append(badNames, e.name)
				continue
			}
			lockedNewOrder = append(lockedNewOrder, id)
		}
		if len(badNames) == 0 && !slices.Equal(lockedOldOrder, lockedNewOrder) {
			// relative order of the locked rounds changed
			for _, id := range lockedOldOrder {
				badNames = append(badNames, cur[id].name)
			}
		}
		if len(badNames) > 0 {
			return httpx.Conflict("rounds_locked", "candidates are already in progress on: "+strings.Join(badNames, ", "))
		}

		// Deleted one at a time rather than "id <> ALL($2::uuid[])": under
		// QueryExecModeExec (see internal/db.Connect) pgx has no Describe step
		// to learn $2's real type, and a []uuid.UUID slice has no default OID
		// mapping the way []string does (that's how jobs.branches/skills get
		// away with it) — it fails client-side trying to encode the array.
		// Round counts are always small, so per-row deletes cost nothing.
		for id := range cur {
			if !slices.Contains(newOrder, id) {
				if _, err := tx.Exec(ctx, `DELETE FROM job_rounds WHERE id = $1`, id); err != nil {
					return err
				}
			}
		}
		var reschedule []uuid.UUID
		for i, it := range items {
			seq := i + 1
			if it.ID != nil {
				e := cur[*it.ID]
				var oldMode, oldLocation string
				var oldScheduled *time.Time
				if err := tx.QueryRow(ctx, `SELECT mode, scheduled_at, location FROM job_rounds WHERE id = $1`, *it.ID).
					Scan(&oldMode, &oldScheduled, &oldLocation); err != nil {
					return err
				}
				logisticsChanged := oldMode != it.Mode || oldLocation != it.Location ||
					!sameTimePtr(oldScheduled, it.scheduledAt)
				bump := 0
				if logisticsChanged && e.locked {
					bump = 1
				}
				if _, err := tx.Exec(ctx, `
					UPDATE job_rounds SET seq=$2, name=$3, mode=$4, scheduled_at=$5, duration_minutes=$6,
					       location=$7, instructions=$8, version = version + $9, updated_at = now()
					 WHERE id = $1`,
					*it.ID, seq, it.Name, it.Mode, it.scheduledAt, it.DurationMinutes, it.Location, it.Instructions, bump); err != nil {
					return err
				}
				if bump > 0 {
					reschedule = append(reschedule, *it.ID)
				}
			} else {
				if _, err := tx.Exec(ctx, `
					INSERT INTO job_rounds (job_id, seq, name, mode, scheduled_at, duration_minutes, location, instructions)
					VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
					jobID, seq, it.Name, it.Mode, it.scheduledAt, it.DurationMinutes, it.Location, it.Instructions); err != nil {
					return err
				}
			}
		}
		for _, id := range reschedule {
			if err := s.notifyReschedule(ctx, tx, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.ForCompanyJob(ctx, userID, jobID)
}

func sameTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// ForCompanyApplication returns one candidate's round progress. userID must
// be the recruiter who owns the application's job.
func (s *Service) ForCompanyApplication(ctx context.Context, userID, appID uuid.UUID) ([]Progress, error) {
	var jobID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT a.job_id FROM applications a JOIN jobs j ON j.id = a.job_id JOIN companies c ON c.id = j.company_id
		 WHERE a.id = $1 AND c.user_id = $2`, appID, userID).Scan(&jobID)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("application")
		}
		return nil, err
	}
	return progressForApp(ctx, s.pool, jobID, appID)
}

// SetStatus sets one round's status for one candidate, on behalf of the
// recruiter who owns the application's job. Mirrors application.MoveStage's
// guards (not withdrawn, no accepted offer) since a round can still be
// scheduled for a candidate the company hasn't yet decided to reject overall.
func (s *Service) SetStatus(ctx context.Context, userID, appID, roundID uuid.UUID, in StatusChange) (*Progress, error) {
	in.Note = strings.TrimSpace(in.Note)
	var v httpx.V
	v.OneOf("status", in.Status, domain.RoundStatuses...)
	v.Text("note", in.Note, 0, 300)
	if err := v.Err(); err != nil {
		return nil, err
	}

	var (
		studentID                       uuid.UUID
		jobID                           uuid.UUID
		stage, company, role, roundName string
		roundSeq                        int
		mode, location, instructions    string
		scheduledAt                     *time.Time
		durationMinutes                 int
	)
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT a.student_id, j.id, a.stage, c.name, j.role, jr.name, jr.seq,
			       jr.mode, jr.scheduled_at, jr.duration_minutes, jr.location, jr.instructions
			  FROM applications a
			  JOIN jobs j ON j.id = a.job_id
			  JOIN companies c ON c.id = j.company_id
			  JOIN job_rounds jr ON jr.job_id = j.id AND jr.id = $3
			 WHERE a.id = $1 AND c.user_id = $2
			 FOR UPDATE OF a`, appID, userID, roundID).
			Scan(&studentID, &jobID, &stage, &company, &role, &roundName, &roundSeq,
				&mode, &scheduledAt, &durationMinutes, &location, &instructions)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("application") // covers "not your job" and "no such round on this job" alike
			}
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

		var prev string
		err = tx.QueryRow(ctx, `SELECT status FROM application_round_status WHERE application_id = $1 AND round_id = $2`,
			appID, roundID).Scan(&prev)
		if db.IsNoRows(err) {
			prev = domain.RoundUpcoming
		} else if err != nil {
			return err
		}
		if prev != in.Status && !CanTransition(prev, in.Status) {
			return httpx.Unprocessable("invalid_transition", fmt.Sprintf("cannot move a round from %s to %s", prev, in.Status))
		}

		var version int
		if err := tx.QueryRow(ctx, `
			INSERT INTO application_round_status (application_id, round_id, status, note, actor_user_id)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (application_id, round_id) DO UPDATE
			  SET status = $3, note = $4, actor_user_id = $5, version = application_round_status.version + 1, updated_at = now()
			RETURNING version`, appID, roundID, in.Status, in.Note, userID).Scan(&version); err != nil {
			return err
		}

		return s.notifyRoundStatus(ctx, tx, studentID, jobID, appID, roundID, version, in.Status, company, role, roundName, roundSeq, in.Note,
			mode, location, instructions, scheduledAt, durationMinutes)
	})
	if err != nil {
		return nil, err
	}
	progress, err := progressForApp(ctx, s.pool, jobID, appID)
	if err != nil {
		return nil, err
	}
	for i := range progress {
		if progress[i].ID == roundID {
			return &progress[i], nil
		}
	}
	return nil, httpx.NotFound("round")
}

// ---- notifications ----------------------------------------------------------

func (s *Service) notifyRoundStatus(ctx context.Context, tx pgx.Tx, studentID, jobID, appID, roundID uuid.UUID,
	version int, status, company, role, roundName string, roundSeq int, note string,
	mode, location, instructions string, scheduledAt *time.Time, durationMinutes int) error {

	spec := notify.Spec{
		Link:      "/students/applications",
		Data:      map[string]any{"applicationId": appID, "roundId": roundID, "status": status},
		DedupeKey: fmt.Sprintf("round_status:%s:%d", roundID, version),
	}
	when := roundWhen(scheduledAt, s.loc)
	switch status {
	case domain.RoundScheduled:
		spec.Type = domain.NotifRoundScheduled
		spec.Title = fmt.Sprintf("%s scheduled: %s at %s", roundName, role, company)
		spec.Body = "Check your application for the date, time and details."
		spec.Email = &email.Content{CompanyName: company, JobRole: role, RoundName: roundName,
			When: when, Mode: mode, Location: location, DurationMinutes: durationMinutes, Instructions: instructions, Note: note}
	case domain.RoundCleared:
		spec.Type = domain.NotifRoundCleared
		spec.Title = fmt.Sprintf("Cleared: %s", roundName)
		var nextName string
		err := tx.QueryRow(ctx, `SELECT name FROM job_rounds WHERE job_id = $1 AND seq = $2`, jobID, roundSeq+1).Scan(&nextName)
		switch {
		case db.IsNoRows(err):
			spec.Body = fmt.Sprintf("You've cleared the %s for %s at %s. Selection process complete — awaiting the final decision.", roundName, role, company)
		case err != nil:
			return err
		default:
			spec.Body = fmt.Sprintf("You've cleared the %s for %s at %s. Next: %s.", roundName, role, company, nextName)
		}
		spec.Email = &email.Content{CompanyName: company, JobRole: role, RoundName: roundName,
			ApplicationStatus: "Cleared", NextRoundName: nextName, Note: note}
	case domain.RoundRejected:
		spec.Type = domain.NotifRoundRejected
		spec.Title = fmt.Sprintf("Update on %s at %s", role, company)
		spec.Body = fmt.Sprintf("%s will not be moving you forward from the %s.", company, roundName)
		spec.Email = &email.Content{CompanyName: company, JobRole: role, RoundName: roundName,
			ApplicationStatus: "Rejected", Note: note}
	default: // Upcoming: nothing to announce
		return nil
	}
	if note != "" {
		spec.Body += " " + note
	}
	uid, err := userIDFor(ctx, tx, studentID)
	if err != nil {
		return err
	}
	_, err = s.notifier.ToUser(ctx, tx, uid, spec)
	return err
}

// roundWhen formats a round's schedule for both push body and email content;
// "" means not yet scheduled (each renders its own "to be announced" wording).
func roundWhen(scheduledAt *time.Time, loc *time.Location) string {
	if scheduledAt == nil {
		return ""
	}
	t := scheduledAt.In(loc)
	return fmt.Sprintf("%s at %s", t.Format("2 Jan"), t.Format("3:04 PM"))
}

// notifyReschedule tells every candidate already tracked against a
// (now-locked) round that its logistics changed.
func (s *Service) notifyReschedule(ctx context.Context, tx pgx.Tx, roundID uuid.UUID) error {
	var name, mode, location, role, company, instructions string
	var scheduledAt *time.Time
	var durationMinutes, version int
	if err := tx.QueryRow(ctx, `
		SELECT jr.name, jr.mode, jr.scheduled_at, jr.location, jr.duration_minutes, jr.instructions, jr.version, j.role, c.name
		  FROM job_rounds jr JOIN jobs j ON j.id = jr.job_id JOIN companies c ON c.id = j.company_id
		 WHERE jr.id = $1`, roundID).
		Scan(&name, &mode, &scheduledAt, &location, &durationMinutes, &instructions, &version, &role, &company); err != nil {
		return err
	}
	when := roundWhen(scheduledAt, s.loc)
	body := fmt.Sprintf("Now %s · %s%s", cmp.Or(when, "a date to be announced"), mode, mapNonEmpty(location))
	_, err := s.notifier.ToUsers(ctx, tx, `
		SELECT DISTINCT st.user_id FROM application_round_status ars
		  JOIN applications a ON a.id = ars.application_id
		  JOIN students st ON st.id = a.student_id
		 WHERE ars.round_id = $1`, []any{roundID}, notify.Spec{
		Type:      domain.NotifRoundUpdated,
		Title:     fmt.Sprintf("Round updated: %s", name),
		Body:      body,
		Link:      "/students/applications",
		DedupeKey: fmt.Sprintf("round_update:%s:%d", roundID, version),
		Email: &email.Content{CompanyName: company, JobRole: role, RoundName: name,
			When: when, Mode: mode, Location: location, DurationMinutes: durationMinutes, Instructions: instructions},
	})
	return err
}

func mapNonEmpty(s string) string {
	if s == "" {
		return ""
	}
	return " · " + s
}

func userIDFor(ctx context.Context, q db.DBTX, studentID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT user_id FROM students WHERE id = $1`, studentID).Scan(&id)
	return id, err
}

// ---- scheduler hook ---------------------------------------------------------

type reminderCandidate struct {
	userID                        uuid.UUID
	roundID                       uuid.UUID
	appID                         uuid.UUID
	roundName, company, role      string
	mode, location, instructions  string
	scheduledAt                   *time.Time
	durationMinutes, roundVersion int
}

// SendReminders notifies every candidate with a Scheduled round starting
// within one of s.LeadTimes. It follows job.Service.SendDeadlineReminders's
// windowed, dedupe-keyed approach exactly: a lead time only fires once the
// round has crossed into its window (scheduled_at - lead <= now) and hasn't
// happened yet (scheduled_at > now); the third clause guards against sending
// a reminder the instant a round is scheduled/rescheduled to a time that's
// already inside the window (mirroring SendDeadlineReminders's j.approved_at
// check). The dedupe key uses job_rounds.version (bumped on reschedule), not
// application_round_status's, so rescheduling re-opens reminder eligibility
// for the new time instead of being permanently suppressed by a reminder
// already sent for the old one.
func (s *Service) SendReminders(ctx context.Context) (int64, error) {
	var total int64
	now := s.Now()
	for _, lead := range s.LeadTimes {
		rows, err := s.pool.Query(ctx, `
			SELECT st.user_id, jr.id, a.id, jr.name, c.name, j.role,
			       jr.mode, jr.location, jr.instructions, jr.scheduled_at, jr.duration_minutes, jr.version
			  FROM application_round_status ars
			  JOIN job_rounds jr ON jr.id = ars.round_id
			  JOIN applications a ON a.id = ars.application_id
			  JOIN jobs j ON j.id = a.job_id
			  JOIN companies c ON c.id = j.company_id
			  JOIN students st ON st.id = a.student_id
			 WHERE ars.status = 'Scheduled'
			   AND jr.scheduled_at IS NOT NULL
			   AND jr.scheduled_at - make_interval(secs => $1) <= $2
			   AND jr.scheduled_at > $2
			   AND GREATEST(ars.updated_at, jr.updated_at) <= jr.scheduled_at - make_interval(secs => $1)`,
			lead.Seconds(), now)
		if err != nil {
			return total, err
		}
		var cands []reminderCandidate
		for rows.Next() {
			var c reminderCandidate
			if err := rows.Scan(&c.userID, &c.roundID, &c.appID, &c.roundName, &c.company, &c.role,
				&c.mode, &c.location, &c.instructions, &c.scheduledAt, &c.durationMinutes, &c.roundVersion); err != nil {
				rows.Close()
				return total, err
			}
			cands = append(cands, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, err
		}

		for _, c := range cands {
			when := roundWhen(c.scheduledAt, s.loc)
			ok, err := s.notifier.ToUser(ctx, s.pool, c.userID, notify.Spec{
				Type:      domain.NotifRoundReminder,
				Title:     fmt.Sprintf("Reminder: %s at %s", c.roundName, c.company),
				Body:      fmt.Sprintf("%s starts %s.", c.roundName, when),
				Link:      "/students/applications",
				Data:      map[string]any{"applicationId": c.appID, "roundId": c.roundID},
				DedupeKey: fmt.Sprintf("round_reminder_%s:%s:%d", leadLabel(lead), c.roundID, c.roundVersion),
				Email: &email.Content{CompanyName: c.company, JobRole: c.role, RoundName: c.roundName,
					When: when, Mode: c.mode, Location: c.location, DurationMinutes: c.durationMinutes, Instructions: c.instructions},
			})
			if err != nil {
				return total, err
			}
			if ok {
				total++
			}
		}
	}
	return total, nil
}

// leadLabel turns a lead time into a short, stable dedupe-key token.
func leadLabel(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	}
	return fmt.Sprintf("%dm", int64(d/time.Minute))
}
