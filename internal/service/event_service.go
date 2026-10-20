package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // event time zones work in the Alpine image, which has no zoneinfo
	"unicode/utf8"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// EventStore persists events and sharing (postgres.EventRepo).
type EventStore interface {
	EventAccess(ctx context.Context, sc authz.OrgScope, eventID string) (*domain.EventAccess, error)
	ListEvents(ctx context.Context, sc authz.OrgScope, q domain.EventListQuery, all bool) ([]domain.EventRow, error)
	CreateEvent(ctx context.Context, sc authz.OrgScope, e *domain.Event, copySharesFrom string, a domain.Audit) (*domain.Event, error)
	UpdateEvent(ctx context.Context, es authz.EventScope, e *domain.Event, a domain.Audit) (*domain.Event, error)
	SetArchived(ctx context.Context, es authz.EventScope, archived bool) error
	SetDeleted(ctx context.Context, es authz.EventScope, deleted bool) error
	UpdateSettings(ctx context.Context, es authz.EventScope, st domain.EventSettings) error
	TransferOwnership(ctx context.Context, es authz.EventScope, newOwnerID string) error
	SetStarred(ctx context.Context, es authz.EventScope, starred bool) error
	TouchOpened(ctx context.Context, es authz.EventScope) error
	Sharing(ctx context.Context, es authz.EventScope) (*domain.SharingState, error)
	GetShare(ctx context.Context, es authz.EventScope, shareID string) (*domain.EventShare, error)
	IsMember(ctx context.Context, sc authz.OrgScope, accountID string) (bool, error)
	PutShare(ctx context.Context, es authz.EventScope, accountID, role string) (*domain.EventShare, error)
	RemoveShare(ctx context.Context, es authz.EventScope, shareID string) error
	CreateInvite(ctx context.Context, es authz.EventScope, email, role string, tokenHash []byte, expires time.Time) (*domain.Invite, error)
	RevokeInvite(ctx context.Context, es authz.EventScope, inviteID string) error
	SetLink(ctx context.Context, es authz.EventScope, tokenHash []byte, expires *time.Time) error
	RemoveLink(ctx context.Context, es authz.EventScope) error
	EventByLink(ctx context.Context, tokenHash []byte) (*domain.Event, string, error)
}

// AccountFinder looks people up by email (postgres.AccountRepo).
type AccountFinder interface {
	FindByEmail(ctx context.Context, email string) (*domain.Account, error)
}

// AuditReader lists the activity log (postgres.OrgRepo).
type AuditReader interface {
	ListAudit(ctx context.Context, sc authz.OrgScope, eventID string, limit int) ([]domain.AuditEntry, error)
}

type EventService struct {
	events   EventStore
	accounts AccountFinder
	audit    AuditReader
	mailer   Mailer
	baseURL  string
	product  string
	now      func() time.Time
}

func NewEventService(events EventStore, accounts AccountFinder, audit AuditReader, mailer Mailer, baseURL, product string) *EventService {
	return &EventService{events: events, accounts: accounts, audit: audit, mailer: mailer,
		baseURL: baseURL, product: product, now: time.Now}
}

// ---- Access --------------------------------------------------------------------

// Resolve opens an event for the caller: their scope (strongest of org role,
// ownership and share) and the event. No access is ErrNotFound, so nobody
// learns which event IDs exist. Trashed events open only for those who may
// restore them.
func (s *EventService) Resolve(ctx context.Context, sc authz.OrgScope, eventID string) (authz.EventScope, *domain.Event, error) {
	acc, err := s.events.EventAccess(ctx, sc, eventID)
	if err != nil {
		return authz.EventScope{}, nil, err
	}
	es, ok := authz.ForEvent(sc, acc.Event.ID, acc.Event.OwnerID, acc.ShareRole, acc.Event.Settings)
	if !ok || (acc.Event.DeletedAt != nil && !es.Can(authz.EventDelete)) {
		return authz.EventScope{}, nil, domain.ErrNotFound
	}
	return es, acc.Event, nil
}

