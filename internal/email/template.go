package email

import (
	"fmt"
	"html"
	"strings"
)

// Content is the structured, event-specific information an email needs. All
// fields are optional; Render fills in only what applies to the event type.
// Callers populate it from data already in local scope — no extra query.
//
// Every field here can originate as recruiter-entered free text (a round
// name, its instructions, a stage-change note), so Render always HTML-escapes
// before embedding — never trust this content as pre-sanitised.
type Content struct {
	CompanyName, JobRole string

	// ApplicationStatus disambiguates notification types that cover more than
	// one outcome (e.g. "application_update" covers both Shortlisted and
	// Rejected).
	ApplicationStatus string // "Shortlisted" | "Interview" | "Offered" | "Rejected" | "Placed" | ""

	RoundName     string // empty for application-level events (shortlist/offer/rejection/placement)
	NextRoundName string // set only on a Cleared event when another round follows

	When            string // pre-formatted by the caller, e.g. "23 Sep at 3:04 PM"; "" = date to be announced
	Mode            string // "Online" | "Offline" | ""
	Location        string // venue, or a meeting link when Mode is Online
	DurationMinutes int    // 0 = omit

	Instructions string
	Note         string // the recruiter's free-text note, if any

	OfferCTC        float64 // > 0 only for an Offered email
	OfferValidUntil string  // pre-formatted, only for an Offered email
}

// Render turns Content into an email for one event type. It performs no I/O
// and is exhaustively covered by template_test.go.
func Render(notifType string, c Content, actionURL string) (subject, htmlBody, text string) {
	subject = subjectFor(notifType, c)
	text = renderText(subject, c, actionURL)
	htmlBody = renderHTML(subject, c, actionURL)
	return
}

func subjectFor(notifType string, c Content) string {
	switch notifType {
	case "round_scheduled":
		return fmt.Sprintf("%s Scheduled – %s", c.RoundName, c.CompanyName)
	case "round_updated":
		return fmt.Sprintf("%s Rescheduled – %s", c.RoundName, c.CompanyName)
	case "round_cleared":
		return fmt.Sprintf("%s Cleared – %s", c.RoundName, c.CompanyName)
	case "round_rejected":
		return fmt.Sprintf("Update on %s – %s", c.RoundName, c.CompanyName)
	case "round_reminder":
		return fmt.Sprintf("Reminder: %s Starts Soon – %s", c.RoundName, c.CompanyName)
	case "interview":
		return fmt.Sprintf("Interview Stage – %s", c.CompanyName)
	case "offer":
		return fmt.Sprintf("Offer Extended – %s", c.CompanyName)
	case "placement":
		return fmt.Sprintf("You're Placed at %s!", c.CompanyName)
	case "application_update":
		if c.ApplicationStatus == "Rejected" {
			return fmt.Sprintf("Application Update – %s", c.CompanyName)
		}
		return fmt.Sprintf("Shortlisted – %s", c.CompanyName)
	default:
		return fmt.Sprintf("Update from %s", c.CompanyName)
	}
}

func roundLabel(mode string) string {
	if mode == "Online" {
		return "Meeting link"
	}
	return "Venue"
}

func renderText(subject string, c Content, actionURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s — %s\n", subject, c.CompanyName, c.JobRole)
	if c.ApplicationStatus != "" {
		fmt.Fprintf(&b, "Status: %s\n", c.ApplicationStatus)
	}
	if c.RoundName != "" {
		fmt.Fprintf(&b, "\nRound: %s\n", c.RoundName)
		if c.When != "" {
			fmt.Fprintf(&b, "When: %s\n", c.When)
		} else {
			b.WriteString("When: date to be announced\n")
		}
		if c.Mode != "" {
			fmt.Fprintf(&b, "Mode: %s\n", c.Mode)
		}
		if c.Location != "" {
			fmt.Fprintf(&b, "%s: %s\n", roundLabel(c.Mode), c.Location)
		}
		if c.DurationMinutes > 0 {
			fmt.Fprintf(&b, "Duration: %d minutes\n", c.DurationMinutes)
		}
		if c.Instructions != "" {
			fmt.Fprintf(&b, "\nWhat to prepare / bring:\n%s\n", c.Instructions)
		}
	}
	if c.NextRoundName != "" {
		fmt.Fprintf(&b, "\nNext round: %s\n", c.NextRoundName)
	}
	if c.OfferCTC > 0 {
		fmt.Fprintf(&b, "\nOffer: ₹%.1f LPA. Respond by %s.\n", c.OfferCTC, c.OfferValidUntil)
	}
	if c.Note != "" {
		fmt.Fprintf(&b, "\nNote from %s: %s\n", c.CompanyName, c.Note)
	}
	fmt.Fprintf(&b, "\nView in PlacementHub: %s\n", actionURL)
	return b.String()
}

