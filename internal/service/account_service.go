package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

// AccountStore persists accounts, emailed sign-in links and sessions.
type AccountStore interface {
	CreateLoginLink(ctx context.Context, tokenHash []byte, email, next string, expires time.Time) error
	RecentLoginLinks(ctx context.Context, email string, since time.Time) (int, error)
	ConsumeLoginLink(ctx context.Context, tokenHash []byte) (email, next string, err error)
	SignIn(ctx context.Context, provider, subject, email, name, avatarURL string) (*domain.Account, error)
	GetAccount(ctx context.Context, id string) (*domain.Account, error)
	CreateSession(ctx context.Context, accountID string, tokenHash []byte, csrf string, expires time.Time, userAgent, ip string) (*domain.Session, error)
	SessionByToken(ctx context.Context, tokenHash []byte) (*domain.Session, *domain.Account, error)
	RevokeSession(ctx context.Context, tokenHash []byte) error
}

const (
	// LoginLinkTTL: long enough to switch to the mail app, short enough that
	// an old email in an inbox isn't a standing key.
	LoginLinkTTL = 15 * time.Minute
	// loginLinksPerWindow caps links per address, so the form can't be used
	// to flood someone's inbox.
	loginLinksPerWindow = 5
	loginLinkWindow     = 15 * time.Minute
)

// AccountConfig is the platform-level settings for sign-in.
type AccountConfig struct {
	BaseURL    string        // APP_BASE_URL, for links in emails
	Product    string        // platform name in emails
	SessionTTL time.Duration // staff sessions: 12 h (spec)
}

type AccountService struct {
	store  AccountStore
	mailer Mailer
	google *GoogleOAuth // nil when Google sign-in isn't configured
	cfg    AccountConfig
}

func NewAccountService(store AccountStore, mailer Mailer, google *GoogleOAuth, cfg AccountConfig) *AccountService {
	return &AccountService{store: store, mailer: mailer, google: google, cfg: cfg}
}

// NewSecret returns 32 random bytes, base64url-encoded: session tokens, link
// tokens, OAuth state.
func NewSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken is how secrets are stored and looked up.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// NormalizeEmail trims and lower-cases an address and checks it parses.
func NormalizeEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || len(s) > 254 {
		return "", fmt.Errorf("%w: enter a valid email address", domain.ErrValidation)
	}
	return s, nil
}

// SafeNext keeps a post-sign-in destination on this site: a path, never
// another host ("//evil.example" or "https://...").
func SafeNext(next string) string {
	if len(next) > 500 || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.ContainsAny(next, "\\\r\n") {
		return ""
	}
	return next
}

// GoogleEnabled reports whether "Sign in with Google" is configured.
func (s *AccountService) GoogleEnabled() bool { return s.google != nil }

// Google returns the OAuth client, or nil.
func (s *AccountService) Google() *GoogleOAuth { return s.google }

// RequestEmailLink emails a one-time sign-in link. Unknown addresses get one
// too: the first sign-in creates the account.
func (s *AccountService) RequestEmailLink(ctx context.Context, email, next string) error {
	email, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	n, err := s.store.RecentLoginLinks(ctx, email, time.Now().Add(-loginLinkWindow))
	if err != nil {
		return err
	}
	if n >= loginLinksPerWindow {
		return fmt.Errorf("%w: several sign-in links were sent to this address recently; use the latest one or wait 15 minutes", domain.ErrRateLimited)
	}

	token := NewSecret()
	if err := s.store.CreateLoginLink(ctx, HashToken(token), email, SafeNext(next), time.Now().Add(LoginLinkTTL)); err != nil {
		return err
	}
	link := s.cfg.BaseURL + "/auth/email/verify?token=" + token
	text, html, err := ActionEmail{
		Product: s.cfg.Product,
		Heading: "Your sign-in link",
		Paragraph: []string{
			fmt.Sprintf("Use the button below to sign in to %s. The link works once, for the next 15 minutes.", s.cfg.Product),
		},
		Button:   "Sign in",
		URL:      link,
		Footnote: "Didn't ask to sign in? Ignore this email; nobody can use the link without access to your inbox.",
	}.Render()
	if err != nil {
		return err
	}
	if err := s.mailer.Send(ctx, Message{
		To:      mail.Address{Address: email},
		Subject: fmt.Sprintf("Sign in to %s", s.cfg.Product),
		Text:    text, HTML: html,
	}); err != nil {
		if errors.Is(err, domain.ErrEmailUnavailable) {
			return err
		}
		slog.Error("sign-in link email failed", "err", err)
		return fmt.Errorf("%w: the sign-in email could not be sent; try again shortly", domain.ErrEmailUnavailable)
	}
	return nil
}

// NewSession is a session just created at sign-in.
type NewSession struct {
	Token   string // for the cookie; never stored
	Session *domain.Session
	Account *domain.Account
	Next    string // safe path to continue to
}

// VerifyEmailLink signs in with an emailed link.
func (s *AccountService) VerifyEmailLink(ctx context.Context, token, userAgent, ip string) (*NewSession, error) {
	email, next, err := s.store.ConsumeLoginLink(ctx, HashToken(token))
	if errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("%w: this sign-in link has expired or was already used; request a new one", domain.ErrValidation)
	}
	if err != nil {
		return nil, err
	}
	acct, err := s.store.SignIn(ctx, domain.ProviderEmail, email, email, "", "")
	if err != nil {
		return nil, err
	}
	return s.startSession(ctx, acct, next, userAgent, ip)
}

// SignInWithGoogle completes the Google callback.
func (s *AccountService) SignInWithGoogle(ctx context.Context, code, verifier, nonce, next, userAgent, ip string) (*NewSession, error) {
	if s.google == nil {
		return nil, domain.ErrNotFound
	}
	id, err := s.google.Exchange(ctx, code, verifier, nonce)
	if err != nil {
		if errors.Is(err, domain.ErrValidation) {
			return nil, err
		}
		slog.Warn("google sign-in failed", "err", err)
		return nil, fmt.Errorf("%w: Google sign-in didn't complete; try again", domain.ErrValidation)
	}
	acct, err := s.store.SignIn(ctx, domain.ProviderGoogle, id.Subject, id.Email, id.Name, id.Picture)
	if err != nil {
		return nil, err
	}
	return s.startSession(ctx, acct, next, userAgent, ip)
}

func (s *AccountService) startSession(ctx context.Context, acct *domain.Account, next, userAgent, ip string) (*NewSession, error) {
	token := NewSecret()
	sess, err := s.store.CreateSession(ctx, acct.ID, HashToken(token), NewSecret(),
		time.Now().Add(s.cfg.SessionTTL), userAgent, ip)
	if err != nil {
		return nil, err
	}
	slog.Info("signed in", "account", acct.ID)
	return &NewSession{Token: token, Session: sess, Account: acct, Next: SafeNext(next)}, nil
}

// Authenticate returns the live session for a cookie token.
func (s *AccountService) Authenticate(ctx context.Context, token string) (*domain.Session, *domain.Account, error) {
	if token == "" {
		return nil, nil, domain.ErrUnauthenticated
	}
	sess, acct, err := s.store.SessionByToken(ctx, HashToken(token))
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil, domain.ErrUnauthenticated
	}
	return sess, acct, err
}

// SignOut ends the session behind token.
func (s *AccountService) SignOut(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.store.RevokeSession(ctx, HashToken(token))
}
