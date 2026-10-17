package domain

import (
	"fmt"
	"time"
)

// ---- Filter criteria -----------------------------------------------------------

// Age groups accepted by ParticipantFilter.AgeGroup. Ages are computed from
// date_of_birth against the database's CURRENT_DATE.
const (
	AgeAll     = ""         // no age filter
	AgeUnder18 = "UNDER_18" // date_of_birth >  CURRENT_DATE - 18 years
	AgeAbove18 = "ABOVE_18" // date_of_birth <= CURRENT_DATE - 18 years
	AgeUnknown = "UNKNOWN"  // date_of_birth IS NULL
)

// Claim statuses accepted by ParticipantFilter.ClaimStatus. Both require a
// ScenarioID: "claimed" only means something for one checkpoint.
const (
	ClaimAll       = ""
	ClaimClaimed   = "CLAIMED"   // has a VALID scan for the scenario
	ClaimUnclaimed = "UNCLAIMED" // has no VALID scan for the scenario
)

// Confirmation filter values.
const (
	ConfirmAll         = ""
	ConfirmConfirmed   = "CONFIRMED"
	ConfirmUnconfirmed = "UNCONFIRMED"
)

// VenueUnassigned in ParticipantFilter.VenueID selects delegates with no venue.
const VenueUnassigned = "UNASSIGNED"

// Pagination bounds for the delegate table.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// ParticipantFilter is the body of POST /api/v1/admin/participants/filter.
// Empty strings mean "no filter" for every field.
type ParticipantFilter struct {
	Search       string `json:"search"`       // name, email, membership ID or team, substring match
	AgeGroup     string `json:"age_group"`    // AgeAll | AgeUnder18 | AgeAbove18 | AgeUnknown
	ScenarioID   string `json:"scenario_id"`  // scenario whose claim status is shown and filtered
	ClaimStatus  string `json:"claim_status"` // ClaimAll | ClaimClaimed | ClaimUnclaimed
	VenueID      string `json:"venue_id"`     // venue UUID, or VenueUnassigned
	Confirmation string `json:"confirmation"` // ConfirmAll | ConfirmConfirmed | ConfirmUnconfirmed
	Page         int    `json:"page"`         // 1-based
	PageSize     int    `json:"page_size"`    // 1..MaxPageSize
}

// ---- Delegate table ------------------------------------------------------------

// ParticipantRow is one row of the dashboard delegate table. The Claim* fields
// describe the filter's scenario and are empty when no scenario was chosen.
type ParticipantRow struct {
	ID           string     `json:"id"`
	MembershipID string     `json:"membership_id"`
	FullName     string     `json:"full_name"`
	Email        string     `json:"email"`
	TeamName     string     `json:"team_name"`
	IsConfirmed  bool       `json:"is_confirmed"`
	DateOfBirth  *string    `json:"date_of_birth"` // YYYY-MM-DD; nil when unknown
	AgeYears     *int       `json:"age_years"`     // nil when unknown
	IsMinor      bool       `json:"is_minor"`
	VenueID      *string    `json:"venue_id"`
	VenueName    string     `json:"venue_name"`
	HallSection  string     `json:"hall_section"`
	Claimed      bool       `json:"claimed"`
	ClaimCount   int        `json:"claim_count"` // >1 only for repeatable scenarios
	ClaimedAt    *time.Time `json:"claimed_at"`  // first VALID scan, server time
	ClaimedBy    string     `json:"claimed_by,omitempty"`
	ClaimMethod  string     `json:"claim_method,omitempty"` // QR | MANUAL_OVERRIDE
}

type ParticipantPage struct {
	Rows     []ParticipantRow `json:"rows"`
	Total    int              `json:"total"` // matching rows across all pages
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
}

// ---- Stats ---------------------------------------------------------------------

// DashboardStats feeds the metric cards and the throughput chart.
type DashboardStats struct {
	TotalParticipants int `json:"total_participants"`
	TotalConfirmed    int `json:"total_confirmed"`
	// Age counts cover confirmed delegates only.
	MinorCount      int `json:"minor_count"`
	AdultCount      int `json:"adult_count"`
	UnknownAgeCount int `json:"unknown_age_count"`

	// Distinct confirmed delegates with a VALID scan for the check-in scenario.
	CheckInScenarioCode string `json:"check_in_scenario_code"`
	CheckInScenarioID   string `json:"check_in_scenario_id,omitempty"` // empty if the code doesn't exist
	CheckedIn           int    `json:"checked_in"`
	MinorsCheckedIn     int    `json:"minors_checked_in"`

	ActiveScenarios int `json:"active_scenarios"`
	TotalScenarios  int `json:"total_scenarios"`

	// Throughput, by server receive time (synced_at). Scans taken offline count
	// when they upload, so a reconnecting phone shows up as a spike.
	ScansLastMinute int              `json:"scans_last_minute"`
	ScansLast15Min  int              `json:"scans_last_15_min"`
	PerMinute       []ThroughputSlot `json:"per_minute"` // oldest first, 15 slots, gaps filled with 0

	Scenarios []ScenarioStat `json:"scenarios"`
	Sync      SyncHealth     `json:"sync"`

	GeneratedAt time.Time `json:"generated_at"`
}

type ThroughputSlot struct {
	Minute time.Time `json:"minute"`
	Count  int       `json:"count"`
}

// ScenarioStat is a per-checkpoint claim summary.
type ScenarioStat struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Code             string `json:"code"`
	IsActive         bool   `json:"is_active"`
	AllowDuplicates  bool   `json:"allow_duplicates"`
	ValidScans       int    `json:"valid_scans"`
	UniqueDelegates  int    `json:"unique_delegates"`
	RejectedAttempts int    `json:"rejected_attempts"`
	ManualOverrides  int    `json:"manual_overrides"`
}

