package email

import (
	"strings"
	"testing"
)

func TestSubjectForEachEventType(t *testing.T) {
	c := Content{CompanyName: "Nimbus Labs", RoundName: "Aptitude Test"}
	cases := []struct {
		notifType string
		content   Content
		want      string
	}{
		{"round_scheduled", c, "Aptitude Test Scheduled – Nimbus Labs"},
		{"round_updated", c, "Aptitude Test Rescheduled – Nimbus Labs"},
		{"round_cleared", c, "Aptitude Test Cleared – Nimbus Labs"},
		{"round_rejected", c, "Update on Aptitude Test – Nimbus Labs"},
		{"round_reminder", c, "Reminder: Aptitude Test Starts Soon – Nimbus Labs"},
		{"interview", Content{CompanyName: "Nimbus Labs"}, "Interview Stage – Nimbus Labs"},
		{"offer", Content{CompanyName: "Nimbus Labs"}, "Offer Extended – Nimbus Labs"},
		{"placement", Content{CompanyName: "Nimbus Labs"}, "You're Placed at Nimbus Labs!"},
		{"application_update", Content{CompanyName: "Nimbus Labs", ApplicationStatus: "Shortlisted"}, "Shortlisted – Nimbus Labs"},
		{"application_update", Content{CompanyName: "Nimbus Labs", ApplicationStatus: "Rejected"}, "Application Update – Nimbus Labs"},
		{"something_unknown", Content{CompanyName: "Nimbus Labs"}, "Update from Nimbus Labs"},
	}
	for _, tc := range cases {
		got := subjectFor(tc.notifType, tc.content)
		if got != tc.want {
			t.Errorf("subjectFor(%q) = %q, want %q", tc.notifType, got, tc.want)
		}
	}
}

func TestRenderIncludesRoundDetailsWhenPresent(t *testing.T) {
	c := Content{
		CompanyName: "Nimbus Labs", JobRole: "SDE Intern",
		RoundName: "Aptitude Test", When: "23 Sep at 3:04 PM", Mode: "Online",
		Location: "https://meet.example.com/abc", DurationMinutes: 60,
		Instructions: "Bring a laptop",
	}
	subject, html, text := Render("round_scheduled", c, "https://app.test/students/applications/1")

	if subject != "Aptitude Test Scheduled – Nimbus Labs" {
		t.Fatalf("subject = %q", subject)
	}
	for _, want := range []string{"Aptitude Test", "23 Sep at 3:04 PM", "Online", "https://meet.example.com/abc", "60 minutes", "Bring a laptop"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
		if !strings.Contains(html, want) {
			t.Errorf("html missing %q:\n%s", want, html)
		}
	}
	// Online mode uses "Meeting link" as the row label, not "Venue".
	if !strings.Contains(html, "Meeting link") {
		t.Errorf("html should label the location as Meeting link for an Online round:\n%s", html)
	}
	if !strings.Contains(html, `href="https://app.test/students/applications/1"`) {
		t.Errorf("html missing action link:\n%s", html)
	}
}

func TestRenderOfflineRoundUsesVenueLabel(t *testing.T) {
	c := Content{CompanyName: "Nimbus Labs", RoundName: "Onsite Interview", Mode: "Offline", Location: "Block C, Room 4"}
	_, html, _ := Render("round_scheduled", c, "https://app.test/x")
	if !strings.Contains(html, "Venue") {
		t.Errorf("html should label the location as Venue for an Offline round:\n%s", html)
	}
}

func TestRenderOmitsRoundDateWhenNotScheduled(t *testing.T) {
	c := Content{CompanyName: "Nimbus Labs", RoundName: "HR Round"}
	_, html, text := Render("round_scheduled", c, "https://app.test/x")
	if !strings.Contains(text, "date to be announced") {
		t.Errorf("text missing fallback when-text:\n%s", text)
	}
	if !strings.Contains(html, "Date to be announced") {
		t.Errorf("html missing fallback when-text:\n%s", html)
	}
}

func TestRenderIncludesOfferDetails(t *testing.T) {
	c := Content{CompanyName: "Nimbus Labs", JobRole: "SDE Intern", ApplicationStatus: "Offered", OfferCTC: 12.5, OfferValidUntil: "30 Sep"}
	_, html, text := Render("offer", c, "https://app.test/x")
	for _, want := range []string{"Offered", "12.5", "30 Sep"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
		if !strings.Contains(html, want) {
			t.Errorf("html missing %q:\n%s", want, html)
		}
	}
}

func TestRenderIncludesNextRoundOnClear(t *testing.T) {
	c := Content{CompanyName: "Nimbus Labs", RoundName: "Aptitude Test", NextRoundName: "Technical Interview"}
	_, html, text := Render("round_cleared", c, "https://app.test/x")
	if !strings.Contains(text, "Technical Interview") || !strings.Contains(html, "Technical Interview") {
		t.Errorf("expected next round name to appear;\ntext=%s\nhtml=%s", text, html)
	}
}

// The recruiter enters round names, instructions, and stage notes as free
// text, so Render must never let it break out of the HTML it's embedded in.
func TestRenderEscapesRecruiterEnteredText(t *testing.T) {
	c := Content{
		CompanyName:  `Nimbus <script>alert(1)</script> Labs`,
		RoundName:    `HR & Culture "Fit"`,
		Instructions: `Bring your <laptop> & ID`,
		Note:         `We're excited & can't wait to see you!`,
	}
	_, html, _ := Render("round_scheduled", c, "https://app.test/x")

	for _, mustNot := range []string{"<script>", "<laptop>"} {
		if strings.Contains(html, mustNot) {
			t.Errorf("html should not contain unescaped %q:\n%s", mustNot, html)
		}
	}
	for _, want := range []string{"&lt;script&gt;", "&amp;", "&#34;Fit&#34;", "&lt;laptop&gt;"} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing escaped %q:\n%s", want, html)
		}
	}
}

func TestRenderEscapesActionURL(t *testing.T) {
	c := Content{CompanyName: "Nimbus Labs"}
	_, html, _ := Render("offer", c, `https://app.test/x?a=1&b="2"`)
	if !strings.Contains(html, "&amp;b=") {
		t.Errorf("action URL should be escaped in the href:\n%s", html)
	}
}
