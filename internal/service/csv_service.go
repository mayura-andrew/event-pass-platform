package service

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nasa-spaceapps/nsac-qr/internal/auth"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// IngestStore is implemented by postgres.IngestRepo.
type IngestStore interface {
	ExistingEmails(ctx context.Context, emails []string) (map[string]string, error)
	VenueIDs(ctx context.Context) (map[string]bool, error)
	FieldDefinitions(ctx context.Context) ([]domain.FieldDefinition, error)
	// BulkInsertWithAutoID also registers newFields (custom fields first seen
	// in this upload) in the same transaction.
	BulkInsertWithAutoID(ctx context.Context, rows []domain.NewParticipant, newFields []domain.NewField, sign func(membershipID, email string) string) ([]domain.CreatedParticipant, []domain.NewParticipant, error)
}

// ImportColumns are the core CSV columns, in template order. full_name and
// email are required; the others may be left out or blank. Any other column
// is a custom field (see csv_dynamic_service.go).
var ImportColumns = []string{"full_name", "email", "team_name", "date_of_birth", "venue_id"}

// IngestService adds confirmed delegates from the dashboard, one at a time or
// from a CSV upload. Membership IDs come from the database sequence and QR
// signatures are computed here, so neither is ever typed by an organizer.
type IngestService struct {
	store IngestStore
	key   []byte // HMAC_SECRET_KEY; must match the key scripts/seed signs with
	today func() time.Time
}

func NewIngestService(store IngestStore, qrKey []byte) *IngestService {
	return &IngestService{store: store, key: qrKey, today: time.Now}
}

func (s *IngestService) sign(membershipID, email string) string {
	return auth.QRSignature(membershipID, email, s.key)
}

func (s *IngestService) fieldsByKey(ctx context.Context) (map[string]domain.FieldDefinition, error) {
	list, err := s.store.FieldDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	defs := make(map[string]domain.FieldDefinition, len(list))
	for _, d := range list {
		defs[d.Key] = d
	}
	return defs, nil
}

// ---- Single entry ------------------------------------------------------------

// Create adds one delegate. actorID is the organizer from the JWT.
func (s *IngestService) Create(ctx context.Context, actorID string, req domain.ParticipantCreateRequest) (*domain.CreatedParticipant, error) {
	venues, err := s.store.VenueIDs(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.validate(req, venues)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrValidation, err)
	}
	if len(req.CustomAttributes) > 0 {
		defs, err := s.fieldsByKey(ctx)
		if err != nil {
			return nil, err
		}
		if p.CustomAttributes, err = customFromForm(req.CustomAttributes, defs); err != nil {
			return nil, fmt.Errorf("%w: %s", domain.ErrValidation, err)
		}
	}
	existing, err := s.store.ExistingEmails(ctx, []string{p.Email})
	if err != nil {
		return nil, err
	}
	if id, ok := existing[p.Email]; ok {
		return nil, fmt.Errorf("%w: %s is already registered as %s", domain.ErrConflict, p.Email, id)
	}

	created, _, err := s.store.BulkInsertWithAutoID(ctx, []domain.NewParticipant{p}, nil, s.sign)
	if err != nil {
		return nil, err
	}
	if len(created) == 0 {
		// Registered by someone else between the check and the insert.
		return nil, fmt.Errorf("%w: %s was registered a moment ago by another organizer", domain.ErrConflict, p.Email)
	}
	c := created[0]
	slog.Info("participant created", "actor", actorID, "participant", c.ID, "membership_id", c.MembershipID)
	return &c, nil
}

// ---- CSV import --------------------------------------------------------------

