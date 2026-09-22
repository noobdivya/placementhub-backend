package application

import (
	"context"
	"fmt"
	"time"

	"placementhub/internal/audit"
	"placementhub/internal/db"
	"placementhub/internal/domain"
	"placementhub/internal/httpx"
	"placementhub/internal/notify"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Offer is an offer as the student sees it.
type Offer struct {
	ID            uuid.UUID  `json:"id"`
	ApplicationID uuid.UUID  `json:"applicationId"`
	JobID         uuid.UUID  `json:"jobId"`
	Company       string     `json:"company"`
	Color         string     `json:"color"`
	Role          string     `json:"role"`
	CTC           float64    `json:"ctc"`
	ValidUntil    time.Time  `json:"validUntil"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"createdAt"`
	RespondedAt   *time.Time `json:"respondedAt"`
}

const offerSelect = `
SELECT o.id, o.application_id, o.job_id, c.name, c.color, j.role, o.ctc, o.valid_until, o.status, o.created_at, o.responded_at
  FROM offers o JOIN jobs j ON j.id = o.job_id JOIN companies c ON c.id = o.company_id`

func scanOffer(row pgx.Row) (*Offer, error) {
	var o Offer
	err := row.Scan(&o.ID, &o.ApplicationID, &o.JobID, &o.Company, &o.Color, &o.Role, &o.CTC, &o.ValidUntil,
		&o.Status, &o.CreatedAt, &o.RespondedAt)
	return &o, err
}

func (s *Service) ListOffers(ctx context.Context, userID uuid.UUID) ([]Offer, error) {
	sid, err := s.studentIDByUser(ctx, s.pool, userID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, offerSelect+` WHERE o.student_id = $1 ORDER BY o.created_at DESC, o.id`, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Offer{}
	for rows.Next() {
		o, err := scanOffer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// AcceptResult is what the student gets back: the accepted offer and what
// changed as a consequence of becoming placed.
type AcceptResult struct {
	Offer                 *Offer `json:"offer"`
	Status                string `json:"status"` // always "Placed"
	ApplicationsWithdrawn int    `json:"applicationsWithdrawn"`
	OffersDeclined        int    `json:"offersDeclined"`
	DriveRegistrationsCut int    `json:"driveRegistrationsCancelled"`
}

// Accept makes the student PLACED.
//
// In one transaction, holding the student row lock:
//   - the offer becomes Accepted (a unique index allows only one per student);
//   - the student's other live applications become Withdrawn (kept in history);
//   - their other pending offers are declined;
//   - their registrations for drives that have not started are cancelled.
//
// From then on applying to jobs or registering for drives is refused, both in
// the services and by a database trigger.
func (s *Service) Accept(ctx context.Context, userID, offerID uuid.UUID) (*AcceptResult, error) {
	res := &AcceptResult{Status: domain.StudentPlaced}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		sid, err := s.studentIDByUser(ctx, tx, userID)
		if err != nil {
			return err
		}
		if err := lockStudent(ctx, tx, sid); err != nil {
			return err
		}

		var (
			appID      uuid.UUID
			status     string
			validUntil time.Time
			company    string
			role       string
		)
		err = tx.QueryRow(ctx,
			`SELECT o.application_id, o.status, o.valid_until, c.name, j.role
			   FROM offers o JOIN jobs j ON j.id = o.job_id JOIN companies c ON c.id = o.company_id
			  WHERE o.id = $1 AND o.student_id = $2 FOR UPDATE OF o`, offerID, sid).
			Scan(&appID, &status, &validUntil, &company, &role)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("offer")
			}
			return err
		}
		switch status {
		case domain.OfferAccepted:
			return nil // idempotent: accepting twice is not an error
		case domain.OfferPending:
		default:
			return httpx.Conflict("offer_not_pending", fmt.Sprintf("this offer is %s and can no longer be accepted", status))
		}
		if !s.Now().Before(validUntil) {
			return httpx.Conflict("offer_expired", "this offer has expired")
		}
		if placed, err := isPlaced(ctx, tx, sid); err != nil {
			return err
		} else if placed {
			return httpx.Conflict("already_placed", "You have already accepted another offer.")
		}

		if _, err := tx.Exec(ctx, `UPDATE offers SET status = 'Accepted', responded_at = now() WHERE id = $1`, offerID); err != nil {
			return err
		}
		if _, err := addEvent(ctx, tx, appID, ptr(domain.StageOffered), domain.StageOffered, &userID, "Offer accepted"); err != nil {
			return err
		}

		// Other pending offers: declined, and their applications withdrawn.
		declined, err := tx.Query(ctx,
			`UPDATE offers SET status = 'Declined', decline_reason = 'Student accepted another offer', responded_at = now()
			  WHERE student_id = $1 AND status = 'Pending' AND id <> $2 RETURNING application_id`, sid, offerID)
		if err != nil {
			return err
		}
		var declinedApps []uuid.UUID
		for declined.Next() {
			var a uuid.UUID
			if err := declined.Scan(&a); err != nil {
				declined.Close()
				return err
			}
			declinedApps = append(declinedApps, a)
		}
		declined.Close()
		if err := declined.Err(); err != nil {
			return err
		}
		res.OffersDeclined = len(declinedApps)

		// Every other live application is withdrawn but stays in history.
		withdrawn, err := tx.Query(ctx,
			`SELECT id, stage FROM applications
			  WHERE student_id = $1 AND id <> $2 AND stage IN ('Applied', 'Shortlisted', 'Interview', 'Offered')
			  ORDER BY id FOR UPDATE`, sid, appID)
		if err != nil {
			return err
		}
		type wd struct {
			id   uuid.UUID
			from string
		}
		var wds []wd
		for withdrawn.Next() {
			var w wd
			if err := withdrawn.Scan(&w.id, &w.from); err != nil {
				withdrawn.Close()
				return err
			}
			wds = append(wds, w)
		}
		withdrawn.Close()
		if err := withdrawn.Err(); err != nil {
			return err
		}
		for _, w := range wds {
			if _, err := tx.Exec(ctx, `UPDATE applications SET stage = 'Withdrawn', updated_at = now() WHERE id = $1`, w.id); err != nil {
				return err
			}
			if _, err := addEvent(ctx, tx, w.id, ptr(w.from), domain.StageWithdrawn, &userID, "Student accepted another offer"); err != nil {
				return err
			}
		}
		res.ApplicationsWithdrawn = len(wds)

		tag, err := tx.Exec(ctx,
			`DELETE FROM drive_registrations dr USING drives d
			  WHERE dr.student_id = $1 AND d.id = dr.drive_id AND d.starts_at > now()`, sid)
		if err != nil {
			return err
		}
		res.DriveRegistrationsCut = int(tag.RowsAffected())

		_, err = s.notifier.ToUser(ctx, tx, userID, notify.Spec{
			Type:      domain.NotifPlacementFinal,
			Title:     fmt.Sprintf("You're placed at %s!", company),
			Body:      fmt.Sprintf("Congratulations on accepting the %s offer. Your other applications have been withdrawn.", role),
			Link:      "/students/applications",
			Data:      map[string]any{"offerId": offerID},
			DedupeKey: "placement:" + offerID.String(),
		})
		return err
	})
	if err != nil {
		if db.PgCode(err) == db.CodeUniqueViolation && db.PgConstraint(err) == "offers_one_accepted_per_student" {
			return nil, httpx.Conflict("already_placed", "You have already accepted another offer.")
		}
		return nil, err
	}
	o, err := scanOffer(s.pool.QueryRow(ctx, offerSelect+` WHERE o.id = $1`, offerID))
	if err != nil {
		return nil, err
	}
	res.Offer = o
	return res, nil
}

