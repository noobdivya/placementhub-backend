package round

import (
	"testing"

	"placementhub/internal/domain"
)

func TestCanTransition(t *testing.T) {
	allowed := map[[2]string]bool{
		{domain.RoundUpcoming, domain.RoundScheduled}: true,
		{domain.RoundUpcoming, domain.RoundRejected}:  true,
		{domain.RoundScheduled, domain.RoundCleared}:  true,
		{domain.RoundScheduled, domain.RoundRejected}: true,
		{domain.RoundCleared, domain.RoundScheduled}:  true,
		{domain.RoundRejected, domain.RoundScheduled}: true,
	}
	for _, from := range domain.RoundStatuses {
		for _, to := range domain.RoundStatuses {
			if got, want := CanTransition(from, to), allowed[[2]string{from, to}]; got != want {
				t.Errorf("CanTransition(%s -> %s) = %v, want %v", from, to, got, want)
			}
		}
	}
	// No skipping straight from Upcoming to Cleared or Rejected to Cleared.
	if CanTransition(domain.RoundUpcoming, domain.RoundCleared) {
		t.Error("a round may not be cleared without first being scheduled")
	}
	if CanTransition(domain.RoundCleared, domain.RoundRejected) || CanTransition(domain.RoundRejected, domain.RoundCleared) {
		t.Error("a terminal round must go back through Scheduled before the other terminal state")
	}
}
