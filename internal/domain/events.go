package domain

import (
	"strings"
	"time"
)

// ---- Events and sharing (SaaS milestone M2) ---------------------------------
// See migrations/008_events_sharing.sql.

// Event types offered as starting points.
var EventTypes = []string{"hackathon", "conference", "workshop_series", "workshop", "meetup"}

// EventTypeLabels are the names shown for EventTypes.
var EventTypeLabels = map[string]string{
	"hackathon": "Hackathon", "conference": "Conference", "workshop_series": "Workshop series",
	"workshop": "Single workshop", "meetup": "Meetup",
}

var EventVisibilities = []string{"private", "unlisted", "public"}

// Display statuses (spec: Draft, Published, Live, Finished, Archived), plus
// Trash for deleted events.
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
	StatusLive      = "live"
	StatusFinished  = "finished"
	StatusArchived  = "archived"
	StatusTrash     = "trash"
)

// TrashRetention is how long a deleted event can be restored.
const TrashRetention = 30 * 24 * time.Hour

// EventSettings are the owner's switches that change the role matrix.
type EventSettings struct {
	EditorsCanShare  bool `json:"editors_can_share"`
	ViewersCanExport bool `json:"viewers_can_export"`
}

type EventVenue struct {
	Name    string   `json:"name,omitempty"`
	Address string   `json:"address,omitempty"`
	Lat     *float64 `json:"lat,omitempty"`
	Lng     *float64 `json:"lng,omitempty"`
}

