package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

// AccountRepo stores accounts, sign-in links and sessions. These are not
// tenant data: they are read before anyone is known to belong to an org, so
// they run as the API login itself, which migration 017 grants these four
// tables and nothing else.
type AccountRepo struct{ db *sql.DB }

func NewAccountRepo(db *sql.DB) *AccountRepo { return &AccountRepo{db: db} }

// ---- Emailed sign-in links ---------------------------------------------------

func (r *AccountRepo) CreateLoginLink(ctx context.Context, tokenHash []byte, email, next string, expires time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO login_links (token_hash, email, next_path, expires_at) VALUES ($1, $2, $3, $4)`,
		tokenHash, email, next, expires)
	return err
}

// RecentLoginLinks counts links sent to email since the given time, for the
// per-address limit.
func (r *AccountRepo) RecentLoginLinks(ctx context.Context, email string, since time.Time) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM login_links WHERE email = $1 AND created_at > $2`, email, since).Scan(&n)
	return n, err
}

// ConsumeLoginLink marks an unused, unexpired link used and returns its email
// and destination. A second use, or an unknown or expired token, is ErrNotFound.
func (r *AccountRepo) ConsumeLoginLink(ctx context.Context, tokenHash []byte) (email, next string, err error) {
	err = r.db.QueryRowContext(ctx, `
		UPDATE login_links SET used_at = NOW()
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > NOW()
		RETURNING email, next_path`, tokenHash).Scan(&email, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", domain.ErrNotFound
	}
	return email, next, err
}

// ---- Accounts ----------------------------------------------------------------

// SignIn finds or creates the account for a verified identity and records the
// login. An identity seen for the first time is linked to the account with
// the same email, so signing in with Google and with an emailed link reaches
// one account. email must already be verified by the provider.
func (r *AccountRepo) SignIn(ctx context.Context, provider, subject, email, name, avatarURL string) (*domain.Account, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var accountID string
	err = tx.QueryRowContext(ctx,
		`SELECT account_id FROM auth_identities WHERE provider = $1 AND provider_subject = $2`,
		provider, subject).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		// New identity: the account by email, or a new one. ON CONFLICT makes
		// two first sign-ins at once land on the same account.
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO accounts (id, email, name, avatar_url) VALUES ($1, $2, $3, NULLIF($4, ''))
			ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
			RETURNING id`, newID(), email, name, avatarURL).Scan(&accountID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_identities (provider, provider_subject, account_id) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, provider, subject, accountID); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	var a domain.Account
	var avatar sql.NullString
	if err := tx.QueryRowContext(ctx, `
		UPDATE accounts SET last_login_at = NOW(),
		       name = CASE WHEN name = '' THEN $2 ELSE name END,
		       avatar_url = COALESCE(NULLIF($3, ''), avatar_url)
		WHERE id = $1
		RETURNING id, email, name, avatar_url, created_at, last_login_at`,
		accountID, name, avatarURL,
	).Scan(&a.ID, &a.Email, &a.Name, &avatar, &a.CreatedAt, &a.LastLoginAt); err != nil {
		return nil, err
	}
	a.AvatarURL = avatar.String
	// Registrations made with this email are not linked here: anyone can
	// type an address into a public form, so the account holder confirms
	// each one on "My passes" first (PortalRepo.ClaimPass).
	return &a, tx.Commit()
}

func (r *AccountRepo) GetAccount(ctx context.Context, id string) (*domain.Account, error) {
	var a domain.Account
	var avatar sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT id, email, name, avatar_url, created_at, last_login_at FROM accounts WHERE id = $1`, id,
	).Scan(&a.ID, &a.Email, &a.Name, &avatar, &a.CreatedAt, &a.LastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	a.AvatarURL = avatar.String
	return &a, err
}

// ---- Sessions ----------------------------------------------------------------

