package service

// Participant profiles and the participant map (beta, M6 demo).
//
// Privacy rules:
//   - A profile is private until its owner makes it public, and public
//     means "to other registered participants of this event" only.
//   - The map is off unless the event's organizer turns it on.
//   - Someone appears on the map only with a public profile AND while they
//     choose to share their location; a location stops showing after
//     LocationFresh and stopping deletes it. Going private, or the organizer
//     turning the map off, deletes it too.
//   - Only registered participants (not pending, waitlisted or cancelled)
//     can see the map.

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/passes"
	"github.com/mayura-andrew/event-pass-platform/internal/repository/postgres"
)

// LocationFresh is how long a shared location shows after its last update.
const LocationFresh = 3 * time.Minute

// ProfileStore is postgres.ProfileRepo.
type ProfileStore interface {
	Profile(ctx context.Context, orgID, participantID string) (*domain.ParticipantProfile, error)
	SaveProfile(ctx context.Context, orgID, eventID, participantID string, u postgres.ProfileUpdate) error
	Photo(ctx context.Context, orgID, eventID, participantID string, publicOnly bool) ([]byte, error)
	MapEnabled(ctx context.Context, orgID, eventID string) (bool, error)
	People(ctx context.Context, orgID, eventID, meID string, fresh time.Duration) ([]domain.MapPerson, error)
	SetLocation(ctx context.Context, orgID, eventID, participantID string, lat, lng, accuracy float64) error
	ClearLocation(ctx context.Context, orgID, participantID string) error
	AddFeedback(ctx context.Context, orgID, eventID, participantID string, rating int, comment string) error
	SetMapEnabled(ctx context.Context, es authz.EventScope, enabled bool) error
	MapStats(ctx context.Context, es authz.EventScope, fresh time.Duration) (*domain.MapStats, error)
}

type ProfileService struct {
	portal PortalStore // finds the caller's own registration
	store  ProfileStore
}

func NewProfileService(portal PortalStore, store ProfileStore) *ProfileService {
	return &ProfileService{portal: portal, store: store}
}

// mine is the caller's own registration; anyone else's is not found.
func (s *ProfileService) mine(ctx context.Context, as authz.AccountScope, participantID string) (*domain.PassContext, error) {
	return s.portal.MyPass(ctx, as, participantID)
}

// Profile is the caller's profile for one registration, and whether the
// event's map is on.
func (s *ProfileService) Profile(ctx context.Context, as authz.AccountScope, participantID string) (*domain.ParticipantProfile, bool, error) {
	pc, err := s.mine(ctx, as, participantID)
	if err != nil {
		return nil, false, err
	}
	p, err := s.store.Profile(ctx, pc.Event.OrgID, participantID)
	if err != nil {
		return nil, false, err
	}
	on, err := s.store.MapEnabled(ctx, pc.Event.OrgID, pc.Event.ID)
	return p, on, err
}

// ProfileInput is the pass page's profile form.
type ProfileInput struct {
	Headline, LinkedIn, Website, GitHub, Visibility string
	Photo                                           io.Reader // nil: keep the current one
	RemovePhoto                                     bool
}

func (s *ProfileService) SaveProfile(ctx context.Context, as authz.AccountScope, participantID string, in ProfileInput) error {
	pc, err := s.mine(ctx, as, participantID)
	if err != nil {
		return err
	}
	errs := FieldErrors{}
	headline := strings.TrimSpace(in.Headline)
	if utf8.RuneCountInString(headline) > 100 || strings.ContainsFunc(headline, unicode.IsControl) {
		errs["headline"] = "Keep the headline to one line of at most 100 characters."
	}
	var links domain.ProfileLinks
	links.LinkedIn = profileLink(in.LinkedIn, []string{"linkedin.com"}, "linkedin", "Enter your LinkedIn profile link (linkedin.com/in/...).", errs)
	links.GitHub = profileLink(in.GitHub, []string{"github.com"}, "github", "Enter your GitHub link (github.com/...).", errs)
	links.Website = profileLink(in.Website, nil, "website", "Enter a web address such as https://example.com.", errs)
	if in.Visibility != domain.ProfilePublic {
		in.Visibility = domain.ProfilePrivate
	}
	u := postgres.ProfileUpdate{Headline: headline, Links: links, Visibility: in.Visibility, RemovePhoto: in.RemovePhoto}
	if in.Photo != nil && !in.RemovePhoto {
		if u.Photo, err = passes.PrepareAvatar(in.Photo); err != nil {
			errs["photo"] = "Upload a photo as a PNG, JPEG or WebP image of at most 10 MB."
		}
	}
	if len(errs) > 0 {
		return errs
	}
	return s.store.SaveProfile(ctx, pc.Event.OrgID, pc.Event.ID, participantID, u)
}

// profileLink checks an optional link: https added if missing, no
// passwords in it, and for LinkedIn and GitHub their own sites only.
func profileLink(raw string, hosts []string, key, msg string, errs FieldErrors) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	ok := err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.User == nil && strings.Contains(u.Hostname(), ".") && len(raw) <= 300
	if ok && hosts != nil {
		h := strings.ToLower(u.Hostname())
		ok = false
		for _, want := range hosts {
			if h == want || strings.HasSuffix(h, "."+want) {
				ok = true
			}
		}
	}
	if !ok {
		errs[key] = msg
		return ""
	}
	return u.String()
}