// SyncHealth summarizes the scanner fleet from device heartbeats.
type SyncHealth struct {
	DevicesOnline     int `json:"devices_online"`      // heartbeat within DeviceOnlineWindow
	DevicesStale      int `json:"devices_stale"`       // silent longer than DeviceOnlineWindow, within DeviceTrackWindow
	PendingOnDevices  int `json:"pending_on_devices"`  // sum of last-reported queues, tracked devices
	StaleWithPending  int `json:"stale_with_pending"`  // stale devices that still reported pending scans
	RejectedOnDevices int `json:"rejected_on_devices"` // lost-race scans reported by devices
}

// Device heartbeat windows. A device is "online" if it reported within
// DeviceOnlineWindow; devices silent longer than DeviceTrackWindow are left out
// of the totals (a phone put away yesterday shouldn't raise an alarm today).
const (
	DeviceOnlineWindow = 2 * time.Minute
	DeviceTrackWindow  = 12 * time.Hour
)

// ---- Manual override -----------------------------------------------------------

// ManualOverrideRequest is the body of POST /api/v1/admin/scans/manual-override.
// The acting admin is taken from the JWT, never from the body, so the audit
// trail can't be attributed to someone else.
type ManualOverrideRequest struct {
	ParticipantID string `json:"participant_id"`
	ScenarioID    string `json:"scenario_id"`
	Rationale     string `json:"rationale"`
}

// Rationale length bounds, also enforced by chk_scans_override_reason (min).
const (
	MinRationaleLen = 10
	MaxRationaleLen = 500
)

// ManualOverride is the validated input handed to the repository.
type ManualOverride struct {
	AdminID       string
	ParticipantID string
	ScenarioID    string
	Rationale     string
}

type ManualOverrideResult struct {
	ScanID          string    `json:"scan_id"`
	ParticipantName string    `json:"participant_name"`
	ScenarioName    string    `json:"scenario_name"`
	RecordedAt      time.Time `json:"recorded_at"`
}

// ClaimConflictError reports that a single-claim scenario was already claimed.
// It wraps ErrConflict so generic handling still works.
type ClaimConflictError struct {
	ClaimedBy string
	ClaimedAt time.Time
	Method    string
}

func (e *ClaimConflictError) Error() string {
	return fmt.Sprintf("already claimed by %s at %s", e.ClaimedBy, e.ClaimedAt.Format(time.RFC3339))
}

func (e *ClaimConflictError) Unwrap() error { return ErrConflict }

// ---- Live scan feed ------------------------------------------------------------

// ScanEvent is one entry of the real-time audit stream.
type ScanEvent struct {
	ID              string    `json:"id"`
	ParticipantID   string    `json:"participant_id"`
	ParticipantName string    `json:"participant_name"`
	MembershipID    string    `json:"membership_id"`
	IsMinor         bool      `json:"is_minor"`
	ScenarioID      string    `json:"scenario_id"`
	ScenarioName    string    `json:"scenario_name"`
	ScenarioCode    string    `json:"scenario_code"`
	AdminName       string    `json:"admin_name"`
	Status          string    `json:"status"`       // VALID | DUPLICATE_REJECTED
	EntryMethod     string    `json:"entry_method"` // QR | MANUAL_OVERRIDE
	OverrideReason  string    `json:"override_reason,omitempty"`
	ScannedAt       time.Time `json:"scanned_at"` // device clock
	SyncedAt        time.Time `json:"synced_at"`  // server clock
}

// ---- Device health -------------------------------------------------------------

// DeviceHeartbeat is sent by each scanner phone (POST /api/v1/devices/heartbeat).
// The admin is taken from the JWT.
type DeviceHeartbeat struct {
	DeviceID      string     `json:"device_id"`
	ScenarioID    string     `json:"scenario_id"` // checkpoint selected on the phone; may be empty
	PendingCount  int        `json:"pending_count"`
	PendingOthers int        `json:"pending_others"`
	RejectedCount int        `json:"rejected_count"`
	InvalidCount  int        `json:"invalid_count"`
	ManifestCount int        `json:"manifest_count"`
	LastSyncAt    *time.Time `json:"last_sync_at"`
	IsOnline      bool       `json:"is_online"`
	UserAgent     string     `json:"-"` // from the request header
}

// Device states shown on the dashboard.
const (
	DeviceOnline  = "ONLINE"  // reported within DeviceOnlineWindow
	DeviceStale   = "STALE"   // silent; may be offline with scans queued
	DeviceOffline = "OFFLINE" // silent longer than DeviceTrackWindow
)

type DeviceHealthItem struct {
	DeviceID      string     `json:"device_id"`
	AdminID       string     `json:"admin_id"`
	AdminName     string     `json:"admin_name"`
	AdminEmail    string     `json:"admin_email"`
	ScenarioName  string     `json:"scenario_name"`
	PendingCount  int        `json:"pending_count"`
	PendingOthers int        `json:"pending_others"`
	RejectedCount int        `json:"rejected_count"`
	InvalidCount  int        `json:"invalid_count"`
	ManifestCount int        `json:"manifest_count"`
	LastSyncAt    *time.Time `json:"last_sync_at"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
	IsOnline      bool       `json:"is_online"` // as last reported by the phone
	State         string     `json:"state"`     // DeviceOnline | DeviceStale | DeviceOffline
	UserAgent     string     `json:"user_agent"`
}

// ScenarioToggleRequest is the body of POST /api/v1/admin/scenarios/toggle.
type ScenarioToggleRequest struct {
	ScenarioID string `json:"scenario_id"`
	IsActive   bool   `json:"is_active"`
}