// List returns one view of the events home.
func (s *EventService) List(ctx context.Context, sc authz.OrgScope, q domain.EventListQuery) ([]domain.EventCard, error) {
	if err := sc.Require(authz.ViewOrg); err != nil {
		return nil, err
	}
	if q.View != "all" && !slices.Contains(domain.EventViews, q.View) {
		q.View = domain.ViewMine
	}
	q.Q = strings.TrimSpace(q.Q)
	if utf8.RuneCountInString(q.Q) > 100 {
		q.Q = string([]rune(q.Q)[:100])
	}
	if q.Sort == "" && q.View == domain.ViewRecent {
		q.Sort = domain.SortOpened
	}
	rows, err := s.events.ListEvents(ctx, sc, q, sc.Can(authz.AllEvents))
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]domain.EventCard, 0, len(rows))
	for _, r := range rows {
		es, ok := authz.ForEvent(sc, r.ID, r.OwnerID, r.ShareRole, r.Settings)
		if !ok || (q.View == domain.ViewTrash && !es.Can(authz.EventDelete)) {
			continue
		}
		c := r.EventCard
		c.Role = es.Role()
		c.DisplayStatus = r.Event.DisplayStatus(now)
		out = append(out, c)
	}
	return out, nil
}

// ---- Validation ----------------------------------------------------------------

