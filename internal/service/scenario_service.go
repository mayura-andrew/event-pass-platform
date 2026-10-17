package service

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type ScenarioStore interface {
	List(ctx context.Context, activeOnly bool) ([]domain.Scenario, error)
	ListActiveGranted(ctx context.Context, adminID string) ([]domain.Scenario, error)
	Create(ctx context.Context, in domain.ScenarioInput) (*domain.Scenario, error)
	Update(ctx context.Context, id string, in domain.ScenarioInput) (*domain.Scenario, error)
}

var codeRe = regexp.MustCompile(`^[A-Z0-9_]{2,50}$`)

type ScenarioService struct{ store ScenarioStore }

func NewScenarioService(s ScenarioStore) *ScenarioService { return &ScenarioService{store: s} }

func (s *ScenarioService) List(ctx context.Context, activeOnly bool) ([]domain.Scenario, error) {
	return s.store.List(ctx, activeOnly)
}

// ListActiveForAccount is what a scanner downloads: every active scenario for
// admins and superadmins, only granted ones for scanner accounts. role must be
// the account's current role, not the one in a possibly stale JWT.
func (s *ScenarioService) ListActiveForAccount(ctx context.Context, adminID, role string) ([]domain.Scenario, error) {
	if domain.IsManager(role) {
		return s.store.List(ctx, true)
	}
	return s.store.ListActiveGranted(ctx, adminID)
}

func (s *ScenarioService) Create(ctx context.Context, in domain.ScenarioInput) (*domain.Scenario, error) {
	if in.Name == nil || in.Code == nil {
		return nil, fmt.Errorf("%w: name and code are required", domain.ErrValidation)
	}
	if err := normalizeScenario(&in); err != nil {
		return nil, err
	}
	return s.store.Create(ctx, in)
}

func (s *ScenarioService) Update(ctx context.Context, id string, in domain.ScenarioInput) (*domain.Scenario, error) {
	if !IsUUID(id) {
		return nil, domain.ErrNotFound
	}
	if err := normalizeScenario(&in); err != nil {
		return nil, err
	}
	return s.store.Update(ctx, id, in)
}

func normalizeScenario(in *domain.ScenarioInput) error {
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" || len(n) > 100 {
			return fmt.Errorf("%w: name must be 1-100 characters", domain.ErrValidation)
		}
		in.Name = &n
	}
	if in.Code != nil {
		c := strings.ToUpper(strings.TrimSpace(*in.Code))
		if !codeRe.MatchString(c) {
			return fmt.Errorf("%w: code must be 2-50 characters of A-Z, 0-9 or _", domain.ErrValidation)
		}
		in.Code = &c
	}
	return nil
}
