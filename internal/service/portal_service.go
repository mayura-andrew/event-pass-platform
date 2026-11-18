package service

import (
	"context"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/passes"
)

// PortalStore reads a participant's own passes (postgres.PortalRepo).
type PortalStore interface {
	MyPasses(ctx context.Context, as authz.AccountScope) ([]domain.PassContext, error)
	MyPass(ctx context.Context, as authz.AccountScope, participantID string) (*domain.PassContext, error)
	PendingClaims(ctx context.Context, as authz.AccountScope) ([]domain.PassClaim, error)
	ClaimPass(ctx context.Context, as authz.AccountScope, participantID string) error
	DeclineClaim(ctx context.Context, as authz.AccountScope, participantID string) error
}

// PortalService is "My passes": one account, every registration across orgs.
type PortalService struct{ store PortalStore }

func NewPortalService(store PortalStore) *PortalService { return &PortalService{store: store} }

func (s *PortalService) MyPasses(ctx context.Context, as authz.AccountScope) ([]domain.PassContext, error) {
	return s.store.MyPasses(ctx, as)
}

func (s *PortalService) MyPass(ctx context.Context, as authz.AccountScope, participantID string) (*domain.PassContext, error) {
	return s.store.MyPass(ctx, as, participantID)
}

// PendingClaims are registrations made with the account's email that it has
// not confirmed yet.
func (s *PortalService) PendingClaims(ctx context.Context, as authz.AccountScope) ([]domain.PassClaim, error) {
	return s.store.PendingClaims(ctx, as)
}

// ClaimPass links a registration made with the account's email ("This is
// mine"). DeclineClaim hides it ("Not me").
func (s *PortalService) ClaimPass(ctx context.Context, as authz.AccountScope, participantID string) error {
	return s.store.ClaimPass(ctx, as, participantID)
}

func (s *PortalService) DeclineClaim(ctx context.Context, as authz.AccountScope, participantID string) error {
	return s.store.DeclineClaim(ctx, as, participantID)
}

// ParticipantAnswers are the answers a participant may see on their own
// pass page: questions marked "participant" or "pass".
func ParticipantAnswers(pc *domain.PassContext) []passes.Line {
	var out []passes.Line
	for _, f := range pc.Fields {
		if f.Visibility == "participant" || f.Visibility == "pass" {
			if v := answerText(pc.Participant.Answers[f.Key]); v != "" {
				out = append(out, passes.Line{Label: f.Label, Value: v})
			}
		}
	}
	return out
}