// e is shorthand for html.EscapeString — every piece of Content is
// recruiter-entered free text, so nothing here is trusted as pre-sanitised.
func e(s string) string { return html.EscapeString(s) }

func renderHTML(subject string, c Content, actionURL string) string {
	var b strings.Builder
	b.WriteString(`<div style="font-family:-apple-system,Segoe UI,Roboto,sans-serif;color:#1e293b;max-width:560px;margin:0 auto">`)
	fmt.Fprintf(&b, `<h2 style="margin:0 0 4px">%s</h2>`, e(subject))
	fmt.Fprintf(&b, `<p style="color:#64748b;margin:0 0 16px">%s &middot; %s</p>`, e(c.CompanyName), e(c.JobRole))

	if c.ApplicationStatus != "" {
		fmt.Fprintf(&b, `<p><strong>Status:</strong> %s</p>`, e(c.ApplicationStatus))
	}

	if c.RoundName != "" {
		b.WriteString(`<table style="width:100%;border-collapse:collapse;margin:16px 0;border:1px solid #e2e8f0;border-radius:8px;overflow:hidden">`)
		fmt.Fprintf(&b, `<tr><td colspan="2" style="background:#f8fafc;padding:10px 14px;font-weight:600">%s</td></tr>`, e(c.RoundName))
		when := "Date to be announced"
		if c.When != "" {
			when = c.When
		}
		b.WriteString(tableRow("When", e(when)))
		if c.Mode != "" {
			b.WriteString(tableRow("Mode", e(c.Mode)))
		}
		if c.Location != "" {
			b.WriteString(tableRow(roundLabel(c.Mode), e(c.Location)))
		}
		if c.DurationMinutes > 0 {
			b.WriteString(tableRow("Duration", fmt.Sprintf("%d minutes", c.DurationMinutes)))
		}
		b.WriteString(`</table>`)
		if c.Instructions != "" {
			fmt.Fprintf(&b, `<p><strong>What to prepare / bring:</strong><br>%s</p>`, e(c.Instructions))
		}
	}

	if c.NextRoundName != "" {
		fmt.Fprintf(&b, `<p><strong>Next round:</strong> %s</p>`, e(c.NextRoundName))
	}
	if c.OfferCTC > 0 {
		fmt.Fprintf(&b, `<p><strong>Offer:</strong> &#8377;%.1f LPA. Respond by %s.</p>`, c.OfferCTC, e(c.OfferValidUntil))
	}
	if c.Note != "" {
		fmt.Fprintf(&b, `<p style="color:#475569"><em>Note from %s: %s</em></p>`, e(c.CompanyName), e(c.Note))
	}

	fmt.Fprintf(&b, `<p style="margin-top:24px"><a href="%s" style="display:inline-block;background:#4f46e5;color:#fff;text-decoration:none;padding:10px 20px;border-radius:6px;font-weight:600">View in PlacementHub</a></p>`, e(actionURL))
	b.WriteString(`</div>`)
	return b.String()
}

func tableRow(label, value string) string {
	return fmt.Sprintf(`<tr><td style="padding:6px 14px;color:#64748b;white-space:nowrap">%s</td><td style="padding:6px 14px">%s</td></tr>`, label, value)
}
