package domain

import "time"

// ---- Pass email queue (email_jobs) ---------------------------------------------

// Email job statuses.
const (
	EmailPending   = "pending"
	EmailSending   = "sending"
	EmailSent      = "sent"
	EmailFailed    = "failed"
	EmailCancelled = "cancelled"
)

// EmailKindPass is the delegate pass confirmation email.
const EmailKindPass = "pass_confirmation"

// EmailJob is a claimed job together with what the email needs to show.
type EmailJob struct {
	ID             string
	ParticipantID  string
	RecipientEmail string
	Attempts       int // including the current one
	MaxAttempts    int

	// The delegate as it is now (not when queued).
	FullName     string
	MembershipID string
	Email        string
	TeamName     string
	VenueName    string // empty when unassigned
	HallSection  string
	VenueNote    string // venues.description
	QRSignature  string
	IsConfirmed  bool
}

// EmailFailure is one job that gave up, for the dashboard.
type EmailFailure struct {
	JobID          string    `json:"job_id"`
	ParticipantID  string    `json:"participant_id"`
	MembershipID   string    `json:"membership_id"`
	FullName       string    `json:"full_name"`
	RecipientEmail string    `json:"recipient_email"`
	Attempts       int       `json:"attempts"`
	LastError      string    `json:"last_error"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// EmailQueueState is the stored pause switch.
type EmailQueueState struct {
	Paused      bool      `json:"paused"`
	PauseReason string    `json:"pause_reason,omitempty"` // empty when an organizer paused it
	UpdatedAt   time.Time `json:"updated_at"`
}

// EmailQueueStats is the body of GET /api/v1/admin/emails/status.
type EmailQueueStats struct {
	Pending   int `json:"pending"` // includes jobs waiting to retry
	Retrying  int `json:"retrying"`
	Sending   int `json:"sending"`
	Sent      int `json:"sent"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`

	ConfirmedDelegates int `json:"confirmed_delegates"`
	// Confirmed delegates with no sent or queued pass email: what a bulk
	// enqueue would add.
	NotYetEmailed int `json:"not_yet_emailed"`

	SentLast24h int        `json:"sent_last_24h"`
	DailyLimit  int        `json:"daily_limit"` // 0 = no limit
	LastSentAt  *time.Time `json:"last_sent_at"`

	EmailQueueState
	Enabled         bool           `json:"enabled"` // SMTP configured
	WorkerRunning   bool           `json:"worker_running"`
	SendInterval    float64        `json:"send_interval_seconds"`
	WaitingForLimit bool           `json:"waiting_for_daily_limit"` // paused by the cap, resumes on its own
	EstimatedFinish *time.Time     `json:"estimated_finish"`        // nil when nothing is pending
	RecentFailures  []EmailFailure `json:"recent_failures"`
}

// EmailEnqueueResult answers send-single and send-bulk.
type EmailEnqueueResult struct {
	Queued  int    `json:"queued"`
	Message string `json:"message"`
}

// EmailSingleRequest is the body of POST /api/v1/admin/emails/send-single.
// Give one of the two.
type EmailSingleRequest struct {
	ParticipantID string `json:"participant_id"`
	MembershipID  string `json:"membership_id"`
}

// Email queue limits.
const (
	MaxEmailErrorLen     = 500 // characters of last_error kept
	EmailRecentFailures  = 50
	EmailStaleClaimAfter = 5 * time.Minute // a "sending" job older than this is reclaimed
)
