// Package domain holds the core types shared by every layer.
package domain

import (
	"errors"
	"time"
)

// ---- Roles -----------------------------------------------------------------

const (
	RoleSuperAdmin = "superadmin" // everything, including staff management
	RoleAdmin      = "admin"      // dashboard and every checkpoint
	RoleScanner    = "scanner"    // scanner app, granted checkpoints only (was "volunteer")
)

// IsManager reports whether role may use the dashboard and scan every
// checkpoint without grants.
func IsManager(role string) bool { return role == RoleSuperAdmin || role == RoleAdmin }

// ValidRole reports whether role is one of the three account roles.
func ValidRole(role string) bool { return IsManager(role) || role == RoleScanner }

// ---- Entities --------------------------------------------------------------

type Admin struct {
	ID                  string
	Email               string
	PasswordHash        string
	FullName            string
	Role                string
	IsActive            bool
	ForcePasswordChange bool // signed in with a generated temporary password
}

type Venue struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	HallSection string  `json:"hall_section"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	Description string  `json:"description,omitempty"`
}

// ParticipantManifest is one row of the offline directory pushed to scanners.
// It deliberately carries qr_signature so devices can verify a QR offline.
type ParticipantManifest struct {
	ID           string `json:"id"`
	MembershipID string `json:"membership_id"`
	FullName     string `json:"full_name"`
	Email        string `json:"email"`
	TeamName     string `json:"team_name"`
	VenueName    string `json:"venue_name"`
	HallSection  string `json:"hall_section"`
	QRSignature  string `json:"qr_signature"`
}

// ParticipantPass is what the public portal shows one delegate.
type ParticipantPass struct {
	ID           string `json:"id"`
	MembershipID string `json:"membership_id"`
	FullName     string `json:"full_name"`
	TeamName     string `json:"team_name"`
	QRSignature  string `json:"qr_signature"`
	Venue        *Venue `json:"venue"` // nil when unassigned
}

type Scenario struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Code            string    `json:"code"`
	Description     string    `json:"description"`
	IsActive        bool      `json:"is_active"`
	AllowDuplicates bool      `json:"allow_duplicates"`
	CreatedAt       time.Time `json:"created_at"`
}

// ScenarioInput is used to create or patch a scenario. Pointer fields let a
// PATCH distinguish "not sent" from "set to false/empty".
type ScenarioInput struct {
	Name            *string `json:"name"`
	Code            *string `json:"code"`
	Description     *string `json:"description"`
	IsActive        *bool   `json:"is_active"`
	AllowDuplicates *bool   `json:"allow_duplicates"`
}

// Claim is a VALID single-claim scan already recorded on the server. Scanners
// download these so a device knows about claims made on other devices.
type Claim struct {
	ParticipantID string    `json:"participant_id"`
	ScenarioID    string    `json:"scenario_id"`
	SyncedAt      time.Time `json:"synced_at"`
}

// ---- Sync protocol ---------------------------------------------------------

// SyncScanPayload is one offline scan uploaded by a device. The scanning admin
// is NOT taken from the payload; the server uses the authenticated JWT subject.
type SyncScanPayload struct {
	IdempotencyKey  string    `json:"idempotency_key"`
	ParticipantID   string    `json:"participant_id"`
	ScenarioID      string    `json:"scenario_id"`
	ScannedAtDevice time.Time `json:"scanned_at_device"`
}

type SyncRequest struct {
	Scans []SyncScanPayload `json:"scans"`
}

// Sync result statuses. The client acts on them as follows:
//
//	SUCCESS            recorded; mark synced
//	ALREADY_SYNCED     this exact scan was recorded earlier (retry after a lost
//	                   response); mark synced
//	DUPLICATE_REJECTED another scan claimed it first; mark rejected, keep claim
//	INVALID            malformed or unknown references; never retry
//	FAILED             transient database error; retry later
const (
	SyncSuccess           = "SUCCESS"
	SyncAlreadySynced     = "ALREADY_SYNCED"
	SyncDuplicateRejected = "DUPLICATE_REJECTED"
	SyncInvalid           = "INVALID"
	SyncFailed            = "FAILED"
)

type SyncResult struct {
	IdempotencyKey string     `json:"idempotency_key"`
	Status         string     `json:"status"`
	Message        string     `json:"message"`
	ClaimedBy      string     `json:"claimed_by,omitempty"` // winning admin's name
	ClaimedAt      *time.Time `json:"claimed_at,omitempty"` // winning scan's device time
}

type SyncResponse struct {
	Results []SyncResult `json:"results"`
}

// ---- Errors ----------------------------------------------------------------

var (
	ErrNotFound           = errors.New("not found")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrConflict           = errors.New("conflict")
	ErrValidation         = errors.New("validation failed")
	// ErrPasswordChangeRequired: the account holds a temporary password and
	// may only set a new one.
	ErrPasswordChangeRequired = errors.New("password change required")
	// ErrEmailUnavailable: credentials could not be emailed (SMTP not
	// configured or the send failed). Nothing was saved.
	ErrEmailUnavailable = errors.New("email unavailable")
)
