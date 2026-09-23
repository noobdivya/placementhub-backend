// Package round covers a company's selection rounds for a job (Aptitude Test,
// Coding Round, Technical Interview, ...) and each application's progress
// through them. It is purely additive to internal/application's 5-stage
// pipeline: a round timeline never changes an application's stage, creates an
// offer, or affects eligibility/placement. The recruiter still uses the
// existing PATCH .../applications/{id}/stage to make an offer once rounds are
// done.
package round

import (
	"fmt"
	"strings"
	"time"

	"placementhub/internal/domain"
	"placementhub/internal/httpx"

	"github.com/google/uuid"
)

// Round is one step in a job's selection process, as the company defined it.
type Round struct {
	ID              uuid.UUID  `json:"id"`
	Seq             int        `json:"seq"`
	Name            string     `json:"name"`
	Mode            string     `json:"mode"`
	ScheduledAt     *time.Time `json:"scheduledAt"`
	DurationMinutes int        `json:"durationMinutes"`
	Location        string     `json:"location"` // venue, or a meeting link when Mode is Online
	Instructions    string     `json:"instructions"`
	Locked          bool       `json:"locked,omitempty"` // company view only: a candidate has progressed past Upcoming
}

// Progress is a Round plus one application's status against it.
type Progress struct {
	Round
	Status    string    `json:"status"`
	Note      string    `json:"note"`
	Current   bool      `json:"current"` // the lowest-seq round not yet Cleared
	UpdatedAt time.Time `json:"updatedAt"`
}

// RoundInput is one round in a PUT .../rounds request body.
type RoundInput struct {
	ID              *uuid.UUID `json:"id"` // nil = new round
	Name            string     `json:"name"`
	Mode            string     `json:"mode"`
	ScheduledAt     *string    `json:"scheduledAt"` // RFC3339, nullable
	DurationMinutes int        `json:"durationMinutes"`
	Location        string     `json:"location"`
	Instructions    string     `json:"instructions"`

	scheduledAt *time.Time // filled by validate
}

// StatusChange is the PATCH .../rounds/{roundId} request body.
type StatusChange struct {
	Status string `json:"status"`
	Note   string `json:"note"`
}

// normaliseAndValidateItems trims every item, requires at least one round,
// and reports every field problem across the whole list in one 422 (mirrors
// job.Input.validate's style, keyed per index so the frontend's existing
// per-field error rendering extends naturally to "rounds[i].field").
func normaliseAndValidateItems(items []RoundInput) ([]RoundInput, error) {
	var v httpx.V
	if len(items) == 0 {
		v.Add("rounds", "define at least one round")
	}
	out := make([]RoundInput, len(items))
	for i, it := range items {
		it.Name = strings.TrimSpace(it.Name)
		it.Location = strings.TrimSpace(it.Location)
		it.Instructions = strings.TrimSpace(it.Instructions)
		if it.DurationMinutes == 0 {
			it.DurationMinutes = 60
		}
		v.Text(fmt.Sprintf("rounds[%d].name", i), it.Name, 2, 120)
		v.OneOf(fmt.Sprintf("rounds[%d].mode", i), it.Mode, domain.RoundModes...)
		v.Range(fmt.Sprintf("rounds[%d].durationMinutes", i), float64(it.DurationMinutes), 5, 24*60)
		v.Text(fmt.Sprintf("rounds[%d].location", i), it.Location, 0, 300)
		v.Text(fmt.Sprintf("rounds[%d].instructions", i), it.Instructions, 0, 2000)
		if it.ScheduledAt != nil && strings.TrimSpace(*it.ScheduledAt) != "" {
			t, err := time.Parse(time.RFC3339, *it.ScheduledAt)
			if err != nil {
				v.Add(fmt.Sprintf("rounds[%d].scheduledAt", i), "must be a valid date-time")
			} else {
				it.scheduledAt = &t
			}
		}
		out[i] = it
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
