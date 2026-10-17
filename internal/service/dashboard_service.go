package service

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type DashboardStore interface {
	GetFilteredParticipants(ctx context.Context, f domain.ParticipantFilter) (*domain.ParticipantPage, error)
	GetDashboardStats(ctx context.Context, checkInCode string) (*domain.DashboardStats, error)
	ManualScanOverride(ctx context.Context, o domain.ManualOverride) (*domain.ManualOverrideResult, error)
	RecentScans(ctx context.Context, limit int) ([]domain.ScanEvent, error)
	UpsertHeartbeat(ctx context.Context, adminID string, hb domain.DeviceHeartbeat) error
	ListDevices(ctx context.Context, limit int) ([]domain.DeviceHealthItem, error)
	ListVenues(ctx context.Context) ([]domain.Venue, error)
}

// Feed and device list bounds.
const (
	MaxRecentScans = 200
	maxDevices     = 500
	maxSearchLen   = 100
	// A phone's queue can't plausibly exceed this; larger numbers are a bug
	// or tampering and would distort the dashboard totals.
	maxReportedCount = 1_000_000
)

// ScenarioUpdater is the slice of ScenarioService the dashboard toggle uses.
type ScenarioUpdater interface {
	Update(ctx context.Context, id string, in domain.ScenarioInput) (*domain.Scenario, error)
}

type DashboardService struct {
	store       DashboardStore
	scenarios   ScenarioUpdater
	checkInCode string // scenario code counted as "checked in"
}

func NewDashboardService(store DashboardStore, scenarios ScenarioUpdater, checkInCode string) *DashboardService {
	return &DashboardService{store: store, scenarios: scenarios, checkInCode: checkInCode}
}

// FilterParticipants validates the filter and returns one page of delegates.
func (s *DashboardService) FilterParticipants(ctx context.Context, f domain.ParticipantFilter) (*domain.ParticipantPage, error) {
	f.Search = strings.TrimSpace(f.Search)
	if utf8.RuneCountInString(f.Search) > maxSearchLen {
		return nil, fmt.Errorf("%w: search must be at most %d characters", domain.ErrValidation, maxSearchLen)
	}

	f.AgeGroup = strings.ToUpper(strings.TrimSpace(f.AgeGroup))
	switch f.AgeGroup {
	case domain.AgeAll, domain.AgeUnder18, domain.AgeAbove18, domain.AgeUnknown:
	default:
		return nil, fmt.Errorf("%w: age_group must be UNDER_18, ABOVE_18, UNKNOWN or empty", domain.ErrValidation)
	}

	f.ClaimStatus = strings.ToUpper(strings.TrimSpace(f.ClaimStatus))
	switch f.ClaimStatus {
	case domain.ClaimAll, domain.ClaimClaimed, domain.ClaimUnclaimed:
	default:
		return nil, fmt.Errorf("%w: claim_status must be CLAIMED, UNCLAIMED or empty", domain.ErrValidation)
	}

	f.ScenarioID = strings.TrimSpace(f.ScenarioID)
	if f.ScenarioID != "" && !IsUUID(f.ScenarioID) {
		return nil, fmt.Errorf("%w: scenario_id is not a UUID", domain.ErrValidation)
	}
	if f.ClaimStatus != domain.ClaimAll && f.ScenarioID == "" {
		return nil, fmt.Errorf("%w: choose a scenario to filter by claim status", domain.ErrValidation)
	}

	f.VenueID = strings.TrimSpace(f.VenueID)
	if f.VenueID != "" && f.VenueID != domain.VenueUnassigned && !IsUUID(f.VenueID) {
		return nil, fmt.Errorf("%w: venue_id must be a UUID or UNASSIGNED", domain.ErrValidation)
	}

	f.Confirmation = strings.ToUpper(strings.TrimSpace(f.Confirmation))
	switch f.Confirmation {
	case domain.ConfirmAll, domain.ConfirmConfirmed, domain.ConfirmUnconfirmed:
	default:
		return nil, fmt.Errorf("%w: confirmation must be CONFIRMED, UNCONFIRMED or empty", domain.ErrValidation)
	}

	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 {
		f.PageSize = domain.DefaultPageSize
	}
	if f.PageSize > domain.MaxPageSize {
		f.PageSize = domain.MaxPageSize
	}
	// Bound OFFSET so a crafted page number can't force a huge scan.
	if f.Page > 100_000 {
		return nil, fmt.Errorf("%w: page is out of range", domain.ErrValidation)
	}

	return s.store.GetFilteredParticipants(ctx, f)
}