var (
	nonSlug     = regexp.MustCompile(`[^a-z0-9]+`)
	langRe      = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})?$`)
	maxCapacity = 1_000_000
)

// Slugify makes a URL slug from a name: "Space Apps Colombo 2026" ->
// "space-apps-colombo-2026".
func Slugify(name string) string {
	s := nonSlug.ReplaceAllString(strings.ToLower(name), "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = strings.TrimRight(s[:60], "-")
	}
	if s == "" {
		return "event"
	}
	return s
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{domain.ErrValidation}, a...)...)
}

// applyInput copies the set fields of in onto e and checks the result.
func applyInput(e *domain.Event, in domain.EventInput) error {
	if in.Name != nil {
		e.Name = strings.TrimSpace(*in.Name)
	}
	if in.Type != nil {
		e.Type = *in.Type
	}
	if in.Description != nil {
		e.Description = strings.TrimSpace(*in.Description)
	}
	if in.ClearDates {
		e.StartsAt, e.EndsAt = nil, nil
	}
	if in.StartsAt != nil {
		t := in.StartsAt.UTC()
		e.StartsAt = &t
	}
	if in.EndsAt != nil {
		t := in.EndsAt.UTC()
		e.EndsAt = &t
	}
	if in.TimeZone != nil {
		e.TimeZone = strings.TrimSpace(*in.TimeZone)
	}
	if in.Venue != nil {
		v := *in.Venue
		v.Name, v.Address = strings.TrimSpace(v.Name), strings.TrimSpace(v.Address)
		e.Venue = v
	}
	if in.OnlineURL != nil {
		e.OnlineURL = strings.TrimSpace(*in.OnlineURL)
	}
	if in.ClearCapacity {
		e.Capacity = nil
	}
	if in.Capacity != nil {
		c := *in.Capacity
		e.Capacity = &c
	}
	if in.Visibility != nil {
		e.Visibility = *in.Visibility
	}
	if in.Language != nil {
		e.Language = strings.TrimSpace(*in.Language)
	}
	if in.Tags != nil {
		e.Tags = domain.NormalizeTags(in.Tags)
	}
	return validateEvent(e)
}

func validateEvent(e *domain.Event) error {
	if n := utf8.RuneCountInString(e.Name); n < 2 || n > 150 || !ValidDisplayName(e.Name) {
		return invalid("the event name must be 2 to 150 characters")
	}
	if !slices.Contains(domain.EventTypes, e.Type) {
		return invalid("unknown event type %q", e.Type)
	}
	if utf8.RuneCountInString(e.Description) > 10_000 {
		return invalid("the description is longer than 10,000 characters")
	}
	if _, err := time.LoadLocation(e.TimeZone); err != nil || e.TimeZone == "" || e.TimeZone == "Local" {
		return invalid("unknown time zone %q", e.TimeZone)
	}
	if e.EndsAt != nil && e.StartsAt == nil {
		return invalid("set a start before an end")
	}
	if e.StartsAt != nil && e.EndsAt != nil && e.EndsAt.Before(*e.StartsAt) {
		return invalid("the event ends before it starts")
	}
	v := e.Venue
	if utf8.RuneCountInString(v.Name) > 150 || utf8.RuneCountInString(v.Address) > 300 ||
		!ValidDisplayName(v.Name) || !ValidDisplayName(v.Address) {
		return invalid("the venue name or address is too long")
	}
	if (v.Lat == nil) != (v.Lng == nil) || (v.Lat != nil && (*v.Lat < -90 || *v.Lat > 90 || *v.Lng < -180 || *v.Lng > 180)) {
		return invalid("the map pin needs a latitude (-90 to 90) and a longitude (-180 to 180)")
	}
	if e.OnlineURL != "" {
		u, err := url.Parse(e.OnlineURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(e.OnlineURL) > 500 {
			return invalid("the online link must be a full http or https address")
		}
	}
	if e.Capacity != nil && (*e.Capacity < 1 || *e.Capacity > maxCapacity) {
		return invalid("capacity must be between 1 and %d", maxCapacity)
	}
	if !slices.Contains(domain.EventVisibilities, e.Visibility) {
		return invalid("visibility must be private, unlisted or public")
	}
	if !langRe.MatchString(e.Language) {
		return invalid("language must be a code such as en or si")
	}
	return nil
}

// ParseLocalTime reads an HTML datetime-local value ("2026-10-03T09:00") in a
// time zone. Empty is nil.
func ParseLocalTime(value, tz string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, invalid("unknown time zone %q", tz)
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, value, loc); err == nil {
			return &t, nil
		}
	}
	return nil, invalid("dates must look like 2026-10-03 09:00")
}

// ---- Create, change, archive, delete ---------------------------------------------

// CreateEventRequest is a new event: blank or a type preset, or a copy of an
// existing event (DuplicateFrom).
type CreateEventRequest struct {
	domain.EventInput
	DuplicateFrom string `json:"duplicate_from"`
}

func (s *EventService) Create(ctx context.Context, sc authz.OrgScope, req CreateEventRequest) (*domain.Event, error) {
	if err := sc.Require(authz.CreateEvent); err != nil {
		return nil, err
	}
	if req.DuplicateFrom != "" {
		es, src, err := s.Resolve(ctx, sc, req.DuplicateFrom)
		if err != nil {
			return nil, err
		}
		return s.Duplicate(ctx, es, src, req.EventInput)
	}
	e := &domain.Event{Type: "hackathon", TimeZone: "UTC", Visibility: "private", Language: "en", Tags: []string{},
		RegMode: domain.RegOpen, Waitlist: true}
	if err := applyInput(e, req.EventInput); err != nil {
		return nil, err
	}
	e.Slug = Slugify(e.Name)
	e.PassPrefix = PassPrefix(e.Name)
	return s.events.CreateEvent(ctx, sc, e, "", domain.Audit{Action: "event.create", TargetType: "event",
		Diff: map[string]any{"name": e.Name, "type": e.Type}})
}

// Duplicate copies an event's details and people into a new draft owned by
// the caller. Fields set in in override the copy (usually name and dates).
func (s *EventService) Duplicate(ctx context.Context, es authz.EventScope, src *domain.Event, in domain.EventInput) (*domain.Event, error) {
	if err := es.Org().Require(authz.CreateEvent); err != nil {
		return nil, err
	}
	if err := es.Require(authz.EventEdit); err != nil {
		return nil, err
	}
	e := *src
	e.Name = "Copy of " + src.Name
	if utf8.RuneCountInString(e.Name) > 150 {
		e.Name = string([]rune(e.Name)[:150])
	}
	e.Status, e.ArchivedAt, e.DeletedAt = domain.StatusDraft, nil, nil
	if err := applyInput(&e, in); err != nil {
		return nil, err
	}
	e.Slug = Slugify(e.Name)
	return s.events.CreateEvent(ctx, es.Org(), &e, src.ID, domain.Audit{Action: "event.duplicate", TargetType: "event",
		Diff: map[string]any{"name": e.Name, "from_event": src.ID, "from_name": src.Name}})
}

// Update applies a patch. The audit entry lists what changed.
func (s *EventService) Update(ctx context.Context, es authz.EventScope, ev *domain.Event, in domain.EventInput) (*domain.Event, error) {
	if err := es.Require(authz.EventEdit); err != nil {
		return nil, err
	}
	if ev.DeletedAt != nil {
		return nil, inTrash()
	}
	next := *ev
	if err := applyInput(&next, in); err != nil {
		return nil, err
	}
	diff := eventDiff(ev, &next)
	if len(diff) == 0 {
		return ev, nil
	}
	return s.events.UpdateEvent(ctx, es, &next, domain.Audit{Action: "event.update", Diff: diff})
}

func inTrash() error {
	return fmt.Errorf("%w: this event is in Trash; restore it first", domain.ErrConflict)
}

// eventDiff names the fields that changed, with before and after values for
// the short ones.
func eventDiff(a, b *domain.Event) map[string]any {
	d := map[string]any{}
	pair := func(k string, x, y any) { d[k] = map[string]any{"from": x, "to": y} }
	if a.Name != b.Name {
		pair("name", a.Name, b.Name)
	}
	if a.Type != b.Type {
		pair("type", a.Type, b.Type)
	}
	if a.Description != b.Description {
		d["description"] = "changed"
	}
	if !sameTime(a.StartsAt, b.StartsAt) || !sameTime(a.EndsAt, b.EndsAt) || a.TimeZone != b.TimeZone {
		pair("dates", a.DateRange(), b.DateRange())
	}
	if a.Venue.Name != b.Venue.Name || a.Venue.Address != b.Venue.Address || !sameFloat(a.Venue.Lat, b.Venue.Lat) || !sameFloat(a.Venue.Lng, b.Venue.Lng) {
		pair("venue", a.Venue.Name, b.Venue.Name)
	}
	if a.OnlineURL != b.OnlineURL {
		pair("online_url", a.OnlineURL, b.OnlineURL)
	}
	if !sameInt(a.Capacity, b.Capacity) {
		pair("capacity", a.Capacity, b.Capacity)
	}
	if a.Visibility != b.Visibility {
		pair("visibility", a.Visibility, b.Visibility)
	}
	if a.Language != b.Language {
		pair("language", a.Language, b.Language)
	}
	if !slices.Equal(a.Tags, b.Tags) {
		pair("tags", a.Tags, b.Tags)
	}
	return d
}

func sameTime(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}
func sameInt(a, b *int) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
func sameFloat(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func (s *EventService) SetArchived(ctx context.Context, es authz.EventScope, ev *domain.Event, archived bool) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	if ev.DeletedAt != nil {
		return inTrash()
	}
	return s.events.SetArchived(ctx, es, archived)
}

// SetDeleted moves the event to Trash or restores it.
func (s *EventService) SetDeleted(ctx context.Context, es authz.EventScope, ev *domain.Event, deleted bool) error {
	if err := es.Require(authz.EventDelete); err != nil {
		return err
	}
	if !deleted && ev.DeletedAt != nil && s.now().Sub(*ev.DeletedAt) > domain.TrashRetention {
		return fmt.Errorf("%w: this event was deleted more than 30 days ago and can't be restored", domain.ErrConflict)
	}
	return s.events.SetDeleted(ctx, es, deleted)
}

// UpdateSettings changes the owner's switches (owner only).
func (s *EventService) UpdateSettings(ctx context.Context, es authz.EventScope, st domain.EventSettings) error {
	if err := es.Require(authz.EventTransfer); err != nil {
		return err
	}
	return s.events.UpdateSettings(ctx, es, st)
}

func (s *EventService) Transfer(ctx context.Context, es authz.EventScope, ev *domain.Event, newOwnerID string) error {
	if err := es.Require(authz.EventTransfer); err != nil {
		return err
	}
	if ev.DeletedAt != nil {
		return inTrash()
	}
	if newOwnerID == "" {
		return invalid("choose the new owner")
	}
	return s.events.TransferOwnership(ctx, es, newOwnerID)
}

func (s *EventService) SetStarred(ctx context.Context, es authz.EventScope, starred bool) error {
	if err := es.Require(authz.EventView); err != nil {
		return err
	}
	return s.events.SetStarred(ctx, es, starred)
}

// Opened records a visit for the Recent view. Failure only costs Recent, so
// it is logged rather than returned.
func (s *EventService) Opened(ctx context.Context, es authz.EventScope) {
	if err := s.events.TouchOpened(ctx, es); err != nil {
		slog.Warn("record event open", "event", es.EventID(), "err", err)
	}
}

// Activity returns the event's audit log.
func (s *EventService) Activity(ctx context.Context, es authz.EventScope) ([]domain.AuditEntry, error) {
	if err := es.Require(authz.EventViewAudit); err != nil {
		return nil, err
	}
	return s.audit.ListAudit(ctx, es.Org(), es.EventID(), 200)
}

// ---- Sharing -------------------------------------------------------------------

var shareRoles = []string{domain.EventRoleEditor, domain.EventRoleCheckin, domain.EventRoleViewer}

// Sharing is the Share tab: visible to those who can edit or share.
func (s *EventService) Sharing(ctx context.Context, es authz.EventScope) (*domain.SharingState, error) {
	if !es.Can(authz.EventEdit) && !es.Can(authz.EventShare) {
		return nil, domain.ErrForbidden
	}
	return s.events.Sharing(ctx, es)
}

// ShareResult is a share given to an existing account, or an invite emailed
// to an address that has none yet.
type ShareResult struct {
	Share    *domain.EventShare `json:"share,omitempty"`
	Invite   *domain.Invite     `json:"invite,omitempty"`
	Notified bool               `json:"notified"`
}

// Share gives email a role on the event, like the Share dialog in Google
// Docs: someone with an account gets access at once and an email; anyone
// else gets an emailed invite.
func (s *EventService) Share(ctx context.Context, es authz.EventScope, org *domain.OrgMembership, ev *domain.Event, actor *domain.Account, email, role string) (*ShareResult, error) {
	if err := es.Require(authz.EventShare); err != nil {
		return nil, err
	}
	if ev.DeletedAt != nil {
		return nil, inTrash()
	}
	if !slices.Contains(shareRoles, role) {
		return nil, invalid("role must be editor, checkin or viewer")
	}
	email, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	if email == strings.ToLower(ev.OwnerEmail) {
		return nil, fmt.Errorf("%w: %s owns this event", domain.ErrConflict, email)
	}
	acct, err := s.accounts.FindByEmail(ctx, email)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	member := false
	if acct != nil {
		if member, err = s.events.IsMember(ctx, es.Org(), acct.ID); err != nil {
			return nil, err
		}
	}
	if !member && !org.Settings.AllowsExternalSharing() {
		return nil, fmt.Errorf("%w: %s can't share events with people outside the organization", domain.ErrForbidden, org.Name)
	}

	who := actor.Name
	if who == "" {
		who = actor.Email
	}
	if acct != nil {
		share, err := s.events.PutShare(ctx, es, acct.ID, role)
		if err != nil {
			return nil, err
		}
		link := fmt.Sprintf("%s/o/%s/events/%s", s.baseURL, org.Slug, ev.ID)
		err = s.send(ctx, email, fmt.Sprintf("%s shared %s with you", who, ev.Name), ActionEmail{
			Product: s.product, Heading: ev.Name,
			Paragraph: []string{fmt.Sprintf("%s gave you %s access to %s (%s).", who, roleLabel(role), ev.Name, org.Name)},
			Button:    "Open event", URL: link,
			Footnote: "You can find it any time under Shared with me.",
		})
		if err != nil {
			slog.Warn("share notification not sent", "event", ev.ID, "err", err)
		}
		return &ShareResult{Share: share, Notified: err == nil}, nil
	}

	token := NewSecret()
	inv, err := s.events.CreateInvite(ctx, es, email, role, HashToken(token), s.now().Add(InviteTTL))
	if err != nil {
		return nil, err
	}
	err = s.send(ctx, email, fmt.Sprintf("%s invited you to %s", who, ev.Name), ActionEmail{
		Product: s.product, Heading: ev.Name,
		Paragraph: []string{
			fmt.Sprintf("%s invited you to %s (%s) with %s access.", who, ev.Name, org.Name, roleLabel(role)),
			fmt.Sprintf("Sign in with this address (%s) to accept. The invite expires in 7 days.", email),
		},
		Button: "Accept invite", URL: s.baseURL + "/invite?token=" + token,
		Footnote: "Not expecting this? Ignore the email and nothing changes.",
	})
	if err != nil {
		if rerr := s.events.RevokeInvite(ctx, es, inv.ID); rerr != nil {
			slog.Error("withdraw unsent event invite", "invite", inv.ID, "err", rerr)
		}
		if errors.Is(err, domain.ErrEmailUnavailable) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: the invite email could not be sent, so no invite was created; try again", domain.ErrEmailUnavailable)
	}
	return &ShareResult{Invite: inv, Notified: true}, nil
}

func (s *EventService) send(ctx context.Context, to, subject string, e ActionEmail) error {
	text, html, err := e.Render()
	if err != nil {
		return err
	}
	return s.mailer.Send(ctx, Message{To: mail.Address{Address: to}, Subject: subject, Text: text, HTML: html})
}

func roleLabel(role string) string {
	switch role {
	case domain.EventRoleEditor:
		return "editor"
	case domain.EventRoleCheckin:
		return "check-in staff"
	}
	return "viewer"
}

func (s *EventService) SetShareRole(ctx context.Context, es authz.EventScope, shareID, role string) error {
	if err := es.Require(authz.EventShare); err != nil {
		return err
	}
	if !slices.Contains(shareRoles, role) {
		return invalid("role must be editor, checkin or viewer")
	}
	sh, err := s.events.GetShare(ctx, es, shareID)
	if err != nil {
		return err
	}
	_, err = s.events.PutShare(ctx, es, sh.AccountID, role)
	return err
}

// RemoveShare takes someone's access away. Anyone may remove their own.
func (s *EventService) RemoveShare(ctx context.Context, es authz.EventScope, shareID string) error {
	sh, err := s.events.GetShare(ctx, es, shareID)
	if err != nil {
		return err
	}
	if sh.AccountID != es.AccountID() {
		if err := es.Require(authz.EventShare); err != nil {
			return err
		}
	}
	return s.events.RemoveShare(ctx, es, shareID)
}

func (s *EventService) RevokeInvite(ctx context.Context, es authz.EventScope, inviteID string) error {
	if err := es.Require(authz.EventShare); err != nil {
		return err
	}
	return s.events.RevokeInvite(ctx, es, inviteID)
}

// EnableLink turns on (or resets) the view-only link and returns its URL,
// shown once: only the token's hash is stored. days 0 means no expiry.
func (s *EventService) EnableLink(ctx context.Context, es authz.EventScope, org *domain.OrgMembership, ev *domain.Event, days int) (string, error) {
	if err := es.Require(authz.EventShare); err != nil {
		return "", err
	}
	if ev.DeletedAt != nil {
		return "", inTrash()
	}
	if !org.Settings.AllowsExternalSharing() {
		return "", fmt.Errorf("%w: %s doesn't allow sharing outside the organization, so links are off", domain.ErrForbidden, org.Name)
	}
	if days < 0 || days > 365 {
		return "", invalid("a link can last 1 to 365 days, or 0 for no expiry")
	}
	var expires *time.Time
	if days > 0 {
		t := s.now().Add(time.Duration(days) * 24 * time.Hour)
		expires = &t
	}
	token := NewSecret()
	if err := s.events.SetLink(ctx, es, HashToken(token), expires); err != nil {
		return "", err
	}
	return s.baseURL + "/l/" + token, nil
}

func (s *EventService) DisableLink(ctx context.Context, es authz.EventScope) error {
	if err := es.Require(authz.EventShare); err != nil {
		return err
	}
	return s.events.RemoveLink(ctx, es)
}

// LinkView opens an event by its view-only link.
func (s *EventService) LinkView(ctx context.Context, token string) (*domain.Event, string, error) {
	if token == "" {
		return nil, "", domain.ErrNotFound
	}
	return s.events.EventByLink(ctx, HashToken(token))
}
