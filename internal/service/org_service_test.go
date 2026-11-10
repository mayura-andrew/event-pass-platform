package service

import (
	"context"
	"errors"
	"testing"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

// createOnlyStore records CreateOrg calls; any other method panics.
type createOnlyStore struct {
	OrgStore
	created int
}

func (s *createOnlyStore) CreateOrg(_ context.Context, _ authz.AccountScope, name, slug string) (*domain.OrgMembership, error) {
	s.created++
	return &domain.OrgMembership{Org: domain.Org{ID: "o1", Name: name, Slug: slug}, Role: domain.OrgRoleOwner}, nil
}

func TestCreateOrgPlatformAdminsOnly(t *testing.T) {
	as := authz.NewAccountScope("a1")
	cases := []struct {
		name    string
		policy  OrgCreatorPolicy
		email   string
		allowed bool
	}{
		{"no policy refuses everyone", nil, "lead@example.org", false},
		{"empty list refuses everyone", OrgCreatorsByEmail(nil), "lead@example.org", false},
		{"listed admin", OrgCreatorsByEmail([]string{" Lead@Example.org "}), "lead@example.org", true},
		{"listed admin, other case", OrgCreatorsByEmail([]string{"lead@example.org"}), "LEAD@example.org", true},
		{"pass holder", OrgCreatorsByEmail([]string{"lead@example.org"}), "guest@example.org", false},
		{"no email", AnyoneCreatesOrgs, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &createOnlyStore{}
			s := NewOrgService(store, nil, "", "")
			s.OrgCreators = c.policy
			if got := s.CanCreateOrg(c.email); got != c.allowed {
				t.Fatalf("CanCreateOrg = %v, want %v", got, c.allowed)
			}
			_, err := s.CreateOrg(context.Background(), as, c.email, "Community Events", "community-events")
			if c.allowed && err != nil {
				t.Fatalf("CreateOrg: %v", err)
			}
			if !c.allowed && !errors.Is(err, domain.ErrForbidden) {
				t.Fatalf("CreateOrg err = %v, want ErrForbidden", err)
			}
			if want := map[bool]int{true: 1, false: 0}[c.allowed]; store.created != want {
				t.Fatalf("store.CreateOrg called %d times, want %d", store.created, want)
			}
		})
	}
}
