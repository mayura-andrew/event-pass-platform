package service

// Google Sheets sync and Google Form import (SaaS milestone M5).
//
// An organizer connects a Google account per event and kind. The refresh
// token is sealed with AES-GCM (bound to the integration's ID) and only the
// worker opens it. Sheets: the platform creates a spreadsheet and rewrites
// its Participants, Check-ins and Workshops tabs after changes (debounced).
// Forms: the worker polls responses and registers each one once, keyed by
// its response ID.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/repository/postgres"
	"github.com/mayura-andrew/event-pass-platform/internal/secrets"
)

// IntegrationStore persists integrations (postgres.IntegrationRepo).
type IntegrationStore interface {
	List(ctx context.Context, es authz.EventScope) ([]domain.Integration, error)
	Get(ctx context.Context, es authz.EventScope, kind string) (*domain.Integration, error)
	Connect(ctx context.Context, es authz.EventScope, kind, email string, config json.RawMessage, seal func(id string) ([]byte, error)) (*domain.Integration, error)
	SetConfig(ctx context.Context, es authz.EventScope, kind string, config json.RawMessage) error
	SyncNow(ctx context.Context, es authz.EventScope, kind string) error
	Disconnect(ctx context.Context, es authz.EventScope, kind string) error
	Secret(ctx context.Context, orgID, id string) ([]byte, error)

	Due(ctx context.Context, debounce, interval time.Duration) ([]domain.IntegrationJob, error)
	Finished(ctx context.Context, id, errMsg string, broken bool, imported int) error
	LoadSheetData(ctx context.Context, orgID, eventID string) (*postgres.SheetData, error)
	EventForImport(ctx context.Context, orgID, eventID string) (*domain.Event, []domain.FormField, error)
	ImportedRefs(ctx context.Context, orgID, eventID string) (map[string]bool, error)
}

// Registrar creates participants (postgres.RegistrationRepo.Register).
type Registrar interface {
	Register(ctx context.Context, ev *domain.Event, p domain.EventParticipant, passID func() string) (*domain.EventParticipant, error)
}

type IntegrationService struct {
	store    IntegrationStore
	reg      Registrar
	oauth    *GoogleOAuth // nil: Google isn't configured
	api      *GoogleAPI
	box      *secrets.Box
	redirect string // APP_BASE_URL + /auth/google/integration/callback

	Debounce time.Duration // Sheets: wait this long after a sync before the next
	Interval time.Duration // forms are polled, and failures retried, this often
	Tick     time.Duration // how often Run looks for due work
}

func NewIntegrationService(store IntegrationStore, reg Registrar, oauth *GoogleOAuth, api *GoogleAPI, box *secrets.Box, redirect string) *IntegrationService {
	return &IntegrationService{store: store, reg: reg, oauth: oauth, api: api, box: box, redirect: redirect,
		Debounce: 30 * time.Second, Interval: 60 * time.Second, Tick: 10 * time.Second}
}

// Configured says whether Google is set up (client ID and secret).
func (s *IntegrationService) Configured() bool { return s != nil && s.oauth != nil }

var ErrGoogleNotConfigured = fmt.Errorf("%w: Google isn't set up on this server (GOOGLE_OAUTH_CLIENT_ID)", domain.ErrConflict)

func kindScopes(kind string) ([]string, error) {
	switch kind {
	case domain.IntegrationSheets:
		return ScopeSheets, nil
	case domain.IntegrationForm:
		return ScopeForms, nil
	}
	return nil, invalid("unknown integration %q", kind)
}

func (s *IntegrationService) List(ctx context.Context, es authz.EventScope) ([]domain.Integration, error) {
	if err := es.Require(authz.EventEdit); err != nil {
		return nil, err
	}
	return s.store.List(ctx, es)
}

// ConsentURL is where the organizer goes to grant access for kind.
func (s *IntegrationService) ConsentURL(es authz.EventScope, kind, state, verifier string) (string, error) {
	if err := es.Require(authz.EventEdit); err != nil {
		return "", err
	}
	scopes, err := kindScopes(kind)
	if err != nil {
		return "", err
	}
	if !s.Configured() {
		return "", ErrGoogleNotConfigured
	}
	return s.oauth.ConsentURL(s.redirect, state, verifier, scopes), nil
}

