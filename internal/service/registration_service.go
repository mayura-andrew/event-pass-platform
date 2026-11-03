package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

// RegistrationStore persists forms and participants (postgres.RegistrationRepo).
type RegistrationStore interface {
	ListFields(ctx context.Context, es authz.EventScope) ([]domain.FormField, error)
	SaveField(ctx context.Context, es authz.EventScope, f domain.FormField) (*domain.FormField, error)
	DeleteField(ctx context.Context, es authz.EventScope, fieldID string) error
	MoveField(ctx context.Context, es authz.EventScope, fieldID string, up bool) error
	PublicEvent(ctx context.Context, orgSlug, eventSlug string) (*domain.PublicEvent, error)
	Register(ctx context.Context, ev *domain.Event, p domain.EventParticipant, passID func() string) (*domain.EventParticipant, error)
	ListParticipants(ctx context.Context, es authz.EventScope, q domain.ParticipantQuery) ([]domain.EventParticipant, domain.ParticipantCounts, error)
	GetParticipant(ctx context.Context, es authz.EventScope, id string) (*domain.EventParticipant, error)
	SetStatus(ctx context.Context, es authz.EventScope, id, status string) error
	ResendPass(ctx context.Context, p *domain.EventParticipant, orgID string) (bool, error)
}

// PassLoader loads a participant's pass content (postgres.OutboxRepo).
type PassLoader interface {
	PassContext(ctx context.Context, orgID, participantID string) (*domain.PassContext, error)
}

// PublishStore is the part of the event store registration settings use.
type PublishStore interface {
	SetPublished(ctx context.Context, es authz.EventScope, published bool) error
	UpdateRegistration(ctx context.Context, es authz.EventScope, st domain.RegistrationSettings) error
}

type RegistrationService struct {
	store  RegistrationStore
	events PublishStore
	passes PassLoader
	now    func() time.Time
}

func NewRegistrationService(store RegistrationStore, events PublishStore, passes PassLoader) *RegistrationService {
	return &RegistrationService{store: store, events: events, passes: passes, now: time.Now}
}

// ---- Pass IDs ------------------------------------------------------------------

// passAlphabet is Crockford base32: no I, L, O or U, so an ID read aloud or
// copied from paper isn't mistaken.
const passAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewPassID returns PREFIX-XXXX-XXXX (40 random bits), e.g. CSA-7KQ4-M2XD.
func NewPassID(prefix string) string {
	b := make([]byte, 8)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(passAlphabet))))
		if err != nil {
			panic(err)
		}
		b[i] = passAlphabet[n.Int64()]
	}
	return prefix + "-" + string(b[:4]) + "-" + string(b[4:])
}

var prefixRe = regexp.MustCompile(`^[A-Z0-9]{2,6}$`)

// PassPrefix suggests a prefix from an event name: the initials of its first
// words ("Example Event 2026" -> "SAC").
func PassPrefix(name string) string {
	var initials []rune
	for _, w := range strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		r, _ := utf8.DecodeRuneInString(w)
		if r < 128 && unicode.IsLetter(r) {
			initials = append(initials, unicode.ToUpper(r))
		}
		if len(initials) == 4 {
			break
		}
	}
	if len(initials) < 2 {
		return "EVT"
	}
	return string(initials)
}

// ---- Form fields ---------------------------------------------------------------

var keyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,59}$`)

// fieldKey makes an answer key from a label: "T-shirt size" -> "t_shirt_size".
func fieldKey(label string) string {
	var b strings.Builder
	underscore := false
	for _, r := range strings.ToLower(label) {
		switch {
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			if b.Len() == 0 && unicode.IsDigit(r) {
				b.WriteString("q_")
			}
			b.WriteRune(r)
			underscore = false
		case !underscore && b.Len() > 0:
			b.WriteByte('_')
			underscore = true
		}
	}
	k := strings.TrimRight(b.String(), "_")
	if len(k) > 60 {
		k = strings.TrimRight(k[:60], "_")
	}
	if k == "" {
		return "question"
	}
	return k
}

// reservedKeys are the built-in questions every form has.
var reservedKeys = map[string]bool{"full_name": true, "email": true, "name": true}

func (s *RegistrationService) Fields(ctx context.Context, es authz.EventScope) ([]domain.FormField, error) {
	if !es.Can(authz.EventEdit) && !es.Can(authz.EventDashboard) {
		return nil, domain.ErrForbidden
	}
	return s.store.ListFields(ctx, es)
}

// SaveField adds a question (f.ID empty) or changes one. The key is fixed
// once created, because answers are stored under it.
func (s *RegistrationService) SaveField(ctx context.Context, es authz.EventScope, f domain.FormField) (*domain.FormField, error) {
	if err := es.Require(authz.EventEdit); err != nil {
		return nil, err
	}
	f.Label = strings.TrimSpace(f.Label)
	f.Help = strings.TrimSpace(f.Help)
	if n := utf8.RuneCountInString(f.Label); n < 1 || n > 200 || !ValidDisplayName(f.Label) {
		return nil, invalid("the question must be 1 to 200 characters")
	}
	if utf8.RuneCountInString(f.Help) > 300 {
		return nil, invalid("the help text is longer than 300 characters")
	}
	if !slices.Contains(domain.FieldTypes, f.Type) {
		return nil, invalid("unknown question type %q", f.Type)
	}
	if f.Visibility == "" {
		f.Visibility = "admin"
	}
	if !slices.Contains(domain.FieldVisibilities, f.Visibility) {
		return nil, invalid("visibility must be admin, staff, participant or pass")
	}
	opts := []string{}
	for _, o := range f.Options {
		if o = strings.TrimSpace(o); o != "" && !slices.Contains(opts, o) {
			if utf8.RuneCountInString(o) > 100 {
				return nil, invalid("an option is longer than 100 characters")
			}
			opts = append(opts, o)
		}
	}
	f.Options = nil
	if f.Type == "select" || f.Type == "multi_select" {
		if len(opts) < 1 || len(opts) > 100 {
			return nil, invalid("a choice question needs 1 to 100 options, one per line")
		}
		f.Options = opts
	}
	if f.ID == "" {
		if f.Key == "" {
			f.Key = fieldKey(f.Label)
		}
		if !keyRe.MatchString(f.Key) || reservedKeys[f.Key] {
			return nil, invalid("the key %q can't be used: lower-case letters, digits and underscores, not name or email", f.Key)
		}
	}
	return s.store.SaveField(ctx, es, f)
}

func (s *RegistrationService) DeleteField(ctx context.Context, es authz.EventScope, fieldID string) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	return s.store.DeleteField(ctx, es, fieldID)
}

func (s *RegistrationService) MoveField(ctx context.Context, es authz.EventScope, fieldID string, up bool) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	return s.store.MoveField(ctx, es, fieldID, up)
}

// ---- Publishing and settings -------------------------------------------------------

func (s *RegistrationService) UpdateSettings(ctx context.Context, es authz.EventScope, st domain.RegistrationSettings) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	st.PassPrefix = strings.ToUpper(strings.TrimSpace(st.PassPrefix))
	switch {
	case st.Mode != domain.RegOpen && st.Mode != domain.RegApproval:
		return invalid("registration must be open or need approval")
	case !prefixRe.MatchString(st.PassPrefix):
		return invalid("the pass ID prefix must be 2 to 6 letters or digits")
	case st.OpensAt != nil && st.ClosesAt != nil && !st.ClosesAt.After(*st.OpensAt):
		return invalid("registration closes before it opens")
	case st.AgeOn != nil && (st.AgeOn.Year() < 2000 || st.AgeOn.Year() > 2100):
		return invalid("the date ages are counted on must be a real date, such as 14 November of the event's year")
	}
	return s.events.UpdateRegistration(ctx, es, st)
}

// SetPublished publishes the event (its public page and form go live) or
// takes it back to draft.
func (s *RegistrationService) SetPublished(ctx context.Context, es authz.EventScope, ev *domain.Event, published bool) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	if ev.DeletedAt != nil {
		return inTrash()
	}
	if published && ev.StartsAt == nil {
		return invalid("set the event's dates before publishing")
	}
	return s.events.SetPublished(ctx, es, published)
}

// ---- Public page and registering -------------------------------------------------

func (s *RegistrationService) PublicEvent(ctx context.Context, orgSlug, eventSlug string) (*domain.PublicEvent, error) {
	return s.store.PublicEvent(ctx, strings.ToLower(orgSlug), strings.ToLower(eventSlug))
}

// RegisterRequest is a filled-in public form. Answers are as posted: one or
// more values per question key.
type RegisterRequest struct {
	FullName string
	Email    string
	Answers  map[string][]string
}

// RegisterResult is a new registration, or (Already) the existing one for
// this email.
type RegisterResult struct {
	Participant *domain.EventParticipant
	Event       *domain.PublicEvent
	Already     bool
	Resent      bool // Already, and the pass email was sent again
}

// FieldErrors maps a question key ("full_name", "email" or a field key) to
// what's wrong with its answer. It is a validation error.
type FieldErrors map[string]string

func (e FieldErrors) Error() string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + e[k]
	}
	return domain.ErrValidation.Error() + ": " + strings.Join(parts, "; ")
}

func (e FieldErrors) Unwrap() error { return domain.ErrValidation }

func (s *RegistrationService) Register(ctx context.Context, orgSlug, eventSlug string, req RegisterRequest) (*RegisterResult, error) {
	pe, err := s.PublicEvent(ctx, orgSlug, eventSlug)
	if err != nil {
		return nil, err
	}
	if ok, why := pe.RegistrationOpen(s.now()); !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrConflict, strings.TrimSuffix(why, "."))
	}
	errs := FieldErrors{}
	name := strings.TrimSpace(req.FullName)
	if n := utf8.RuneCountInString(name); n < 2 || n > 150 || !ValidDisplayName(name) {
		errs["full_name"] = "Enter your full name."
	}
	email, err := NormalizeEmail(req.Email)
	if err != nil {
		errs["email"] = "Enter a valid email address."
	}
	answers := CheckAnswers(pe.Fields, req.Answers, errs)
	if len(errs) > 0 {
		return nil, errs
	}
	p, err := s.store.Register(ctx, pe.Event, domain.EventParticipant{
		Email: email, FullName: name, Answers: answers, Source: "form", BirthDate: BirthDate(pe.Fields, answers),
	}, func() string { return NewPassID(pe.Event.PassPrefix) })
	var already *domain.AlreadyRegisteredError
	if errors.As(err, &already) {
		resent, err := s.store.ResendPass(ctx, &already.Participant, pe.Event.OrgID)
		if err != nil {
			return nil, err
		}
		return &RegisterResult{Participant: &already.Participant, Event: pe, Already: true, Resent: resent}, nil
	}
	if err != nil {
		return nil, err
	}
	return &RegisterResult{Participant: p, Event: pe}, nil
}

var phoneRe = regexp.MustCompile(`^\+?[0-9 ()\-]{6,30}$`)

// CheckAnswers validates posted answers against the form and returns them
// typed for storage: strings, numbers, booleans and string lists. Problems
// are added to errs by field key. Unknown keys are dropped.
func CheckAnswers(fields []domain.FormField, in map[string][]string, errs FieldErrors) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		var vals []string
		for _, v := range in[f.Key] {
			if v = strings.TrimSpace(v); v != "" {
				vals = append(vals, v)
			}
		}
		if len(vals) == 0 {
			if f.Required {
				if f.Type == "consent" {
					errs[f.Key] = "Please tick this box to continue."
				} else {
					errs[f.Key] = "Please answer this question."
				}
			} else if f.Type == "checkbox" {
				out[f.Key] = false
			}
			continue
		}
		v := vals[0]
		switch f.Type {
		case "text", "long_text":
			max := 500
			if f.Type == "long_text" {
				max = 5000
			}
			if utf8.RuneCountInString(v) > max {
				errs[f.Key] = fmt.Sprintf("Keep this under %d characters.", max)
				continue
			}
			out[f.Key] = v
		case "email":
			e, err := NormalizeEmail(v)
			if err != nil {
				errs[f.Key] = "Enter a valid email address."
				continue
			}
			out[f.Key] = e
		case "phone":
			if !phoneRe.MatchString(v) {
				errs[f.Key] = "Enter a phone number, digits with an optional +."
				continue
			}
			out[f.Key] = v
		case "number":
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				errs[f.Key] = "Enter a number."
				continue
			}
			out[f.Key] = n
		case "date":
			if _, err := time.Parse("2006-01-02", v); err != nil {
				errs[f.Key] = "Enter a date."
				continue
			}
			out[f.Key] = v
		case domain.FieldBirthDate:
			d, err := time.Parse("2006-01-02", v)
			if err != nil || d.Year() < 1900 || d.After(time.Now()) {
				errs[f.Key] = "Enter your date of birth."
				continue
			}
			out[f.Key] = v
		case "select":
			if !slices.Contains(f.Options, v) {
				errs[f.Key] = "Choose one of the options."
				continue
			}
			out[f.Key] = v
		case "multi_select":
			chosen := []string{}
			for _, x := range vals {
				if !slices.Contains(f.Options, x) {
					errs[f.Key] = "Choose from the options."
					break
				}
				if !slices.Contains(chosen, x) {
					chosen = append(chosen, x)
				}
			}
			out[f.Key] = chosen
		case "checkbox", "consent":
			out[f.Key] = true
		case "url":
			u, err := url.Parse(v)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(v) > 500 {
				errs[f.Key] = "Enter a full web address starting with https://."
				continue
			}
			out[f.Key] = v
		}
	}
	return out
}

// BirthDate is the answer to the form's birth date question, if any, as
// checked by CheckAnswers (YYYY-MM-DD).
func BirthDate(fields []domain.FormField, answers map[string]any) string {
	for _, f := range fields {
		if f.Type == domain.FieldBirthDate {
			v, _ := answers[f.Key].(string)
			return v
		}
	}
	return ""
}

// AgeOn is the date an event counts ages on for the under-18 check: its
// own setting, else its start date in its time zone; zero when neither.
func AgeOn(ev *domain.Event) time.Time {
	if ev.AgeOn != nil {
		return *ev.AgeOn
	}
	if ev.StartsAt != nil {
		if loc, err := time.LoadLocation(ev.TimeZone); err == nil {
			y, m, d := ev.StartsAt.In(loc).Date()
			return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		}
	}
	return time.Time{}
}

// ---- Organizer: participants ----------------------------------------------------

func (s *RegistrationService) Participants(ctx context.Context, es authz.EventScope, q domain.ParticipantQuery) ([]domain.EventParticipant, domain.ParticipantCounts, error) {
	if err := es.Require(authz.EventDashboard); err != nil {
		return nil, nil, err
	}
	if q.Status != "" && !slices.Contains(domain.ParticipantStatuses, q.Status) {
		q.Status = ""
	}
	q.Q = strings.TrimSpace(q.Q)
	return s.store.ListParticipants(ctx, es, q)
}

// SetStatus approves, cancels, restores or moves someone off the waitlist.
func (s *RegistrationService) SetStatus(ctx context.Context, es authz.EventScope, participantID, status string) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	if !slices.Contains(domain.ParticipantStatuses, status) {
		return invalid("unknown status %q", status)
	}
	return s.store.SetStatus(ctx, es, participantID, status)
}

// PassFor loads a participant's pass for an organizer (preview, download).
func (s *RegistrationService) PassFor(ctx context.Context, es authz.EventScope, participantID string) (*domain.PassContext, error) {
	if err := es.Require(authz.EventDashboard); err != nil {
		return nil, err
	}
	if _, err := s.store.GetParticipant(ctx, es, participantID); err != nil { // in this event, or not found
		return nil, err
	}
	return s.passes.PassContext(ctx, es.OrgID(), participantID)
}

// Pass check results. The scanner (M4) shows these with color and text.
const (
	CheckValid        = "valid"         // registered: let them in
	CheckNotConfirmed = "not_confirmed" // waiting for approval or on the waitlist
	CheckCancelled    = "cancelled"
	CheckWrongEvent   = "wrong_event" // a real pass, for another event
	CheckInvalid      = "invalid"     // not a pass, or forged
)

// PassCheck is what checking a scanned QR code found.
type PassCheck struct {
	Result      string                   `json:"result"`
	Message     string                   `json:"message"`
	Participant *domain.EventParticipant `json:"participant,omitempty"`
}

// ParticipantChecker loads a participant for a pass check
// (postgres.RegistrationRepo).
type ParticipantChecker interface {
	CheckParticipant(ctx context.Context, orgID, accountID, eventID, participantID string) (*domain.EventParticipant, error)
}

// CheckPass decides whether a scanned QR is a valid pass for this event.
func (s *RegistrationService) CheckPass(ctx context.Context, es authz.EventScope, ps *PassService, checker ParticipantChecker, payload string) (*PassCheck, error) {
	if err := es.Require(authz.EventLookup); err != nil {
		return nil, err
	}
	scanned, err := ps.Parse(payload)
	if err != nil {
		return &PassCheck{Result: CheckInvalid, Message: "Not a valid pass"}, nil
	}
	if scanned.Claims.OrgID != es.OrgID() || scanned.Claims.EventID != es.EventID() {
		return &PassCheck{Result: CheckWrongEvent, Message: "Pass is for another event"}, nil
	}
	p, err := checker.CheckParticipant(ctx, es.OrgID(), es.AccountID(), es.EventID(), scanned.Claims.ParticipantID)
	if errors.Is(err, domain.ErrNotFound) {
		return &PassCheck{Result: CheckInvalid, Message: "This pass no longer exists"}, nil
	}
	if err != nil {
		return nil, err
	}
	switch {
	case scanned.Claims.PassID != p.PassID:
		return &PassCheck{Result: CheckInvalid, Message: "This pass was replaced"}, nil
	}
	pc := &PassCheck{Participant: p}
	switch p.Status {
	case domain.PartRegistered:
		pc.Result, pc.Message = CheckValid, "Registered"
	case domain.PartCancelled:
		pc.Result, pc.Message = CheckCancelled, "Registration cancelled"
	case domain.PartPending:
		pc.Result, pc.Message = CheckNotConfirmed, "Waiting for approval"
	default:
		pc.Result, pc.Message = CheckNotConfirmed, "On the waitlist"
	}
	return pc, nil
}
