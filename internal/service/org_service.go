package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// OrgStore persists organizations, members and invites (postgres.OrgRepo).
type OrgStore interface {
	CreateOrg(ctx context.Context, as authz.AccountScope, name, slug string) (*domain.OrgMembership, error)
	ListMyOrgs(ctx context.Context, as authz.AccountScope) ([]domain.OrgMembership, error)
	Membership(ctx context.Context, as authz.AccountScope, slug string) (*domain.OrgMembership, error)
	ListMembers(ctx context.Context, sc authz.OrgScope) ([]domain.OrgMember, error)
	GetMember(ctx context.Context, sc authz.OrgScope, accountID string) (*domain.OrgMember, error)
	SetMemberRole(ctx context.Context, sc authz.OrgScope, accountID, role string) error
	RemoveMember(ctx context.Context, sc authz.OrgScope, accountID string) error
	CreateInvite(ctx context.Context, sc authz.OrgScope, email, role string, tokenHash []byte, expires time.Time) (*domain.Invite, error)
	ListInvites(ctx context.Context, sc authz.OrgScope) ([]domain.Invite, error)
	RevokeInvite(ctx context.Context, sc authz.OrgScope, inviteID string) error
	InviteByToken(ctx context.Context, tokenHash []byte) (*domain.InviteDetails, error)
	AcceptInvite(ctx context.Context, as authz.AccountScope, inv *domain.InviteDetails) (*domain.OrgMembership, error)
	UpdateSettings(ctx context.Context, sc authz.OrgScope, st domain.OrgSettings) error
	Onboarding(ctx context.Context, sc authz.OrgScope) (*domain.Onboarding, error)
	ListAudit(ctx context.Context, sc authz.OrgScope, eventID string, limit int) ([]domain.AuditEntry, error)
}

// InviteTTL is how long an emailed invite stays valid.
const InviteTTL = 7 * 24 * time.Hour

var slugRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,38}[a-z0-9])$`)

// reservedSlugs would read as product pages in /o/{slug} URLs.
var reservedSlugs = map[string]bool{
	"new": true, "admin": true, "api": true, "app": true, "settings": true,
	"billing": true, "help": true, "support": true, "login": true, "static": true,
}

type OrgService struct {
	store   OrgStore
	mailer  Mailer
	baseURL string
	product string
}

func NewOrgService(store OrgStore, mailer Mailer, baseURL, product string) *OrgService {
	return &OrgService{store: store, mailer: mailer, baseURL: baseURL, product: product}
}

// CreateOrg makes a new organization owned by the caller.
func (s *OrgService) CreateOrg(ctx context.Context, as authz.AccountScope, name, slug string) (*domain.OrgMembership, error) {
	name = strings.TrimSpace(name)
	slug = strings.ToLower(strings.TrimSpace(slug))
	if n := utf8.RuneCountInString(name); n < 2 || n > 120 || !ValidDisplayName(name) {
		return nil, fmt.Errorf("%w: the organization name must be 2 to 120 characters", domain.ErrValidation)
	}
	if !slugRe.MatchString(slug) {
		return nil, fmt.Errorf("%w: the address must be 3 to 40 lower-case letters, digits or hyphens, not starting or ending with a hyphen", domain.ErrValidation)
	}
	if reservedSlugs[slug] {
		return nil, fmt.Errorf("%w: the address /o/%s is reserved; choose another", domain.ErrConflict, slug)
	}
	m, err := s.store.CreateOrg(ctx, as, name, slug)
	if err != nil {
		return nil, err
	}
	slog.Info("org created", "org", m.ID, "account", as.AccountID())
	return m, nil
}

func (s *OrgService) ListOrgs(ctx context.Context, as authz.AccountScope) ([]domain.OrgMembership, error) {
	return s.store.ListMyOrgs(ctx, as)
}

// Scope resolves an org slug for the caller. It is how every org request
// starts: non-members get ErrNotFound, exactly like an unknown slug.
func (s *OrgService) Scope(ctx context.Context, as authz.AccountScope, slug string) (authz.OrgScope, *domain.OrgMembership, error) {
	m, err := s.store.Membership(ctx, as, strings.ToLower(slug))
	if err != nil {
		return authz.OrgScope{}, nil, err
	}
	return authz.ForMember(as, m.ID, m.Role), m, nil
}

func (s *OrgService) ListMembers(ctx context.Context, sc authz.OrgScope) ([]domain.OrgMember, error) {
	if err := sc.Require(authz.ViewMembers); err != nil {
		return nil, err
	}
	return s.store.ListMembers(ctx, sc)
}

func validInviteRole(role string) bool {
	return role == domain.OrgRoleAdmin || role == domain.OrgRoleMember
}

// Invite emails someone a link to join the org. If the email can't be sent
// the invite is withdrawn, so the list never shows one nobody received.
func (s *OrgService) Invite(ctx context.Context, sc authz.OrgScope, org *domain.OrgMembership, inviter *domain.Account, email, role string) (*domain.Invite, error) {
	if err := sc.Require(authz.ManageMembers); err != nil {
		return nil, err
	}
	email, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	if !validInviteRole(role) {
		return nil, fmt.Errorf("%w: role must be admin or member", domain.ErrValidation)
	}

	token := NewSecret()
	inv, err := s.store.CreateInvite(ctx, sc, email, role, HashToken(token), time.Now().Add(InviteTTL))
	if err != nil {
		return nil, err
	}

	who := inviter.Name
	if who == "" {
		who = inviter.Email
	}
	link := s.baseURL + "/invite?token=" + token
	text, html, err := ActionEmail{
		Product: s.product,
		Heading: fmt.Sprintf("Join %s", org.Name),
		Paragraph: []string{
			fmt.Sprintf("%s invited you to join %s on %s as %s.", who, org.Name, s.product, roleArticle(role)),
			fmt.Sprintf("Sign in with this address (%s) to accept. The invite expires in 7 days.", email),
		},
		Button:   "Accept invite",
		URL:      link,
		Footnote: "Not expecting this? Ignore the email and nothing changes.",
	}.Render()
	if err == nil {
		err = s.mailer.Send(ctx, Message{
			To:      mail.Address{Address: email},
			Subject: fmt.Sprintf("%s invited you to %s", who, org.Name),
			Text:    text, HTML: html,
		})
	}
	if err != nil {
		if rerr := s.store.RevokeInvite(ctx, sc, inv.ID); rerr != nil {
			slog.Error("withdraw unsent invite", "invite", inv.ID, "err", rerr)
		}
		if errors.Is(err, domain.ErrEmailUnavailable) {
			return nil, err
		}
		slog.Error("invite email failed", "org", sc.OrgID(), "err", err)
		return nil, fmt.Errorf("%w: the invite email could not be sent, so no invite was created; try again", domain.ErrEmailUnavailable)
	}
	slog.Info("invite sent", "org", sc.OrgID(), "invite", inv.ID, "by", sc.AccountID())
	return inv, nil
}

func roleArticle(role string) string {
	if role == domain.OrgRoleAdmin {
		return "an admin"
	}
	return "a member"
}

func (s *OrgService) ListInvites(ctx context.Context, sc authz.OrgScope) ([]domain.Invite, error) {
	if err := sc.Require(authz.ViewInvites); err != nil {
		return nil, err
	}
	return s.store.ListInvites(ctx, sc)
}

func (s *OrgService) RevokeInvite(ctx context.Context, sc authz.OrgScope, inviteID string) error {
	if err := sc.Require(authz.ManageMembers); err != nil {
		return err
	}
	return s.store.RevokeInvite(ctx, sc, inviteID)
}

// SetRole changes a member's role. Only owners can make owners or change an
// owner's role; the database keeps at least one owner.
func (s *OrgService) SetRole(ctx context.Context, sc authz.OrgScope, accountID, role string) error {
	if err := sc.Require(authz.ManageMembers); err != nil {
		return err
	}
	if role != domain.OrgRoleOwner && !validInviteRole(role) {
		return fmt.Errorf("%w: role must be owner, admin or member", domain.ErrValidation)
	}
	target, err := s.store.GetMember(ctx, sc, accountID)
	if err != nil {
		return err
	}
	if role == domain.OrgRoleOwner || target.Role == domain.OrgRoleOwner {
		if err := sc.Require(authz.GrantOwner); err != nil {
			return err
		}
	}
	return s.store.SetMemberRole(ctx, sc, accountID, role)
}

// RemoveMember takes someone out of the org. Anyone may leave; removing
// someone else needs ManageMembers, and an owner needs another owner.
func (s *OrgService) RemoveMember(ctx context.Context, sc authz.OrgScope, accountID string) error {
	if accountID != sc.AccountID() {
		if err := sc.Require(authz.ManageMembers); err != nil {
			return err
		}
		target, err := s.store.GetMember(ctx, sc, accountID)
		if err != nil {
			return err
		}
		if target.Role == domain.OrgRoleOwner {
			if err := sc.Require(authz.GrantOwner); err != nil {
				return err
			}
		}
	}
	return s.store.RemoveMember(ctx, sc, accountID)
}

// InviteDetails looks up an invite by its emailed token, for the accept page.
func (s *OrgService) InviteDetails(ctx context.Context, token string) (*domain.InviteDetails, error) {
	if token == "" {
		return nil, domain.ErrNotFound
	}
	return s.store.InviteByToken(ctx, HashToken(token))
}

// AcceptInvite joins the org, or for an event invite gets the event's share.
// The invite only works for the address it was sent to, so a forwarded email
// can't be used by someone else. The invite is returned too, so callers can
// go to the event it was for.
func (s *OrgService) AcceptInvite(ctx context.Context, as authz.AccountScope, acct *domain.Account, token string) (*domain.OrgMembership, *domain.InviteDetails, error) {
	inv, err := s.InviteDetails(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	m, err := s.acceptInvite(ctx, as, acct, inv)
	return m, inv, err
}

func (s *OrgService) acceptInvite(ctx context.Context, as authz.AccountScope, acct *domain.Account, inv *domain.InviteDetails) (*domain.OrgMembership, error) {
	switch {
	case inv.Revoked:
		return nil, fmt.Errorf("%w: this invite was withdrawn; ask for a new one", domain.ErrValidation)
	case inv.Accepted:
		return nil, fmt.Errorf("%w: this invite was already used", domain.ErrValidation)
	case time.Now().After(inv.ExpiresAt):
		return nil, fmt.Errorf("%w: this invite has expired; ask for a new one", domain.ErrValidation)
	case !strings.EqualFold(inv.Email, acct.Email):
		return nil, fmt.Errorf("%w: this invite was sent to %s; sign in with that address to accept it", domain.ErrForbidden, inv.Email)
	}
	m, err := s.store.AcceptInvite(ctx, as, inv)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("%w: this invite is no longer valid; ask for a new one", domain.ErrValidation)
	}
	if err != nil {
		return nil, err
	}
	slog.Info("invite accepted", "org", m.ID, "account", as.AccountID())
	return m, nil
}

// UpdateSettings changes org-wide switches (owners and admins).
func (s *OrgService) UpdateSettings(ctx context.Context, sc authz.OrgScope, org *domain.OrgMembership, externalSharing bool) error {
	if err := sc.Require(authz.UpdateOrg); err != nil {
		return err
	}
	st := org.Settings
	st.ExternalSharing = &externalSharing
	return s.store.UpdateSettings(ctx, sc, st)
}

// Onboarding is the setup checklist's progress, for the owners and admins
// who set the org up.
func (s *OrgService) Onboarding(ctx context.Context, sc authz.OrgScope) (*domain.Onboarding, error) {
	if err := sc.Require(authz.UpdateOrg); err != nil {
		return nil, err
	}
	return s.store.Onboarding(ctx, sc)
}

// SetChecklistHidden hides or shows the setup checklist for the org.
func (s *OrgService) SetChecklistHidden(ctx context.Context, sc authz.OrgScope, org *domain.OrgMembership, hidden bool) error {
	if err := sc.Require(authz.UpdateOrg); err != nil {
		return err
	}
	st := org.Settings
	st.ChecklistHidden = hidden
	return s.store.UpdateSettings(ctx, sc, st)
}

// Activity returns the org's audit log (owners and admins).
func (s *OrgService) Activity(ctx context.Context, sc authz.OrgScope) ([]domain.AuditEntry, error) {
	if err := sc.Require(authz.ViewAudit); err != nil {
		return nil, err
	}
	return s.store.ListAudit(ctx, sc, "", 200)
}
