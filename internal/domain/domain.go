// Package domain holds the vocabulary shared across features: roles, stages,
// branches and notification categories.
package domain

import (
	"hash/fnv"
	"slices"
)

const (
	RoleStudent = "student"
	RoleCompany = "company"
	RoleAdmin   = "admin"
)

// Application stages. Withdrawn is set when a student withdraws, declines an
// offer, or accepts a different offer.
const (
	StageApplied     = "Applied"
	StageShortlisted = "Shortlisted"
	StageInterview   = "Interview"
	StageOffered     = "Offered"
	StageRejected    = "Rejected"
	StageWithdrawn   = "Withdrawn"
)

var Stages = []string{StageApplied, StageShortlisted, StageInterview, StageOffered, StageRejected, StageWithdrawn}

// ActiveStages are the stages in which an application is still in play.
var ActiveStages = []string{StageApplied, StageShortlisted, StageInterview, StageOffered}

const (
	JobDraft    = "Draft"
	JobPending  = "Pending"
	JobOpen     = "Open"
	JobClosed   = "Closed"
	JobRejected = "Rejected"
)

const (
	CompanyPending  = "Pending"
	CompanyApproved = "Approved"
	CompanyRejected = "Rejected"
)

const (
	OfferPending   = "Pending"
	OfferAccepted  = "Accepted"
	OfferDeclined  = "Declined"
	OfferExpired   = "Expired"
	OfferRescinded = "Rescinded"
)

const (
	StudentPlaced    = "Placed"
	StudentInProcess = "In process"
	StudentUnplaced  = "Unplaced"
)

var Branches = []string{"CSE", "IT", "ECE", "EEE", "Mechanical", "Civil", "BBA", "BCA", "MBA"}

func ValidBranch(b string) bool { return slices.Contains(Branches, b) }

var JobTypes = []string{"Full-time", "Internship"}
var DriveModes = []string{"On-campus", "Virtual", "Off-campus"}
var NoticeTags = []string{"Result", "Drive", "Alert", "Event", "General"}

var RoundModes = []string{"Online", "Offline"}

// Selection-round statuses, tracked per (application, round). A row only
// exists once a recruiter has moved a candidate off the implicit default of
// Upcoming — see internal/round.
const (
	RoundUpcoming  = "Upcoming"
	RoundScheduled = "Scheduled"
	RoundCleared   = "Cleared"
	RoundRejected  = "Rejected"
)

var RoundStatuses = []string{RoundUpcoming, RoundScheduled, RoundCleared, RoundRejected}

// Notification types. Each maps to a category the student can mute.
const (
	NotifNewJob         = "new_job"
	NotifStageUpdate    = "application_update"
	NotifInterview      = "interview"
	NotifOffer          = "offer"
	NotifOfferExpiring  = "offer_expiring"
	NotifOfferExpired   = "offer_expired"
	NotifDeadline       = "deadline"
	NotifDriveNew       = "drive_new"
	NotifDriveUpdated   = "drive_updated"
	NotifDriveReminder  = "drive_reminder"
	NotifNotice         = "notice"
	NotifPlacementFinal = "placement"

	// Selection-round events. Every one of these is as time-sensitive as an
	// interview or offer, so CategoryFor maps them to CatCritical below.
	NotifRoundScheduled = "round_scheduled" // a round's schedule was set/updated for one candidate
	NotifRoundCleared   = "round_cleared"
	NotifRoundRejected  = "round_rejected"
	NotifRoundUpdated   = "round_updated"  // logistics of an already-scheduled round changed for everyone in it
	NotifRoundReminder  = "round_reminder" // an already-Scheduled round starts soon
)

// Categories are what students see in their mute settings.
const (
	CatNewJob       = "new_job"
	CatApplications = "application_update"
	CatDeadlines    = "deadline"
	CatDrives       = "drive"
	CatNotices      = "notice"
	// CatCritical covers interviews and offers. It cannot be muted per
	// category; only the master push switch turns it off.
	CatCritical = "critical"
)

// MutableCategories is the set a student may toggle.
var MutableCategories = []string{CatNewJob, CatApplications, CatDeadlines, CatDrives, CatNotices}

// CategoryFor returns the mute category of a notification type.
func CategoryFor(notifType string) string {
	switch notifType {
	case NotifNewJob:
		return CatNewJob
	case NotifStageUpdate:
		return CatApplications
	case NotifDeadline:
		return CatDeadlines
	case NotifDriveNew, NotifDriveUpdated, NotifDriveReminder:
		return CatDrives
	case NotifNotice:
		return CatNotices
	default: // interview, offer, offer_expiring, offer_expired, placement,
		// round_scheduled, round_cleared, round_rejected, round_updated, round_reminder
		return CatCritical
	}
}

var palette = []string{"#4f46e5", "#0891b2", "#059669", "#7c3aed", "#e11d48", "#d97706", "#db2777", "#2563eb", "#64748b"}

// ColorFor picks a stable brand tint for a company name.
func ColorFor(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return palette[int(h.Sum32())%len(palette)]
}
