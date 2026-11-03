package domain

import (
	"encoding/json"
	"time"
)

// ---- Registration and participants (SaaS milestone M3) ----------------------
// See migrations/009_registration.sql.

// Registration modes.
const (
	RegOpen     = "open"     // anyone with the page registers straight away
	RegApproval = "approval" // registrations wait for an organizer
)

// Participant statuses.
const (
	PartPending    = "pending"
	PartRegistered = "registered"
	PartWaitlisted = "waitlisted"
	PartCancelled  = "cancelled"
)

var ParticipantStatuses = []string{PartPending, PartRegistered, PartWaitlisted, PartCancelled}

// Form field types offered in the form builder. Photo and file uploads
// arrive with file storage.
var FieldTypes = []string{"text", "long_text", "email", "phone", "number", "date", "birth_date",
	"select", "multi_select", "checkbox", "consent", "url"}

// FieldBirthDate is the birth date question: at most one per event, its
// answer decides who is under 18 on the event's cut-off date.
const FieldBirthDate = "birth_date"

var FieldTypeLabels = map[string]string{
	"text": "Short text", "long_text": "Long text", "email": "Email", "phone": "Phone", "number": "Number",
	"date": "Date", "birth_date": "Birth date (under-18 check)", "select": "Choose one", "multi_select": "Choose several", "checkbox": "Yes / no",
	"consent": "Consent", "url": "Web address",
}

// Who may see an answer. "pass" also prints it on the pass.
var FieldVisibilities = []string{"admin", "staff", "participant", "pass"}

// FormField is one question on an event's registration form. Name and email
// are always asked and are not FormFields.
type FormField struct {
	ID         string   `json:"id"`
	Key        string   `json:"key"`
	Type       string   `json:"type"`
	Label      string   `json:"label"`
	Help       string   `json:"help,omitempty"`
	Required   bool     `json:"required"`
	Options    []string `json:"options,omitempty"` // select, multi_select
	Visibility string   `json:"visibility"`
	Position   int      `json:"position"`
}

// FieldConfig is FormField's stored config (form_fields.config).
type FieldConfig struct {
	Label      string   `json:"label"`
	Help       string   `json:"help,omitempty"`
	Required   bool     `json:"required"`
	Options    []string `json:"options,omitempty"`
	Visibility string   `json:"visibility"`
}

// EventParticipant is one person's registration for one event.
type EventParticipant struct {
	ID          string         `json:"id"`
	EventID     string         `json:"event_id"`
	AccountID   string         `json:"account_id,omitempty"`
	Email       string         `json:"email"`
	FullName    string         `json:"full_name"`
	Status      string         `json:"status"`
	PassID      string         `json:"pass_id"`
	Answers     map[string]any `json:"answers"`
	Source      string         `json:"source"`
	ExternalRef string         `json:"-"` // e.g. a Google Form response ID, imported once
	// BirthDate is the answer to the event's birth date question
	// (YYYY-MM-DD, "" when not asked or not given), and Age the person's age
	// on the event's cut-off date (nil without a birth date).
	BirthDate    string    `json:"birth_date,omitempty"`
	Age          *int      `json:"age,omitempty"`
	RegisteredAt time.Time `json:"registered_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// RegistrationSettings are the organizer's choices for an event's form.
type RegistrationSettings struct {
	Mode       string     `json:"reg_mode"`
	OpensAt    *time.Time `json:"reg_opens_at"`
	ClosesAt   *time.Time `json:"reg_closes_at"`
	Waitlist   bool       `json:"waitlist"`
	PassPrefix string     `json:"pass_prefix"`
	AgeOn      *time.Time `json:"age_on"` // the under-18 cut-off; nil: the start date
}

// PublicEvent is what a public event page shows.
type PublicEvent struct {
	Event   *Event
	OrgName string
	OrgSlug string
	Fields  []FormField
	Taken   int // registered + pending, for capacity
}

// RegistrationOpen reports whether new registrations are accepted now, and
// if not, why.
func (p *PublicEvent) RegistrationOpen(now time.Time) (bool, string) {
	e := p.Event
	switch {
	case e.RegOpensAt != nil && now.Before(*e.RegOpensAt):
		return false, "Registration opens " + e.RegOpensAt.UTC().Format("2 Jan 2006, 15:04 UTC") + "."
	case e.RegClosesAt != nil && !now.Before(*e.RegClosesAt):
		return false, "Registration has closed."
	case e.EndsAt != nil && now.After(*e.EndsAt):
		return false, "This event has finished."
	case e.Capacity != nil && p.Taken >= *e.Capacity && !e.Waitlist:
		return false, "This event is full."
	}
	return true, ""
}

// AlreadyRegisteredError: this email already has a live registration for
// the event (which it carries). It is a conflict.
type AlreadyRegisteredError struct{ Participant EventParticipant }

func (e *AlreadyRegisteredError) Error() string { return "already registered" }
func (e *AlreadyRegisteredError) Unwrap() error { return ErrConflict }

// ParticipantQuery filters the organizer's participant list.
type ParticipantQuery struct {
	Status string
	Q      string // name, email or pass ID
	Minors bool   // only people under 18 on the event's cut-off date
}

// CountMinors is the ParticipantCounts key for people under 18 on the
// cut-off date (cancelled registrations not included).
const CountMinors = "minors"

// Minor says whether the person is under 18 on the event's cut-off date.
func (p EventParticipant) Minor() bool { return p.Age != nil && *p.Age < 18 }

// ParticipantCounts are the per-status totals for an event.
type ParticipantCounts map[string]int

// Outbox email templates.
const (
	MailPassIssued    = "pass_issued"
	MailReceived      = "registration_received"
	MailWaitlisted    = "waitlisted"
	MailCancelled     = "registration_cancelled"
	MailResendMinimum = 5 * time.Minute // between re-sent passes to one person
)

// MailForStatus is the email a participant gets on entering a status.
var MailForStatus = map[string]string{
	PartRegistered: MailPassIssued, PartPending: MailReceived,
	PartWaitlisted: MailWaitlisted, PartCancelled: MailCancelled,
}

// OutboxJob is one email to send.
type OutboxJob struct {
	ID            string
	OrgID         string
	ParticipantID string
	ToEmail       string
	Template      string
	Attempts      int
}

// PassContext is everything needed to render and email one pass.
type PassContext struct {
	Participant EventParticipant
	Event       Event
	OrgName     string
	OrgSlug     string
	Fields      []FormField
	// Imported from the current build: the QR keeps its old payload.
	LegacySig string
	// The event's pinned pass design (nil: Classic).
	Design json.RawMessage
}
