package service

import (
	"context"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/internal/passes"
)

// PortalStore reads a participant's own passes (postgres.PortalRepo).
type PortalStore interface {
	MyPasses(ctx context.Context, as authz.AccountScope) ([]domain.PassContext, error)
	MyPass(ctx context.Context, as authz.AccountScope, participantID string) (*domain.PassContext, error)
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