func (s *DashboardService) Stats(ctx context.Context) (*domain.DashboardStats, error) {
	return s.store.GetDashboardStats(ctx, s.checkInCode)
}

// ToggleScenario switches a checkpoint on or off. Phones pick the change up
// on their next refresh; scans already taken still upload (see SyncService).
func (s *DashboardService) ToggleScenario(ctx context.Context, req domain.ScenarioToggleRequest) (*domain.Scenario, error) {
	if !IsUUID(req.ScenarioID) {
		return nil, fmt.Errorf("%w: scenario_id is not a UUID", domain.ErrValidation)
	}
	active := req.IsActive
	return s.scenarios.Update(ctx, req.ScenarioID, domain.ScenarioInput{IsActive: &active})
}

// ManualOverride records a scan without a QR code. adminID must come from the
// verified JWT.
func (s *DashboardService) ManualOverride(ctx context.Context, adminID string, req domain.ManualOverrideRequest) (*domain.ManualOverrideResult, error) {
	if !IsUUID(req.ParticipantID) {
		return nil, fmt.Errorf("%w: participant_id is not a UUID", domain.ErrValidation)
	}
	if !IsUUID(req.ScenarioID) {
		return nil, fmt.Errorf("%w: scenario_id is not a UUID", domain.ErrValidation)
	}
	// Collapse runs of whitespace so "          ." can't pass the length check.
	rationale := strings.Join(strings.Fields(req.Rationale), " ")
	n := utf8.RuneCountInString(rationale)
	if n < domain.MinRationaleLen || n > domain.MaxRationaleLen {
		return nil, fmt.Errorf("%w: rationale must be %d-%d characters explaining why the QR was bypassed",
			domain.ErrValidation, domain.MinRationaleLen, domain.MaxRationaleLen)
	}
	return s.store.ManualScanOverride(ctx, domain.ManualOverride{
		AdminID:       adminID,
		ParticipantID: req.ParticipantID,
		ScenarioID:    req.ScenarioID,
		Rationale:     rationale,
	})
}

// RecentScans returns the newest scans for the feed backlog.
func (s *DashboardService) RecentScans(ctx context.Context, limit int) ([]domain.ScanEvent, error) {
	if limit < 1 || limit > MaxRecentScans {
		limit = 50
	}
	return s.store.RecentScans(ctx, limit)
}

// RecordHeartbeat stores a scanner's queue report. adminID comes from the JWT.
func (s *DashboardService) RecordHeartbeat(ctx context.Context, adminID string, hb domain.DeviceHeartbeat) error {
	if !IsUUID(hb.DeviceID) {
		return fmt.Errorf("%w: device_id is not a UUID", domain.ErrValidation)
	}
	if hb.ScenarioID != "" && !IsUUID(hb.ScenarioID) {
		hb.ScenarioID = "" // stale local value; don't reject the whole report
	}
	for _, c := range []int{hb.PendingCount, hb.PendingOthers, hb.RejectedCount, hb.InvalidCount, hb.ManifestCount} {
		if c < 0 || c > maxReportedCount {
			return fmt.Errorf("%w: counts must be between 0 and %d", domain.ErrValidation, maxReportedCount)
		}
	}
	if len(hb.UserAgent) > 300 {
		hb.UserAgent = hb.UserAgent[:300]
	}
	return s.store.UpsertHeartbeat(ctx, adminID, hb)
}

func (s *DashboardService) Devices(ctx context.Context) ([]domain.DeviceHealthItem, error) {
	return s.store.ListDevices(ctx, maxDevices)
}

func (s *DashboardService) Venues(ctx context.Context) ([]domain.Venue, error) {
	return s.store.ListVenues(ctx)
}
