package domain

import "time"

// ---- Participant profiles and the participant map (beta) ----------------------
// See migrations/015_participant_profiles.sql.

const (
	ProfilePrivate = "private"
	ProfilePublic  = "public" // seen by this event's other participants
)

type ProfileLinks struct {
	LinkedIn string `json:"linkedin,omitempty"`
	Website  string `json:"website,omitempty"`
	GitHub   string `json:"github,omitempty"`
}

// ParticipantProfile is what a participant chose to show about themselves
// at one event.
type ParticipantProfile struct {
	ParticipantID  string       `json:"participant_id"`
	Headline       string       `json:"headline"`
	Links          ProfileLinks `json:"links"`
	HasPhoto       bool         `json:"has_photo"`
	PhotoUpdatedAt *time.Time   `json:"photo_updated_at,omitempty"`
	Visibility     string       `json:"visibility"`
}

// MapPerson is a public profile on the participant map, with a location
// when they're sharing one.
type MapPerson struct {
	ParticipantID string       `json:"id"`
	FullName      string       `json:"name"`
	Headline      string       `json:"headline,omitempty"`
	Links         ProfileLinks `json:"links"`
	PhotoVersion  int64        `json:"photo_version,omitempty"` // 0: no photo
	Lat           *float64     `json:"lat,omitempty"`
	Lng           *float64     `json:"lng,omitempty"`
	AccuracyM     *float64     `json:"accuracy_m,omitempty"`
	LocatedAt     *time.Time   `json:"located_at,omitempty"`
	Me            bool         `json:"me,omitempty"`
}

// MapStats is the organizer's view of the beta at one event.
type MapStats struct {
	Enabled        bool
	PublicProfiles int
	SharingNow     int
	Up, Down       int
	Feedback       []BetaFeedback
}

type BetaFeedback struct {
	Rating    int       `json:"rating"` // 1 or -1
	Comment   string    `json:"comment"`
	FullName  string    `json:"full_name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
