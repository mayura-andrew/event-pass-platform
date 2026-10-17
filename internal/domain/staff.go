package domain

import "time"

// ---- Staff accounts ------------------------------------------------------------

// StaffGrant is one checkpoint a scanner account may scan.
type StaffGrant struct {
	ScenarioID string `json:"scenario_id"`
	Name       string `json:"name"`
	Code       string `json:"code"`
	IsActive   bool   `json:"is_active"`
}

// StaffMember is one row of the staff management table. Grants is empty for
// admins and superadmins, who can scan every checkpoint.
type StaffMember struct {
	ID                  string       `json:"id"`
	FullName            string       `json:"full_name"`
	Email               string       `json:"email"`
	Role                string       `json:"role"`
	IsActive            bool         `json:"is_active"`
	ForcePasswordChange bool         `json:"force_password_change"` // still on the emailed temporary password
	LastLoginAt         *time.Time   `json:"last_login_at"`
	CreatedAt           time.Time    `json:"created_at"`
	CreatedByName       string       `json:"created_by_name,omitempty"`
	Grants              []StaffGrant `json:"grants"`
}

// StaffCreateRequest is the body of POST /api/v1/admin/staff/create.
type StaffCreateRequest struct {
	FullName    string   `json:"full_name"`
	Email       string   `json:"email"`
	Role        string   `json:"role"`
	ScenarioIDs []string `json:"scenario_ids"` // scanner role only; ignored for admins
}

// StaffUpdateRequest is the body of PATCH /api/v1/admin/staff/{id}. Nil fields
// are left unchanged; a non-nil ScenarioIDs replaces the grants.
type StaffUpdateRequest struct {
	FullName    *string   `json:"full_name"`
	Role        *string   `json:"role"`
	IsActive    *bool     `json:"is_active"`
	ScenarioIDs *[]string `json:"scenario_ids"`
}

// NewStaff is validated input for the repository. PasswordHash is the bcrypt
// hash of the generated temporary password.
type NewStaff struct {
	FullName     string
	Email        string
	Role         string
	PasswordHash string
	ScenarioIDs  []string
	CreatedBy    string
}

// StaffCreateResult is returned once the account exists and the email went out.
type StaffCreateResult struct {
	Staff       *StaffMember `json:"staff"`
	EmailSentTo string       `json:"email_sent_to"`
}

// AccountState is the live state of an account, checked on every request so
// deactivation, role changes and password resets apply before the JWT expires.
type AccountState struct {
	ID                  string
	Role                string
	IsActive            bool
	ForcePasswordChange bool
}

// Staff field limits.
const (
	MaxStaffNameLen   = 100
	MaxStaffGrants    = 100
	MinPasswordLength = 10
	// bcrypt ignores everything after 72 bytes, so longer passwords would
	// silently be truncated.
	MaxPasswordBytes = 72
)
