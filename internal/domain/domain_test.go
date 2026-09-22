package domain

import "testing"

func TestCategoryFor(t *testing.T) {
	cases := map[string]string{
		NotifNewJob: CatNewJob, NotifStageUpdate: CatApplications, NotifDeadline: CatDeadlines,
		NotifDriveNew: CatDrives, NotifDriveUpdated: CatDrives, NotifDriveReminder: CatDrives, NotifNotice: CatNotices,
		// Interviews, offers and placement events are critical and cannot be muted per category.
		NotifInterview: CatCritical, NotifOffer: CatCritical, NotifOfferExpiring: CatCritical,
		NotifOfferExpired: CatCritical, NotifPlacementFinal: CatCritical,
	}
	for typ, want := range cases {
		if got := CategoryFor(typ); got != want {
			t.Errorf("CategoryFor(%q) = %q, want %q", typ, got, want)
		}
	}
	for _, c := range MutableCategories {
		if c == CatCritical {
			t.Error("critical must not be a mutable category")
		}
	}
}

func TestColorForIsStable(t *testing.T) {
	if ColorFor("Nimbus Labs") != ColorFor("Nimbus Labs") {
		t.Error("colour changes between calls")
	}
	if len(ColorFor("x")) != 7 || ColorFor("x")[0] != '#' {
		t.Errorf("not a hex colour: %s", ColorFor("x"))
	}
}

func TestValidBranch(t *testing.T) {
	for _, b := range Branches {
		if !ValidBranch(b) {
			t.Errorf("%s should be valid", b)
		}
	}
	for _, b := range []string{"", "cse", "Astrology", "CSE "} {
		if ValidBranch(b) {
			t.Errorf("%q should be invalid", b)
		}
	}
}
