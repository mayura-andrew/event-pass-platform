package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type AdminStore interface {
	GetByEmail(ctx context.Context, email string) (*domain.Admin, error)
	GetByID(ctx context.Context, id string) (*domain.Admin, error)
	SetPassword(ctx context.Context, id, hash string) error
	TouchLogin(ctx context.Context, id string) error
}

type TokenIssuer interface {
	Issue(adminID, name, role string) (string, time.Time, error)
}

type LoginResult struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	AdminID   string    `json:"admin_id"`
	FullName  string    `json:"full_name"`
	Role      string    `json:"role"`
	// True after signing in with an emailed temporary password. The token then
	// only works for POST /api/v1/auth/change-password.
	ForcePasswordChange bool `json:"force_password_change"`
}

type AuthService struct {
	admins AdminStore
	tokens TokenIssuer
	// dummyHash is compared against when the email is unknown so the response
	// time doesn't reveal which admin emails exist.
	dummyHash []byte
}

func NewAuthService(a AdminStore, t TokenIssuer) *AuthService {
	h, _ := bcrypt.GenerateFromPassword([]byte("timing-equalizer"), bcrypt.DefaultCost)
	return &AuthService{admins: a, tokens: t, dummyHash: h}
}

func (s *AuthService) Login(ctx context.Context, email, password string) (*LoginResult, error) {
	email = strings.ToLower(strings.TrimSpace(email))

	admin, err := s.admins.GetByEmail(ctx, email)
	if errors.Is(err, domain.ErrNotFound) {
		_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password))
		return nil, domain.ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(password)) != nil || !admin.IsActive {
		return nil, domain.ErrInvalidCredentials
	}

	if err := s.admins.TouchLogin(ctx, admin.ID); err != nil {
		slog.Warn("record last login", "admin", admin.ID, "err", err) // informational only
	}
	return s.issue(admin)
}

func (s *AuthService) issue(admin *domain.Admin) (*LoginResult, error) {
	token, exp, err := s.tokens.Issue(admin.ID, admin.FullName, admin.Role)
	if err != nil {
		return nil, err
	}
	return &LoginResult{
		Token: token, ExpiresAt: exp, AdminID: admin.ID, FullName: admin.FullName,
		Role: admin.Role, ForcePasswordChange: admin.ForcePasswordChange,
	}, nil
}

// ChangePassword replaces the password after checking the current one, clears
// the temporary-password flag and returns a fresh session.
func (s *AuthService) ChangePassword(ctx context.Context, adminID, current, next string) (*LoginResult, error) {
	switch {
	case utf8.RuneCountInString(next) < domain.MinPasswordLength:
		return nil, fmt.Errorf("%w: the new password must be at least %d characters", domain.ErrValidation, domain.MinPasswordLength)
	case len(next) > domain.MaxPasswordBytes:
		return nil, fmt.Errorf("%w: the new password is too long (max %d bytes)", domain.ErrValidation, domain.MaxPasswordBytes)
	case next == current:
		return nil, fmt.Errorf("%w: choose a password different from the temporary one", domain.ErrValidation)
	}

	admin, err := s.admins.GetByID(ctx, adminID)
	if err != nil {
		return nil, err
	}
	if !admin.IsActive || bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(current)) != nil {
		return nil, fmt.Errorf("%w: the current password is wrong", domain.ErrValidation)
	}
	if strings.Contains(strings.ToLower(next), strings.Split(admin.Email, "@")[0]) {
		return nil, fmt.Errorf("%w: the password must not contain your email name", domain.ErrValidation)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	if err := s.admins.SetPassword(ctx, admin.ID, string(hash)); err != nil {
		return nil, err
	}
	admin.ForcePasswordChange = false
	slog.Info("password changed", "admin", admin.ID)
	return s.issue(admin)
}
