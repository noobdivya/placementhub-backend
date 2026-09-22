package application

import (
	"testing"

	"placementhub/internal/domain"
)

func TestCanTransition(t *testing.T) {
	allowed := map[[2]string]bool{
		{domain.StageApplied, domain.StageShortlisted}:   true,
		{domain.StageApplied, domain.StageRejected}:      true,
		{domain.StageShortlisted, domain.StageInterview}: true,
		{domain.StageShortlisted, domain.StageRejected}:  true,
		{domain.StageShortlisted, domain.StageApplied}:   true,
		{domain.StageInterview, domain.StageOffered}:     true,
		{domain.StageInterview, domain.StageRejected}:    true,
		{domain.StageInterview, domain.StageApplied}:     true,
		{domain.StageOffered, domain.StageRejected}:      true,
		{domain.StageOffered, domain.StageApplied}:       true,
		{domain.StageRejected, domain.StageApplied}:      true,
	}
	for _, from := range domain.Stages {
		for _, to := range domain.Stages {
			if got, want := CanTransition(from, to), allowed[[2]string{from, to}]; got != want {
				t.Errorf("CanTransition(%s -> %s) = %v, want %v", from, to, got, want)
			}
		}
	}
	// Withdrawn is terminal for recruiters, and never a target.
	for _, to := range domain.Stages {
		if CanTransition(domain.StageWithdrawn, to) || CanTransition(to, domain.StageWithdrawn) {
			t.Errorf("Withdrawn must be unreachable and terminal (%s)", to)
		}
	}
	// No skipping straight to an interview or an offer.
	if CanTransition(domain.StageApplied, domain.StageInterview) || CanTransition(domain.StageApplied, domain.StageOffered) ||
		CanTransition(domain.StageShortlisted, domain.StageOffered) {
		t.Error("stages may not be skipped")
	}
}
