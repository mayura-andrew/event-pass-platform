package domain

import "time"

// ---- Checkpoints, check-ins and workshops (SaaS milestone M4) ------------------
// See migrations/011_checkin_workshops.sql.

// Checkpoint kinds.
const (
	KindConfirm  = "confirm"
	KindCheckIn  = "check_in"
	KindWorkshop = "workshop"
	KindCustom   = "custom"
)

// CheckpointRules decide when a checkpoint may be recorded.
type CheckpointRules struct {
	Requires   []string   `json:"requires,omitempty"` // checkpoint IDs that must be recorded first
	Repeatable bool       `json:"repeatable"`         // e.g. re-entry, meals with seconds
	OpensAt    *time.Time `json:"opens_at,omitempty"`
	ClosesAt   *time.Time `json:"closes_at,omitempty"`
	Roles      []string   `json:"roles,omitempty"` // event roles that may record; empty = anyone who can scan
}

type Checkpoint struct {
	ID         string          `json:"id"`
	EventID    string          `json:"event_id"`
	WorkshopID string          `json:"workshop_id,omitempty"`
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	Rules      CheckpointRules `json:"rules"`
	Position   int             `json:"position"`
	ArchivedAt *time.Time      `json:"archived_at,omitempty"`
	Recorded   int             `json:"recorded"` // people with an active check-in here
}

// Checkin is one recorded scan.
type Checkin struct {
	ID            string     `json:"id"`
	ParticipantID string     `json:"participant_id"`
	CheckpointID  string     `json:"checkpoint_id"`
	StaffID       string     `json:"staff_account_id,omitempty"`
	StaffName     string     `json:"staff_name"`
	DeviceID      string     `json:"device_id,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UndoneAt      *time.Time `json:"undone_at,omitempty"`
}

// CheckinRow is a check-in as the recent-scans list shows it.
type CheckinRow struct {
	Checkin
	FullName   string `json:"full_name"`
	PassID     string `json:"pass_id"`
	Checkpoint string `json:"checkpoint"`
}

// RecordInput is one check-in to write.
type RecordInput struct {
	ParticipantID  string
	CheckpointID   string
	IdempotencyKey string
	DeviceID       string
	ClientTime     *time.Time
	Once           bool
}

// Scan results, shown on the scanner with a status band: green for
// recorded/ready, amber for duplicate and not-signed-up, red otherwise.
const (
	ScanRecorded     = "recorded"      // recorded now
	ScanReady        = "ready"         // valid; waiting for staff to press Record
	ScanDuplicate    = "duplicate"     // already recorded at this checkpoint
	ScanNotSignedUp  = "not_signed_up" // workshop needs a sign-up
	ScanNotAllowed   = "not_allowed"   // a prerequisite is missing, or the staff role can't record this
	ScanOutsideHours = "outside_window"
)

// Workshop signup modes.
const (
	SignupEveryone = "everyone"
	SignupOptIn    = "opt_in"
	SignupInvite   = "invite"
)

// Workshop sign-up statuses.
const (
	SignupSignedUp   = "signed_up"
	SignupWaitlisted = "waitlisted"
	SignupCancelled  = "cancelled"
)

type Workshop struct {
	ID           string     `json:"id"`
	EventID      string     `json:"event_id"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	Speakers     string     `json:"speakers"`
	Room         string     `json:"room"`
	StartsAt     *time.Time `json:"starts_at"`
	EndsAt       *time.Time `json:"ends_at"`
	Capacity     *int       `json:"capacity"`
	SignupMode   string     `json:"signup_mode"`
	Waitlist     bool       `json:"waitlist"`
	Position     int        `json:"position"`
	CheckpointID string     `json:"checkpoint_id"`
	SignedUp     int        `json:"signed_up"`
	Waitlisted   int        `json:"waitlisted"`
	Attended     int        `json:"attended"`
	MyStatus     string     `json:"my_status,omitempty"` // portal: the participant's own sign-up
}

// PlacesLeft is how many more can sign up (-1: no limit).
func (w *Workshop) PlacesLeft() int {
	if w.Capacity == nil {
		return -1
	}
	return max(*w.Capacity-w.SignedUp, 0)
}

// Overlaps reports whether two workshops run at the same time.
func (w *Workshop) Overlaps(o *Workshop) bool {
	if w.StartsAt == nil || w.EndsAt == nil || o.StartsAt == nil || o.EndsAt == nil {
		return false
	}
	return w.StartsAt.Before(*o.EndsAt) && o.StartsAt.Before(*w.EndsAt)
}

type WorkshopSignup struct {
	WorkshopID    string    `json:"workshop_id"`
	ParticipantID string    `json:"participant_id"`
	FullName      string    `json:"full_name"`
	Email         string    `json:"email"`
	PassID        string    `json:"pass_id"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

// EventStats feed the live dashboard.
type EventStats struct {
	Participants ParticipantCounts `json:"participants"`
	Checkpoints  []Checkpoint      `json:"checkpoints"`
	Workshops    []Workshop        `json:"workshops"`
	// Check-ins per 10 minutes over the last 12 hours, oldest first.
	PerTenMinutes []Bucket `json:"per_ten_minutes"`
	// Registrations per day over the last 30 days.
	RegistrationsPerDay []Bucket `json:"registrations_per_day"`
	CheckedIn           int      `json:"checked_in"`
	// Minors: registered people under 18 on the event's cut-off date;
	// AsksBirthDate: the form has a birth date question (else no tile).
	Minors        int       `json:"minors"`
	AsksBirthDate bool      `json:"asks_birth_date"`
	At            time.Time `json:"at"`
}

type Bucket struct {
	Start time.Time `json:"start"`
	Count int       `json:"count"`
}
