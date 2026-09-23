package round

import (
	"testing"

	"placementhub/internal/httpx"
)

func validItems() []RoundInput {
	return []RoundInput{
		{Name: "Aptitude Test", Mode: "Online", DurationMinutes: 60},
		{Name: "Technical Interview", Mode: "Offline", DurationMinutes: 45, Location: "Block A, Room 3"},
	}
}

func TestNormaliseAndValidateItems(t *testing.T) {
	items, err := normaliseAndValidateItems(validItems())
	if err != nil {
		t.Fatalf("valid items rejected: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}

	if _, err := normaliseAndValidateItems(nil); err == nil {
		t.Error("an empty round list was accepted")
	} else if he, ok := err.(*httpx.Error); !ok || he.Fields["rounds"] == "" {
		t.Errorf("empty list error = %v, want a rounds field error", err)
	}

	mut := map[string]func([]RoundInput) []RoundInput{
		"rounds[0].name": func(it []RoundInput) []RoundInput { it[0].Name = "x"; return it },
		"rounds[0].mode": func(it []RoundInput) []RoundInput { it[0].Mode = "Hybrid"; return it },
		"rounds[0].durationMinutes": func(it []RoundInput) []RoundInput {
			it[0].DurationMinutes = 1
			return it
		},
		"rounds[0].scheduledAt": func(it []RoundInput) []RoundInput {
			bad := "not-a-date"
			it[0].ScheduledAt = &bad
			return it
		},
		"rounds[1].name": func(it []RoundInput) []RoundInput { it[1].Name = ""; return it },
	}
	for field, f := range mut {
		bad := f(validItems())
		_, err := normaliseAndValidateItems(bad)
		he, ok := err.(*httpx.Error)
		if !ok || he.Fields[field] == "" {
			t.Errorf("%s: err = %v", field, err)
		}
	}
}

func TestNormaliseTrimsAndDefaultsDuration(t *testing.T) {
	items := []RoundInput{{Name: "  Aptitude Test  ", Mode: "Online", DurationMinutes: 0, Location: "  ", Instructions: " Bring a laptop "}}
	out, err := normaliseAndValidateItems(items)
	if err != nil {
		t.Fatalf("valid item rejected: %v", err)
	}
	if out[0].Name != "Aptitude Test" {
		t.Errorf("name = %q, want trimmed", out[0].Name)
	}
	if out[0].DurationMinutes != 60 {
		t.Errorf("duration = %d, want default 60", out[0].DurationMinutes)
	}
	if out[0].Instructions != "Bring a laptop" {
		t.Errorf("instructions = %q, want trimmed", out[0].Instructions)
	}
}

func TestNormaliseParsesScheduledAt(t *testing.T) {
	at := "2026-10-05T10:00:00Z"
	items := []RoundInput{{Name: "Aptitude Test", Mode: "Online", DurationMinutes: 60, ScheduledAt: &at}}
	out, err := normaliseAndValidateItems(items)
	if err != nil {
		t.Fatalf("valid item rejected: %v", err)
	}
	if out[0].scheduledAt == nil || out[0].scheduledAt.Year() != 2026 {
		t.Errorf("scheduledAt not parsed: %+v", out[0].scheduledAt)
	}

	// A nil or empty scheduledAt is fine — many rounds aren't dated yet.
	items[0].ScheduledAt = nil
	if out, err := normaliseAndValidateItems(items); err != nil || out[0].scheduledAt != nil {
		t.Errorf("nil scheduledAt should pass through as nil: out=%+v err=%v", out, err)
	}
}