// ImportCSV reads a CSV with the ImportColumns header from r as it streams in,
// then inserts every valid, new row in one batch. Invalid rows and emails
// already registered are reported, not inserted, so uploading the corrected
// file again adds only what was missing.
//
// Extra columns become custom fields: each row's values are stored in
// custom_attributes, and columns not seen before are registered in the same
// transaction (as text fields, hidden from scanners and filters until an
// organizer turns them on).
//
// A file that can't be read as a whole (no header, broken quoting, too many
// rows) is a validation error and nothing is inserted.
func (s *IngestService) ImportCSV(ctx context.Context, actorID string, r io.Reader) (*domain.ImportResult, error) {
	venues, err := s.store.VenueIDs(ctx)
	if err != nil {
		return nil, err
	}
	defs, err := s.fieldsByKey(ctx)
	if err != nil {
		return nil, err
	}

	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // spreadsheet exports drop or add trailing columns
	cr.ReuseRecord = true

	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: the file is empty", domain.ErrValidation)
	}
	if err != nil {
		return nil, csvError(err)
	}
	plan, err := planColumns(header, defs)
	if err != nil {
		return nil, err
	}

	res := &domain.ImportResult{
		Created:        []domain.CreatedParticipant{},
		Duplicates:     []domain.ImportIssue{},
		Errors:         []domain.ImportIssue{},
		CustomFields:   []string{},
		NewFields:      []string{},
		IgnoredColumns: append([]string{}, plan.ignored...),
	}
	for _, c := range plan.custom {
		res.CustomFields = append(res.CustomFields, c.key)
	}
	var valid []domain.NewParticipant
	firstLine := make(map[string]int) // email -> line it first appeared on

	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, csvError(err)
		}
		line, _ := cr.FieldPos(0)
		get := func(name string) string {
			if i, ok := plan.core[name]; ok && i < len(rec) {
				return rec[i]
			}
			return ""
		}
		req := domain.ParticipantCreateRequest{
			FullName:    get("full_name"),
			Email:       get("email"),
			TeamName:    get("team_name"),
			DateOfBirth: get("date_of_birth"),
			VenueID:     get("venue_id"),
		}
		custom, customErr := plan.customValues(rec)
		if blankRow(req) && len(custom) == 0 && customErr == nil {
			continue // spreadsheets often export trailing ",,,," rows
		}
		res.TotalRows++
		if res.TotalRows > domain.MaxImportRows {
			return nil, fmt.Errorf("%w: the file has more than %d delegates; split it and upload each part",
				domain.ErrValidation, domain.MaxImportRows)
		}

		p, err := s.validate(req, venues)
		if err == nil {
			err = customErr
		}
		if err != nil {
			res.Errors = append(res.Errors, domain.ImportIssue{
				Line: line, Email: strings.TrimSpace(req.Email), Message: err.Error(),
			})
			continue
		}
		p.CustomAttributes = custom
		if first, dup := firstLine[p.Email]; dup {
			res.Duplicates = append(res.Duplicates, domain.ImportIssue{
				Line: line, Email: p.Email, Message: fmt.Sprintf("repeats line %d of this file", first),
			})
			continue
		}
		firstLine[p.Email] = line
		p.Line = line
		valid = append(valid, p)
	}

	if len(valid) > 0 {
		emails := make([]string, len(valid))
		for i, p := range valid {
			emails[i] = p.Email
		}
		existing, err := s.store.ExistingEmails(ctx, emails)
		if err != nil {
			return nil, err
		}
		fresh := valid[:0]
		for _, p := range valid {
			if id, ok := existing[p.Email]; ok {
				res.Duplicates = append(res.Duplicates, domain.ImportIssue{
					Line: p.Line, Email: p.Email, Message: "already registered as " + id,
				})
				continue
			}
			fresh = append(fresh, p)
		}

		newFields := plan.fieldsToRegister(fresh)
		created, skipped, err := s.store.BulkInsertWithAutoID(ctx, fresh, newFields, s.sign)
		if err != nil {
			return nil, err
		}
		for _, f := range newFields {
			res.NewFields = append(res.NewFields, f.Key)
		}
		res.Created = append(res.Created, created...)
		for _, p := range skipped {
			res.Duplicates = append(res.Duplicates, domain.ImportIssue{
				Line: p.Line, Email: p.Email, Message: "registered by another organizer during this import",
			})
		}
	}

	slog.Info("participants imported", "actor", actorID, "rows", res.TotalRows,
		"created", len(res.Created), "duplicates", len(res.Duplicates), "errors", len(res.Errors),
		"new_fields", res.NewFields)
	return res, nil
}

func csvError(err error) error {
	var pe *csv.ParseError
	if errors.As(err, &pe) {
		return fmt.Errorf("%w: line %d is not valid CSV (%v); check for an unclosed quote", domain.ErrValidation, pe.StartLine, pe.Err)
	}
	return err
}

func blankRow(r domain.ParticipantCreateRequest) bool {
	return strings.TrimSpace(r.FullName+r.Email+r.TeamName+r.DateOfBirth+r.VenueID) == ""
}

// ---- Validation --------------------------------------------------------------

// validate normalizes one delegate. Errors are plain sentences for the row
// error log; callers wrap them in ErrValidation where needed.
func (s *IngestService) validate(req domain.ParticipantCreateRequest, venues map[string]bool) (domain.NewParticipant, error) {
	var p domain.NewParticipant

	p.FullName = strings.Join(strings.Fields(req.FullName), " ")
	switch {
	case p.FullName == "":
		return p, errors.New("full name is required")
	case utf8.RuneCountInString(p.FullName) > domain.MaxParticipantNameLen:
		return p, fmt.Errorf("full name is longer than %d characters", domain.MaxParticipantNameLen)
	case !ValidDisplayName(p.FullName):
		return p, errors.New("full name contains characters that aren't allowed")
	}

	email, err := normalizeStaffEmail(req.Email)
	if err != nil {
		if strings.TrimSpace(req.Email) == "" {
			return p, errors.New("email is required")
		}
		return p, fmt.Errorf("%q is not a valid email address", strings.TrimSpace(req.Email))
	}
	p.Email = email

	p.TeamName = strings.Join(strings.Fields(req.TeamName), " ")
	if utf8.RuneCountInString(p.TeamName) > domain.MaxParticipantTeamLen {
		return p, fmt.Errorf("team name is longer than %d characters", domain.MaxParticipantTeamLen)
	}
	if p.TeamName != "" && !ValidDisplayName(p.TeamName) {
		return p, errors.New("team name contains characters that aren't allowed")
	}

	if raw := strings.TrimSpace(req.DateOfBirth); raw != "" {
		dob, err := time.Parse("2006-01-02", raw)
		if err != nil {
			return p, fmt.Errorf("date of birth %q must be written YYYY-MM-DD", raw)
		}
		// Same range as the chk_participants_dob_range constraint (002).
		if dob.Year() < 1900 || dob.After(s.today()) {
			return p, fmt.Errorf("date of birth %s is not a plausible birth date", raw)
		}
		p.DateOfBirth = dob.Format("2006-01-02")
	}

	if raw := strings.ToLower(strings.TrimSpace(req.VenueID)); raw != "" {
		if !IsUUID(raw) {
			return p, fmt.Errorf("venue_id %q is not a venue ID; copy it from the venue list", strings.TrimSpace(req.VenueID))
		}
		if !venues[raw] {
			return p, fmt.Errorf("venue_id %s doesn't match any venue", raw)
		}
		p.VenueID = raw
	}
	return p, nil
}