func ptr[T any](v T) *T { return &v }

// Decline turns an offer down. The application is recorded as withdrawn.
func (s *Service) Decline(ctx context.Context, userID, offerID uuid.UUID, reason string) (*Offer, error) {
	var v httpx.V
	v.Text("reason", reason, 0, 300)
	if err := v.Err(); err != nil {
		return nil, err
	}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		sid, err := s.studentIDByUser(ctx, tx, userID)
		if err != nil {
			return err
		}
		if err := lockStudent(ctx, tx, sid); err != nil {
			return err
		}
		var appID uuid.UUID
		var status string
		err = tx.QueryRow(ctx, `SELECT application_id, status FROM offers WHERE id = $1 AND student_id = $2 FOR UPDATE`, offerID, sid).
			Scan(&appID, &status)
		if err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("offer")
			}
			return err
		}
		if status != domain.OfferPending {
			return httpx.Conflict("offer_not_pending", fmt.Sprintf("this offer is %s and can no longer be declined", status))
		}
		if _, err := tx.Exec(ctx,
			`UPDATE offers SET status = 'Declined', decline_reason = $2, responded_at = now() WHERE id = $1`, offerID, reason); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE applications SET stage = 'Withdrawn', updated_at = now() WHERE id = $1`, appID); err != nil {
			return err
		}
		_, err = addEvent(ctx, tx, appID, ptr(domain.StageOffered), domain.StageWithdrawn, &userID, "Offer declined by student")
		return err
	})
	if err != nil {
		return nil, err
	}
	return scanOffer(s.pool.QueryRow(ctx, offerSelect+` WHERE o.id = $1`, offerID))
}

// RevokePlacement lets the placement cell rescind an accepted offer (for
// example when the company withdraws it). The student stops being placed and
// may apply again; applications withdrawn at acceptance stay withdrawn.
func (s *Service) RevokePlacement(ctx context.Context, offerID uuid.UUID, reason string, actor uuid.UUID, ip string) (*Offer, error) {
	var v httpx.V
	v.Text("reason", reason, 3, 500)
	if err := v.Err(); err != nil {
		return nil, err
	}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var sid, appID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT student_id, application_id FROM offers WHERE id = $1`, offerID).Scan(&sid, &appID); err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("offer")
			}
			return err
		}
		if err := lockStudent(ctx, tx, sid); err != nil {
			return err
		}
		var status, company string
		if err := tx.QueryRow(ctx,
			`SELECT o.status, c.name FROM offers o JOIN companies c ON c.id = o.company_id WHERE o.id = $1 FOR UPDATE OF o`, offerID).
			Scan(&status, &company); err != nil {
			return err
		}
		if status != domain.OfferAccepted {
			return httpx.Conflict("bad_state", fmt.Sprintf("only accepted offers can be revoked, this one is %s", status))
		}
		if _, err := tx.Exec(ctx, `UPDATE offers SET status = 'Rescinded', responded_at = now(), decline_reason = $2 WHERE id = $1`, offerID, reason); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE applications SET stage = 'Rejected', updated_at = now() WHERE id = $1`, appID); err != nil {
			return err
		}
		if _, err := addEvent(ctx, tx, appID, ptr(domain.StageOffered), domain.StageRejected, &actor, "Placement revoked by placement cell: "+reason); err != nil {
			return err
		}
		uid, err := userIDOf(ctx, tx, sid)
		if err != nil {
			return err
		}
		_, err = s.notifier.ToUser(ctx, tx, uid, notify.Spec{
			Type:      domain.NotifOffer,
			Title:     fmt.Sprintf("Your placement at %s was revoked", company),
			Body:      "The placement cell has revoked this placement. You can apply to jobs again.",
			Link:      "/students/applications",
			DedupeKey: "placement_revoked:" + offerID.String(),
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	audit.Log(ctx, s.pool, &actor, "offer.revoked", "offer", offerID.String(), ip, map[string]any{"reason": reason})
	return scanOffer(s.pool.QueryRow(ctx, offerSelect+` WHERE o.id = $1`, offerID))
}

// ---- scheduler hooks ------------------------------------------------------

// ExpireOffers marks pending offers past their deadline as Expired, records
// the application as rejected, and tells the student.
func (s *Service) ExpireOffers(ctx context.Context) (int, error) {
	type exp struct {
		offerID, appID, sid uuid.UUID
		company, role       string
	}
	rows, err := s.pool.Query(ctx,
		`SELECT o.id, o.application_id, o.student_id, c.name, j.role
		   FROM offers o JOIN jobs j ON j.id = o.job_id JOIN companies c ON c.id = o.company_id
		  WHERE o.status = 'Pending' AND o.valid_until <= $1`, s.Now())
	if err != nil {
		return 0, err
	}
	var due []exp
	for rows.Next() {
		var e exp
		if err := rows.Scan(&e.offerID, &e.appID, &e.sid, &e.company, &e.role); err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	for _, e := range due {
		err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			// The WHERE re-checks status under the row lock, so an offer accepted
			// a moment ago is left alone.
			tag, err := tx.Exec(ctx,
				`UPDATE offers SET status = 'Expired', responded_at = now() WHERE id = $1 AND status = 'Pending' AND valid_until <= $2`,
				e.offerID, s.Now())
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE applications SET stage = 'Rejected', updated_at = now() WHERE id = $1 AND stage = 'Offered'`, e.appID); err != nil {
				return err
			}
			if _, err := addEvent(ctx, tx, e.appID, ptr(domain.StageOffered), domain.StageRejected, nil, "Offer expired"); err != nil {
				return err
			}
			uid, err := userIDOf(ctx, tx, e.sid)
			if err != nil {
				return err
			}
			_, err = s.notifier.ToUser(ctx, tx, uid, notify.Spec{
				Type:      domain.NotifOfferExpired,
				Title:     fmt.Sprintf("Offer from %s expired", e.company),
				Body:      fmt.Sprintf("Your %s offer was not accepted in time and has expired.", e.role),
				Link:      "/students/applications",
				DedupeKey: "offer_expired:" + e.offerID.String(),
			})
			n++
			return err
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// SendOfferReminders warns students whose pending offer expires within 24 hours.
func (s *Service) SendOfferReminders(ctx context.Context) (int, error) {
	now := s.Now()
	rows, err := s.pool.Query(ctx,
		`SELECT o.id, s.user_id, c.name, j.role, o.valid_until
		   FROM offers o JOIN jobs j ON j.id = o.job_id JOIN companies c ON c.id = o.company_id JOIN students s ON s.id = o.student_id
		  WHERE o.status = 'Pending' AND o.valid_until > $1 AND o.valid_until <= $2`, now, now.Add(24*time.Hour))
	if err != nil {
		return 0, err
	}
	type rem struct {
		id, uid       uuid.UUID
		company, role string
		until         time.Time
	}
	var list []rem
	for rows.Next() {
		var r rem
		if err := rows.Scan(&r.id, &r.uid, &r.company, &r.role, &r.until); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	sent := 0
	for _, r := range list {
		ok, err := s.notifier.ToUser(ctx, s.pool, r.uid, notify.Spec{
			Type:      domain.NotifOfferExpiring,
			Title:     fmt.Sprintf("Offer from %s expires soon", r.company),
			Body:      fmt.Sprintf("Respond to the %s offer before %s.", r.role, r.until.In(s.loc).Format("2 Jan, 3:04 PM")),
			Link:      "/students/applications",
			Data:      map[string]any{"offerId": r.id},
			DedupeKey: "offer_expiring:" + r.id.String(),
		})
		if err != nil {
			return sent, err
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}