// Photo is a participant's photo as the caller may see it: their own, or a
// public one at an event where the caller can see the map.
func (s *ProfileService) Photo(ctx context.Context, as authz.AccountScope, participantID, otherID string) ([]byte, error) {
	if participantID == otherID {
		pc, err := s.mine(ctx, as, participantID)
		if err != nil {
			return nil, err
		}
		return s.store.Photo(ctx, pc.Event.OrgID, pc.Event.ID, otherID, false)
	}
	pc, err := s.mapAccess(ctx, as, participantID)
	if err != nil {
		return nil, err
	}
	return s.store.Photo(ctx, pc.Event.OrgID, pc.Event.ID, otherID, true)
}

// ErrMapOff: the organizer hasn't turned the map on for this event.
var ErrMapOff = fmt.Errorf("%w: the participant map isn't on for this event", domain.ErrForbidden)

// mapAccess is the caller's registration when they may use the event's map.
func (s *ProfileService) mapAccess(ctx context.Context, as authz.AccountScope, participantID string) (*domain.PassContext, error) {
	pc, err := s.mine(ctx, as, participantID)
	if err != nil {
		return nil, err
	}
	on, err := s.store.MapEnabled(ctx, pc.Event.OrgID, pc.Event.ID)
	if err != nil {
		return nil, err
	}
	if !on {
		return nil, ErrMapOff
	}
	if pc.Participant.Status != domain.PartRegistered {
		return nil, fmt.Errorf("%w: the map is for confirmed participants", domain.ErrForbidden)
	}
	return pc, nil
}

// MapView is what the map page shows.
type MapView struct {
	Event  *domain.Event
	Me     domain.MapPerson
	Public bool // the caller's profile is public, so they can share
	People []domain.MapPerson
}

func (s *ProfileService) Map(ctx context.Context, as authz.AccountScope, participantID string) (*MapView, error) {
	pc, err := s.mapAccess(ctx, as, participantID)
	if err != nil {
		return nil, err
	}
	people, err := s.store.People(ctx, pc.Event.OrgID, pc.Event.ID, participantID, LocationFresh)
	if err != nil {
		return nil, err
	}
	prof, err := s.store.Profile(ctx, pc.Event.OrgID, participantID)
	if err != nil {
		return nil, err
	}
	v := &MapView{Event: &pc.Event, Public: prof.Visibility == domain.ProfilePublic, People: []domain.MapPerson{}}
	for _, p := range people {
		if p.Me {
			v.Me = p
		} else {
			v.People = append(v.People, p)
		}
	}
	return v, nil
}

// ShareLocation records where the caller is. It needs a public profile:
// nobody shows on the map without choosing to be seen.
func (s *ProfileService) ShareLocation(ctx context.Context, as authz.AccountScope, participantID string, lat, lng, accuracy float64) error {
	pc, err := s.mapAccess(ctx, as, participantID)
	if err != nil {
		return err
	}
	if math.IsNaN(lat) || math.IsNaN(lng) || lat < -90 || lat > 90 || lng < -180 || lng > 180 || (lat == 0 && lng == 0) {
		return invalid("that location isn't valid")
	}
	if accuracy < 0 || accuracy > 100_000 || math.IsNaN(accuracy) {
		accuracy = 0
	}
	prof, err := s.store.Profile(ctx, pc.Event.OrgID, participantID)
	if err != nil {
		return err
	}
	if prof.Visibility != domain.ProfilePublic {
		return fmt.Errorf("%w: make your profile public to appear on the map", domain.ErrConflict)
	}
	return s.store.SetLocation(ctx, pc.Event.OrgID, pc.Event.ID, participantID, lat, lng, accuracy)
}

// StopSharing deletes the caller's location at once.
func (s *ProfileService) StopSharing(ctx context.Context, as authz.AccountScope, participantID string) error {
	pc, err := s.mine(ctx, as, participantID)
	if err != nil {
		return err
	}
	return s.store.ClearLocation(ctx, pc.Event.OrgID, participantID)
}

// Feedback records a thumbs up or down, with an optional comment.
func (s *ProfileService) Feedback(ctx context.Context, as authz.AccountScope, participantID string, rating int, comment string) error {
	pc, err := s.mapAccess(ctx, as, participantID)
	if err != nil {
		return err
	}
	if rating != 1 && rating != -1 {
		return invalid("choose thumbs up or down")
	}
	comment = strings.TrimSpace(comment)
	if utf8.RuneCountInString(comment) > 1000 {
		return invalid("keep the comment to 1,000 characters")
	}
	return s.store.AddFeedback(ctx, pc.Event.OrgID, pc.Event.ID, participantID, rating, comment)
}

// ---- Organizers ----------------------------------------------------------------

func (s *ProfileService) MapStats(ctx context.Context, es authz.EventScope) (*domain.MapStats, error) {
	if err := es.Require(authz.EventEdit); err != nil {
		return nil, err
	}
	return s.store.MapStats(ctx, es, LocationFresh)
}

func (s *ProfileService) SetMapEnabled(ctx context.Context, es authz.EventScope, enabled bool) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	return s.store.SetMapEnabled(ctx, es, enabled)
}
