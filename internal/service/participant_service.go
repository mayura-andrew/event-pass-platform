package service

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type ParticipantService struct{ participants ParticipantStore }

func NewParticipantService(p ParticipantStore) *ParticipantService {
	return &ParticipantService{participants: p}
}

// normalizeLookup trims and lower-cases exactly like the seed script does
// before computing the HMAC (trimmed membership ID, trimmed lower-case email).
func normalizeLookup(membershipID, email string) (string, string, error) {
	membershipID = strings.TrimSpace(membershipID)
	email = strings.ToLower(strings.TrimSpace(email))
	if membershipID == "" || email == "" {
		return "", "", fmt.Errorf("%w: membership ID and email are both required", domain.ErrValidation)
	}
	return membershipID, email, nil
}

func (s *ParticipantService) GetPass(ctx context.Context, membershipID, email string) (*domain.ParticipantPass, error) {
	membershipID, email, err := normalizeLookup(membershipID, email)
	if err != nil {
		return nil, err
	}
	p, err := s.participants.GetPass(ctx, membershipID, email)
	if err != nil {
		return nil, err
	}
	p.FoodOptions = domain.FoodPreferenceOptions
	return p, nil
}

// UpdateFoodPreference lets a delegate set their own food preference, proven
// by the same membership ID + email pair that opens their pass.
func (s *ParticipantService) UpdateFoodPreference(ctx context.Context, membershipID, email, value string) (string, error) {
	membershipID, email, err := normalizeLookup(membershipID, email)
	if err != nil {
		return "", err
	}
	value = strings.TrimSpace(value)
	if !slices.Contains(domain.FoodPreferenceOptions, value) {
		return "", fmt.Errorf("%w: food preference must be one of %s", domain.ErrValidation,
			strings.Join(domain.FoodPreferenceOptions, ", "))
	}
	return s.participants.UpdateFoodPreference(ctx, membershipID, email, value)
}
