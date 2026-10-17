package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type StaffStore interface {
	CreateStaff(ctx context.Context, n domain.NewStaff, beforeCommit func(*domain.StaffMember) error) (*domain.StaffMember, error)
	ListStaff(ctx context.Context) ([]domain.StaffMember, error)
	GetStaff(ctx context.Context, id string) (*domain.StaffMember, error)
	UpdateStaff(ctx context.Context, id string, u domain.StaffUpdateRequest, actorID string) (*domain.StaffMember, error)
	ResetTemporaryPassword(ctx context.Context, id, hash string, beforeCommit func(*domain.StaffMember) error) (*domain.StaffMember, error)
}

var roleLabels = map[string]string{
	domain.RoleSuperAdmin: "Superadmin",
	domain.RoleAdmin:      "Admin",
	domain.RoleScanner:    "Scanner",
}

type StaffService struct {
	store     StaffStore
	mailer    Mailer
	baseURL   string // e.g. https://pass.spaceappscolombo.org, no trailing slash
	eventName string
	logo      EmailLogo
}

func NewStaffService(store StaffStore, mailer Mailer, baseURL, eventName string, logo EmailLogo) *StaffService {
	return &StaffService{
		store:     store,
		mailer:    mailer,
		baseURL:   strings.TrimRight(baseURL, "/"),
		eventName: eventName,
		logo:      logo,
	}
}

// loginURL sends scanners to the scanner app and managers to the dashboard.
func (s *StaffService) loginURL(role string) string {
	if domain.IsManager(role) {
		return s.baseURL + "/admin/"
	}
	return s.baseURL + "/scanner.html"
}

func (s *StaffService) List(ctx context.Context) ([]domain.StaffMember, error) {
	return s.store.ListStaff(ctx)
}

// Create provisions an account with a generated temporary password and emails
// it. actorID is the superadmin from the JWT.
func (s *StaffService) Create(ctx context.Context, actorID string, req domain.StaffCreateRequest) (*domain.StaffCreateResult, error) {
	name, err := normalizeStaffName(req.FullName)
	if err != nil {
		return nil, err
	}
	addr, err := normalizeStaffEmail(req.Email)
	if err != nil {
		return nil, err
	}
	role := strings.ToLower(strings.TrimSpace(req.Role))
	if !domain.ValidRole(role) {
		return nil, fmt.Errorf("%w: role must be superadmin, admin or scanner", domain.ErrValidation)
	}
	grants, err := normalizeGrants(role, req.ScenarioIDs)
	if err != nil {
		return nil, err
	}

	temp, err := GenerateTempPassword()
	if err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(temp), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	member, err := s.store.CreateStaff(ctx, domain.NewStaff{
		FullName:     name,
		Email:        addr,
		Role:         role,
		PasswordHash: string(hash),
		ScenarioIDs:  grants,
		CreatedBy:    actorID,
	}, func(m *domain.StaffMember) error {
		// Inside the transaction: a failed send rolls the account back.
		return s.sendCredentials(ctx, m, temp, false)
	})
	if err != nil {
		return nil, err
	}
	slog.Info("staff created", "actor", actorID, "staff", member.ID, "role", member.Role, "grants", len(member.Grants))
	return &domain.StaffCreateResult{Staff: member, EmailSentTo: member.Email}, nil
}

// Update changes name, role, status or grants. An account can't deactivate or
// demote itself, so a superadmin can't lock everyone out by mistake.
func (s *StaffService) Update(ctx context.Context, actorID, id string, req domain.StaffUpdateRequest) (*domain.StaffMember, error) {
	if !IsUUID(id) {
		return nil, domain.ErrNotFound
	}
	if req.FullName != nil {
		n, err := normalizeStaffName(*req.FullName)
		if err != nil {
			return nil, err
		}
		req.FullName = &n
	}
	if req.Role != nil {
		r := strings.ToLower(strings.TrimSpace(*req.Role))
		if !domain.ValidRole(r) {
			return nil, fmt.Errorf("%w: role must be superadmin, admin or scanner", domain.ErrValidation)
		}
		req.Role = &r
	}
	if strings.EqualFold(id, actorID) {
		if req.IsActive != nil && !*req.IsActive {
			return nil, fmt.Errorf("%w: you can't deactivate your own account", domain.ErrValidation)
		}
		if req.Role != nil && *req.Role != domain.RoleSuperAdmin {
			return nil, fmt.Errorf("%w: you can't change your own role", domain.ErrValidation)
		}
	}
	if req.ScenarioIDs != nil {
		g, err := normalizeGrants(domain.RoleScanner, *req.ScenarioIDs)
		if err != nil {
			return nil, err
		}
		req.ScenarioIDs = &g
	}

	member, err := s.store.UpdateStaff(ctx, id, req, actorID)
	if err != nil {
		return nil, err
	}
	slog.Info("staff updated", "actor", actorID, "staff", id, "role", member.Role, "active", member.IsActive, "grants", len(member.Grants))
	return member, nil
}

