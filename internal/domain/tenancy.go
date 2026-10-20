package domain

import (
	"context"
	"errors"
	"time"
)

// ---- SaaS tenancy (milestone M1) --------------------------------------------
// An Account is a person; an Org is the tenant that owns events. See
// migrations/007_tenancy.sql.

// Org roles. Event roles (editor, check-in staff, viewer) arrive with
// sharing in M2.
const (
	OrgRoleOwner  = "owner"  // everything, including billing and deleting the org
	OrgRoleAdmin  = "admin"  // members, branding, templates, integrations
	OrgRoleMember = "member" // creates events; sees own and shared ones
	// OrgRoleGuest is not stored: it is someone outside the org who was
	// shared one of its events.
	OrgRoleGuest = "guest"
)

// Event roles. The owner is events.owner_id; the others are shares.
const (
	EventRoleOwner   = "owner"
	EventRoleEditor  = "editor"
	EventRoleCheckin = "checkin" // check-in staff
	EventRoleViewer  = "viewer"
)

// Sign-in providers.
const (
	ProviderGoogle = "google"
	ProviderEmail  = "email"
)

type Account struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	AvatarURL   string     `json:"avatar_url,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// Session is a signed-in browser. The token itself is never stored.
type Session struct {
	ID        string
	AccountID string
	CSRFToken string
	ExpiresAt time.Time
}

type Org struct {
	ID           string      `json:"id"`
	Slug         string      `json:"slug"`
	Name         string      `json:"name"`
	BrandPrimary string      `json:"brand_primary,omitempty"`
	Plan         string      `json:"plan"`
	Settings     OrgSettings `json:"settings"`
	CreatedAt    time.Time   `json:"created_at"`
}

// OrgMembership is an org as seen by one member.
type OrgMembership struct {
	Org
	Role string `json:"role"`
}

type OrgMember struct {
	AccountID string    `json:"account_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	AvatarURL string    `json:"avatar_url,omitempty"`
	Role      string    `json:"role"`
	JoinedAt  time.Time `json:"joined_at"`
}

type Invite struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	EventID   string    `json:"event_id,omitempty"` // set for an event share invite
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	InvitedBy string    `json:"invited_by_name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// InviteDetails is what the invitee sees before accepting.
type InviteDetails struct {
	Invite
	OrgSlug   string `json:"org_slug"`
	EventName string `json:"event_name,omitempty"`
	OrgName   string `json:"org_name"`
	Accepted  bool   `json:"accepted"`
	Revoked   bool   `json:"revoked"`
}

var (
	// ErrUnauthenticated: no valid session (HTTP 401).
	ErrUnauthenticated = errors.New("sign in required")
	// ErrForbidden: signed in and a member, but the role doesn't allow it
	// (HTTP 403). Callers outside the org get ErrNotFound instead, so they
	// can't learn what exists in another tenant.
	ErrForbidden = errors.New("not allowed")
	// ErrRateLimited: the caller should wait before trying again (HTTP 429).
	ErrRateLimited = errors.New("too many requests")
)

type clientIPKey struct{}

// WithClientIP records the caller's address for the audit log.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIP returns the address set by WithClientIP, or "".
func ClientIP(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}
