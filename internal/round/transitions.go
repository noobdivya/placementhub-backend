package round

import (
	"slices"

	"placementhub/internal/domain"
)

// roundTransitions lists the moves a recruiter may make on one candidate's
// round status. Forward moves go one step at a time; a Cleared or Rejected
// round can be corrected back to Scheduled (e.g. a mis-click), mirroring
// application.CanTransition's "reset to Applied" allowance.
var roundTransitions = map[string][]string{
	domain.RoundUpcoming:  {domain.RoundScheduled, domain.RoundRejected},
	domain.RoundScheduled: {domain.RoundCleared, domain.RoundRejected},
	domain.RoundCleared:   {domain.RoundScheduled},
	domain.RoundRejected:  {domain.RoundScheduled},
}

// CanTransition reports whether a recruiter may move a round from one status to another.
func CanTransition(from, to string) bool { return slices.Contains(roundTransitions[from], to) }