// ResendCredentials issues a new temporary password and emails it. The old
// password stops working once the email has gone out.
func (s *StaffService) ResendCredentials(ctx context.Context, actorID, id string) (*domain.StaffCreateResult, error) {
	if !IsUUID(id) {
		return nil, domain.ErrNotFound
	}
	if strings.EqualFold(id, actorID) {
		return nil, fmt.Errorf("%w: use your own sign-in to change your password", domain.ErrValidation)
	}
	current, err := s.store.GetStaff(ctx, id)
	if err != nil {
		return nil, err
	}
	if !current.IsActive {
		return nil, fmt.Errorf("%w: activate the account before sending new credentials", domain.ErrValidation)
	}

	temp, err := GenerateTempPassword()
	if err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(temp), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	member, err := s.store.ResetTemporaryPassword(ctx, id, string(hash), func(m *domain.StaffMember) error {
		return s.sendCredentials(ctx, m, temp, true)
	})
	if err != nil {
		return nil, err
	}
	slog.Info("staff credentials reset", "actor", actorID, "staff", id)
	return &domain.StaffCreateResult{Staff: member, EmailSentTo: member.Email}, nil
}

func (s *StaffService) sendCredentials(ctx context.Context, m *domain.StaffMember, temp string, reset bool) error {
	checkpoints := make([]string, 0, len(m.Grants))
	for _, g := range m.Grants {
		checkpoints = append(checkpoints, g.Name)
	}
	subject, text, html, err := RenderCredentialsEmail(CredentialsEmail{
		FullName:     m.FullName,
		Email:        m.Email,
		TempPassword: temp,
		RoleLabel:    roleLabels[m.Role],
		LoginURL:     s.loginURL(m.Role),
		Checkpoints:  checkpoints,
		IsReset:      reset,
		EventName:    s.eventName,
		LogoCID:      s.logo.LogoCID(),
	})
	if err != nil {
		return err
	}
	err = s.mailer.Send(ctx, Message{
		To:      mail.Address{Name: m.FullName, Address: m.Email},
		Subject: subject,
		Text:    text,
		HTML:    html,
		Inline:  s.logo.Inline(),
	})
	if err != nil {
		// The SMTP error can contain server details; log it, return a clean one.
		slog.Error("credentials email failed", "staff_email", m.Email, "err", err)
		if errors.Is(err, domain.ErrEmailUnavailable) {
			return err // already a user-facing message (e.g. SMTP not configured)
		}
		return fmt.Errorf("%w: the email could not be sent, so nothing was saved; check the address and try again", domain.ErrEmailUnavailable)
	}
	return nil
}

// ---- Validation ----------------------------------------------------------------

func normalizeStaffName(raw string) (string, error) {
	n := strings.Join(strings.Fields(raw), " ")
	if n == "" || utf8.RuneCountInString(n) > domain.MaxStaffNameLen || !ValidDisplayName(n) {
		return "", fmt.Errorf("%w: name must be 1-%d characters", domain.ErrValidation, domain.MaxStaffNameLen)
	}
	return n, nil
}

// normalizeStaffEmail accepts a bare address only ("a@b.org", not
// "Name <a@b.org>") and lower-cases it, matching how logins look accounts up.
func normalizeStaffEmail(raw string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 255 || !strings.Contains(e[strings.LastIndexByte(e, '@')+1:], ".") {
		return "", fmt.Errorf("%w: enter a valid email address", domain.ErrValidation)
	}
	return e, nil
}

// normalizeGrants validates and de-duplicates scenario IDs. Managers can scan
// everything, so their grants are dropped rather than stored.
func normalizeGrants(role string, ids []string) ([]string, error) {
	if domain.IsManager(role) {
		return nil, nil
	}
	if len(ids) > domain.MaxStaffGrants {
		return nil, fmt.Errorf("%w: at most %d checkpoints", domain.ErrValidation, domain.MaxStaffGrants)
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if !IsUUID(id) {
			return nil, fmt.Errorf("%w: scenario_ids must be UUIDs", domain.ErrValidation)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}