// Complete finishes the consent: it exchanges the code, stores the sealed
// refresh token and, for Sheets, creates the spreadsheet.
func (s *IntegrationService) Complete(ctx context.Context, es authz.EventScope, ev *domain.Event, kind, code, verifier string) (*domain.Integration, error) {
	if err := es.Require(authz.EventEdit); err != nil {
		return nil, err
	}
	if _, err := kindScopes(kind); err != nil {
		return nil, err
	}
	if !s.Configured() {
		return nil, ErrGoogleNotConfigured
	}
	tok, err := s.oauth.ExchangeTokens(ctx, code, verifier, s.redirect)
	if err != nil {
		return nil, err
	}
	var config any = domain.FormConfig{}
	if kind == domain.IntegrationSheets {
		title := ev.Name + " · participants"
		id, link, err := s.api.CreateSpreadsheet(ctx, tok.Access, title, sheetTabs)
		if err != nil {
			return nil, err
		}
		config = domain.SheetsConfig{SpreadsheetID: id, URL: link, Title: title}
	}
	cfg, _ := json.Marshal(config)
	return s.store.Connect(ctx, es, kind, tok.Email, cfg, func(id string) ([]byte, error) {
		return s.box.Seal([]byte(tok.Refresh), []byte(id))
	})
}

func (s *IntegrationService) SyncNow(ctx context.Context, es authz.EventScope, kind string) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	return s.store.SyncNow(ctx, es, kind)
}

func (s *IntegrationService) Disconnect(ctx context.Context, es authz.EventScope, kind string) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	return s.store.Disconnect(ctx, es, kind)
}

// accessToken opens an integration's refresh token and trades it for an
// access token.
func (s *IntegrationService) accessToken(ctx context.Context, orgID, id string, sealed []byte) (string, error) {
	if s.oauth == nil {
		return "", ErrGoogleNotConfigured
	}
	if sealed == nil {
		var err error
		if sealed, err = s.store.Secret(ctx, orgID, id); err != nil {
			return "", err
		}
	}
	refresh, err := s.box.Open(sealed, []byte(id))
	if err != nil {
		return "", fmt.Errorf("the saved Google access can't be read (was INTEGRATIONS_KEY changed?): %w", err)
	}
	return s.oauth.AccessToken(ctx, string(refresh))
}

var formIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{20,100}$`)

// FormID takes a form's ID or any of its links (edit, view or share).
func FormID(s string) (string, error) {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		for i, p := range parts {
			if p == "d" && i+1 < len(parts) {
				s = parts[i+1]
				if s == "e" && i+2 < len(parts) {
					// /forms/d/e/<published id>/viewform: not the form ID
					return "", invalid("that's the form's public link; paste the link from the form editor's address bar")
				}
				break
			}
		}
	}
	if !formIDRe.MatchString(s) {
		return "", invalid("paste the Google Form's link from the form editor")
	}
	return s, nil
}

// LinkForm reads the form's questions so the organizer can map them.
func (s *IntegrationService) LinkForm(ctx context.Context, es authz.EventScope, link string) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	in, err := s.store.Get(ctx, es, domain.IntegrationForm)
	if err != nil {
		return err
	}
	formID, err := FormID(link)
	if err != nil {
		return err
	}
	tok, err := s.accessToken(ctx, es.OrgID(), in.ID, nil)
	if err != nil {
		return err
	}
	form, err := s.api.GetForm(ctx, tok, formID)
	if err != nil {
		return fmt.Errorf("%w: couldn't open that form with %s's Google account: %v", domain.ErrValidation, in.GoogleEmail, err)
	}
	cfg := domain.FormConfig{FormID: formID, Title: form.Title, Questions: form.Questions}
	// Guess the obvious questions.
	for _, q := range form.Questions {
		t := strings.ToLower(q.Title)
		switch {
		case cfg.NameQuestion == "" && strings.Contains(t, "name"):
			cfg.NameQuestion = q.ID
		case cfg.EmailQuestion == "" && strings.Contains(t, "email") && !form.CollectsEmails:
			cfg.EmailQuestion = q.ID
		}
	}
	b, _ := json.Marshal(cfg)
	return s.store.SetConfig(ctx, es, domain.IntegrationForm, b)
}

// MapForm saves which question is the name, which the email ("" = the
// respondent's address Google collects) and which go to the event's
// questions, then starts importing.
func (s *IntegrationService) MapForm(ctx context.Context, es authz.EventScope, fields []domain.FormField, name, email string, m map[string]string) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	in, err := s.store.Get(ctx, es, domain.IntegrationForm)
	if err != nil {
		return err
	}
	var cfg domain.FormConfig
	_ = json.Unmarshal(in.Config, &cfg)
	if cfg.FormID == "" {
		return invalid("link a form first")
	}
	known := map[string]bool{}
	for _, q := range cfg.Questions {
		known[q.ID] = true
	}
	keys := map[string]bool{}
	for _, f := range fields {
		keys[f.Key] = true
	}
	if !known[name] {
		return invalid("choose the question that asks for the full name")
	}
	if email != "" && !known[email] {
		return invalid("choose the question that asks for the email address")
	}
	clean := map[string]string{}
	for q, key := range m {
		if key == "" {
			continue
		}
		if !known[q] || !keys[key] {
			return invalid("a question is mapped to something that doesn't exist")
		}
		clean[q] = key
	}
	cfg.NameQuestion, cfg.EmailQuestion, cfg.Map, cfg.Ready = name, email, clean, true
	b, _ := json.Marshal(cfg)
	if err := s.store.SetConfig(ctx, es, domain.IntegrationForm, b); err != nil {
		return err
	}
	return s.store.SyncNow(ctx, es, domain.IntegrationForm)
}

// ---- Worker -----------------------------------------------------------------------

// Run syncs due integrations until ctx ends.
func (s *IntegrationService) Run(ctx context.Context) {
	t := time.NewTicker(s.Tick)
	defer t.Stop()
	for {
		if n, err := s.RunOnce(ctx); err != nil {
			slog.Error("integrations", "err", err)
		} else if n > 0 {
			slog.Info("integrations synced", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce runs every due integration and says how many ran.
func (s *IntegrationService) RunOnce(ctx context.Context) (int, error) {
	if s.oauth == nil {
		return 0, nil
	}
	jobs, err := s.store.Due(ctx, s.Debounce, s.Interval)
	if err != nil {
		return 0, err
	}
	for _, j := range jobs {
		imported, err := s.run(ctx, j)
		msg, broken := "", false
		if err != nil {
			msg, broken = err.Error(), errors.Is(err, ErrGoogleRevoked)
			slog.Warn("integration failed", "id", j.ID, "kind", j.Kind, "err", err)
		}
		if err := s.store.Finished(ctx, j.ID, msg, broken, imported); err != nil {
			return 0, err
		}
	}
	return len(jobs), nil
}

func (s *IntegrationService) run(ctx context.Context, j domain.IntegrationJob) (int, error) {
	tok, err := s.accessToken(ctx, j.OrgID, j.ID, j.Secret)
	if err != nil {
		return 0, err
	}
	if j.Kind == domain.IntegrationSheets {
		return 0, s.syncSheet(ctx, j, tok)
	}
	return s.importForm(ctx, j, tok)
}

var sheetTabs = []string{"Participants", "Check-ins", "Workshops"}

func (s *IntegrationService) syncSheet(ctx context.Context, j domain.IntegrationJob, tok string) error {
	var cfg domain.SheetsConfig
	if err := json.Unmarshal(j.Config, &cfg); err != nil || cfg.SpreadsheetID == "" {
		return errors.New("the spreadsheet is missing; disconnect and connect again")
	}
	d, err := s.store.LoadSheetData(ctx, j.OrgID, j.EventID)
	if err != nil {
		return err
	}
	return s.api.ReplaceTabs(ctx, tok, cfg.SpreadsheetID, SheetRows(d), sheetTabs)
}

// SheetRows lays out the spreadsheet: one header row per tab, times in the
// event's time zone. Values are written RAW, so text that looks like a
// formula stays text.
func SheetRows(d *postgres.SheetData) map[string][][]any {
	loc, err := time.LoadLocation(d.Event.TimeZone)
	if err != nil {
		loc = time.UTC
	}
	at := func(t *time.Time) any {
		if t == nil {
			return ""
		}
		return t.In(loc).Format("2006-01-02 15:04")
	}
	head := []any{"Pass ID", "Full name", "Email", "Status", "Registered", "Source"}
	for _, f := range d.Fields {
		head = append(head, f.Label)
	}
	asksAge := slices.ContainsFunc(d.Fields, func(f domain.FormField) bool { return f.Type == domain.FieldBirthDate })
	if asksAge {
		label := "Under 18"
		if day := AgeOn(&d.Event); !day.IsZero() {
			label += " on " + day.Format("2 Jan 2006")
		}
		head = append(head, label)
	}
	people := [][]any{head}
	for _, p := range d.Participants {
		reg := p.RegisteredAt
		row := []any{p.PassID, p.FullName, p.Email, p.Status, at(&reg), p.Source}
		for _, f := range d.Fields {
			row = append(row, answerText(p.Answers[f.Key]))
		}
		if asksAge {
			minor := ""
			if p.Age != nil {
				minor = answerText(p.Minor())
			}
			row = append(row, minor)
		}
		people = append(people, row)
	}
	checkins := [][]any{{"Time", "Pass ID", "Full name", "Checkpoint", "Staff", "Undone"}}
	for _, c := range d.Checkins {
		created := c.CreatedAt
		checkins = append(checkins, []any{at(&created), c.PassID, c.FullName, c.Checkpoint, c.StaffName, at(c.UndoneAt)})
	}
	workshops := [][]any{{"Title", "Room", "Starts", "Ends", "Capacity", "Signed up"}}
	for _, w := range d.Workshops {
		capacity := any("")
		if w.Capacity != nil {
			capacity = *w.Capacity
		}
		workshops = append(workshops, []any{w.Title, w.Room, at(w.StartsAt), at(w.EndsAt), capacity, w.SignedUp})
	}
	return map[string][][]any{"Participants": people, "Check-ins": checkins, "Workshops": workshops}
}

// importForm registers new responses. A response is imported once (its ID
// is the participant's external reference); someone already registered with
// the same email is skipped.
func (s *IntegrationService) importForm(ctx context.Context, j domain.IntegrationJob, tok string) (int, error) {
	var cfg domain.FormConfig
	if err := json.Unmarshal(j.Config, &cfg); err != nil || !cfg.Ready {
		return 0, nil
	}
	ev, fields, err := s.store.EventForImport(ctx, j.OrgID, j.EventID)
	if err != nil {
		return 0, err
	}
	seen, err := s.store.ImportedRefs(ctx, j.OrgID, j.EventID)
	if err != nil {
		return 0, err
	}
	responses, err := s.api.ListResponses(ctx, tok, cfg.FormID)
	if err != nil {
		return 0, err
	}
	sort.Slice(responses, func(a, b int) bool { return responses[a].Submitted.Before(responses[b].Submitted) })
	imported, skipped := 0, 0
	for _, r := range responses {
		if seen[r.ID] {
			continue
		}
		p, ok := mapResponse(cfg, fields, r)
		if !ok {
			skipped++
			continue
		}
		_, err := s.reg.Register(ctx, ev, p, func() string { return NewPassID(ev.PassPrefix) })
		var already *domain.AlreadyRegisteredError
		switch {
		case err == nil:
			imported++
		case errors.As(err, &already), errors.Is(err, postgres.ErrAlreadyImported):
			// registered another way, or imported by a concurrent run
		case errors.Is(err, domain.ErrConflict): // the event is full
			return imported, err
		default:
			return imported, err
		}
	}
	if skipped == 1 {
		return imported, errors.New("1 response has no valid name or email and wasn't imported")
	} else if skipped > 1 {
		return imported, fmt.Errorf("%d responses have no valid name or email and weren't imported", skipped)
	}
	return imported, nil
}

// mapResponse turns a response into a participant, or false when it has no
// usable name or email. Answers that don't fit the event's questions (an
// option that isn't offered, say) are left out rather than refusing the
// whole response.
func mapResponse(cfg domain.FormConfig, fields []domain.FormField, r FormResponse) (domain.EventParticipant, bool) {
	first := func(q string) string {
		if v := r.Answers[q]; len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	name := first(cfg.NameQuestion)
	rawEmail := r.RespondentEmail
	if cfg.EmailQuestion != "" {
		rawEmail = first(cfg.EmailQuestion)
	}
	email, err := NormalizeEmail(rawEmail)
	if n := len([]rune(name)); err != nil || n < 2 || n > 150 || !ValidDisplayName(name) {
		return domain.EventParticipant{}, false
	}
	in := map[string][]string{}
	for q, key := range cfg.Map {
		in[key] = append(in[key], r.Answers[q]...)
	}
	errs := FieldErrors{}
	answers := CheckAnswers(fields, in, errs)
	for key := range errs {
		delete(answers, key)
	}
	return domain.EventParticipant{Email: email, FullName: name, Answers: answers, Source: "import", ExternalRef: r.ID,
		BirthDate: BirthDate(fields, answers)}, true
}