type Event struct {
	ID          string        `json:"id"`
	OrgID       string        `json:"org_id"`
	Slug        string        `json:"slug"`
	Name        string        `json:"name"`
	Type        string        `json:"type"`
	Status      string        `json:"status"` // stored: draft | published
	Description string        `json:"description"`
	StartsAt    *time.Time    `json:"starts_at"`
	EndsAt      *time.Time    `json:"ends_at"`
	TimeZone    string        `json:"time_zone"`
	Venue       EventVenue    `json:"venue"`
	OnlineURL   string        `json:"online_url,omitempty"`
	Capacity    *int          `json:"capacity"`
	Visibility  string        `json:"visibility"`
	Language    string        `json:"language"`
	Tags        []string      `json:"tags"`
	Settings    EventSettings `json:"settings"`
	OwnerID     string        `json:"owner_id"`
	OwnerName   string        `json:"owner_name"`
	OwnerEmail  string        `json:"owner_email"`
	OwnerAvatar string        `json:"owner_avatar_url,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	ArchivedAt  *time.Time    `json:"archived_at,omitempty"`
	DeletedAt   *time.Time    `json:"deleted_at,omitempty"`
}

// DisplayStatus is the status chip: stored status, dates, archive and trash.
func (e *Event) DisplayStatus(now time.Time) string {
	switch {
	case e.DeletedAt != nil:
		return StatusTrash
	case e.ArchivedAt != nil:
		return StatusArchived
	case e.Status != StatusPublished:
		return StatusDraft
	case e.EndsAt != nil && now.After(*e.EndsAt):
		return StatusFinished
	case e.StartsAt != nil && !now.Before(*e.StartsAt):
		return StatusLive
	}
	return StatusPublished
}

// DateRange is the event's dates in its own time zone, e.g. "3–4 Oct 2026".
func (e *Event) DateRange() string {
	if e.StartsAt == nil {
		return "Dates not set"
	}
	loc, err := time.LoadLocation(e.TimeZone)
	if err != nil {
		loc = time.UTC
	}
	s := e.StartsAt.In(loc)
	if e.EndsAt == nil {
		return s.Format("2 Jan 2006")
	}
	end := e.EndsAt.In(loc)
	switch {
	case s.Year() != end.Year():
		return s.Format("2 Jan 2006") + " – " + end.Format("2 Jan 2006")
	case s.Month() != end.Month():
		return s.Format("2 Jan") + " – " + end.Format("2 Jan 2006")
	case s.Day() != end.Day():
		return s.Format("2") + "–" + end.Format("2 Jan 2006")
	}
	return s.Format("2 Jan 2006")
}

// Where is the venue name, or "Online", or "".
func (e *Event) Where() string {
	switch {
	case e.Venue.Name != "":
		return e.Venue.Name
	case e.OnlineURL != "":
		return "Online"
	}
	return ""
}

// EventCard is one event in the events home, as seen by the caller.
type EventCard struct {
	Event
	Role          string     `json:"role"` // caller's effective event role
	DisplayStatus string     `json:"display_status"`
	Starred       bool       `json:"starred"`
	LastOpenedAt  *time.Time `json:"last_opened_at,omitempty"`
	SharedCount   int        `json:"shared_count"` // people with a share
	LinkShared    bool       `json:"link_shared"`
	Registrations int        `json:"registrations"` // participants arrive in M3
}

// EventRow is what the store returns for list views, before the caller's
// role is worked out.
type EventRow struct {
	EventCard
	ShareRole string // caller's explicit share, if any
}

// Events home views.
const (
	ViewMine     = "mine"
	ViewShared   = "shared"
	ViewRecent   = "recent"
	ViewStarred  = "starred"
	ViewArchived = "archived"
	ViewTrash    = "trash"
)

var EventViews = []string{ViewMine, ViewShared, ViewRecent, ViewStarred, ViewArchived, ViewTrash}

// Sorts.
const (
	SortOpened   = "opened"
	SortModified = "modified"
	SortDate     = "date"
	SortName     = "name"
)

type EventListQuery struct {
	View string
	Q    string // search: name, venue, tags
	Sort string
}

// EventInput creates an event or, with nil fields left alone, patches one.
type EventInput struct {
	Name        *string     `json:"name"`
	Type        *string     `json:"type"`
	Description *string     `json:"description"`
	StartsAt    *time.Time  `json:"starts_at"`
	EndsAt      *time.Time  `json:"ends_at"`
	TimeZone    *string     `json:"time_zone"`
	Venue       *EventVenue `json:"venue"`
	OnlineURL   *string     `json:"online_url"`
	Capacity    *int        `json:"capacity"`
	Visibility  *string     `json:"visibility"`
	Language    *string     `json:"language"`
	Tags        []string    `json:"tags"`
	// ClearDates / ClearCapacity let a PATCH remove a value.
	ClearDates    bool `json:"clear_dates"`
	ClearCapacity bool `json:"clear_capacity"`
}

// EventAccess is an event plus the caller's explicit share, for building an
// authz.EventScope.
type EventAccess struct {
	Event     *Event
	ShareRole string
}

// EventShare is one person with a role on an event.
type EventShare struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	AvatarURL string    `json:"avatar_url,omitempty"`
	Role      string    `json:"role"`
	Guest     bool      `json:"guest"` // not a member of the org
	GrantedBy string    `json:"granted_by_name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// EventLink is the event's view-only link, if on.
type EventLink struct {
	Enabled   bool       `json:"enabled"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	URL       string     `json:"url,omitempty"` // only right after it is created
}

// SharingState is everything on the Share tab.
type SharingState struct {
	Owner   OrgMember    `json:"owner"`
	Shares  []EventShare `json:"shares"`
	Invites []Invite     `json:"invites"`
	Link    EventLink    `json:"link"`
}

// AuditEntry is one line of the activity log.
type AuditEntry struct {
	ID         string         `json:"id"`
	EventID    string         `json:"event_id,omitempty"`
	EventName  string         `json:"event_name,omitempty"`
	ActorID    string         `json:"actor_account_id,omitempty"`
	ActorName  string         `json:"actor_name"`
	Action     string         `json:"action"`
	TargetType string         `json:"target_type"`
	TargetID   string         `json:"target_id"`
	Diff       map[string]any `json:"diff"`
	CreatedAt  time.Time      `json:"created_at"`
}

// Audit is a change to record, written in the same transaction as the change.
type Audit struct {
	EventID    string
	Action     string
	TargetType string
	TargetID   string
	Diff       map[string]any
}

// OrgSettings are org-wide switches stored in orgs.settings.
type OrgSettings struct {
	// ExternalSharing lets events be shared with people outside the org.
	// Default on; stored as a pointer so "unset" reads as on.
	ExternalSharing *bool `json:"external_sharing,omitempty"`
}

func (s OrgSettings) AllowsExternalSharing() bool {
	return s.ExternalSharing == nil || *s.ExternalSharing
}

// NormalizeTags trims, lower-cases and de-duplicates tags (at most 10).
func NormalizeTags(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] || len(t) > 30 {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) == 10 {
			break
		}
	}
	return out
}
