package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type ParticipantService struct{ participants ParticipantStore }

func NewParticipantService(p ParticipantStore) *ParticipantService {
	return &ParticipantService{participants: p}
}

// GetPass normalizes the lookup exactly like the seed script does before
// computing the HMAC (trimmed membership ID, trimmed lower-case email).
func (s *ParticipantService) GetPass(ctx context.Context, membershipID, email string) (*domain.ParticipantPass, error) {
	membershipID = strings.TrimSpace(membershipID)
	email = strings.ToLower(strings.TrimSpace(email))
	if membershipID == "" || email == "" {
		return nil, fmt.Errorf("%w: membership ID and email are both required", domain.ErrValidation)
	}
	return s.participants.GetPass(ctx, membershipID, email)
}