func (r *AccountRepo) CreateSession(ctx context.Context, accountID string, tokenHash []byte, csrf string, expires time.Time, userAgent, ip string) (*domain.Session, error) {
	now := time.Now()
	if len(userAgent) > 300 {
		userAgent = userAgent[:300]
	}
	s := &domain.Session{ID: newID(), AccountID: accountID, CSRFToken: csrf, ExpiresAt: expires,
		CreatedAt: now, LastSeenAt: now, UserAgent: userAgent, IP: ip}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO sessions (id, token_hash, account_id, csrf_token, expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		s.ID, tokenHash, accountID, csrf, expires, userAgent, ip)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// SessionByToken returns a live session and its account, or ErrNotFound.
func (r *AccountRepo) SessionByToken(ctx context.Context, tokenHash []byte) (*domain.Session, *domain.Account, error) {
	var s domain.Session
	var a domain.Account
	var avatar sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT s.id, s.account_id, s.csrf_token, s.expires_at, s.created_at, s.last_seen_at, s.user_agent, s.ip,
		       a.id, a.email, a.name, a.avatar_url, a.created_at, a.last_login_at
		FROM sessions s JOIN accounts a ON a.id = s.account_id
		WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > NOW()`, tokenHash,
	).Scan(&s.ID, &s.AccountID, &s.CSRFToken, &s.ExpiresAt, &s.CreatedAt, &s.LastSeenAt, &s.UserAgent, &s.IP,
		&a.ID, &a.Email, &a.Name, &avatar, &a.CreatedAt, &a.LastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	a.AvatarURL = avatar.String
	return &s, &a, nil
}

func (r *AccountRepo) RevokeSession(ctx context.Context, tokenHash []byte) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = NOW() WHERE token_hash = $1 AND revoked_at IS NULL`, tokenHash)
	return err
}

// TouchSession records activity on a session (for the idle timeout). The
// service calls it at most every few minutes per session.
func (r *AccountRepo) TouchSession(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = NOW() WHERE id = $1 AND revoked_at IS NULL`, id)
	return err
}

// ListSessions returns the account's live sessions, most recently used first.
func (r *AccountRepo) ListSessions(ctx context.Context, accountID string) ([]domain.Session, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, account_id, expires_at, created_at, last_seen_at, user_agent, ip FROM sessions
		WHERE account_id = $1 AND revoked_at IS NULL AND expires_at > NOW()
		ORDER BY last_seen_at DESC LIMIT 100`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Session{}
	for rows.Next() {
		var s domain.Session
		if err := rows.Scan(&s.ID, &s.AccountID, &s.ExpiresAt, &s.CreatedAt, &s.LastSeenAt, &s.UserAgent, &s.IP); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeSessionByID signs out one of the account's own sessions; another
// account's session ID is ErrNotFound.
func (r *AccountRepo) RevokeSessionByID(ctx context.Context, accountID, id string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = NOW() WHERE id = $1 AND account_id = $2 AND revoked_at IS NULL`, id, accountID)
	if pgCode(err) == pgInvalidTextRepr {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// RevokeAllSessions signs the account out everywhere except keepID ("" for
// everywhere) and returns how many sessions ended.
func (r *AccountRepo) RevokeAllSessions(ctx context.Context, accountID, keepID string) (int, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = NOW()
		WHERE account_id = $1 AND revoked_at IS NULL AND id::text <> $2`, accountID, keepID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// FindByEmail returns the account with this (lower-cased) email, or
// ErrNotFound. Sharing uses it to tell an existing person from a new one.
func (r *AccountRepo) FindByEmail(ctx context.Context, email string) (*domain.Account, error) {
	var a domain.Account
	var avatar sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT id, email, name, avatar_url, created_at, last_login_at FROM accounts WHERE email = $1`, email,
	).Scan(&a.ID, &a.Email, &a.Name, &avatar, &a.CreatedAt, &a.LastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	a.AvatarURL = avatar.String
	return &a, err
}
