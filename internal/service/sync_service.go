// Package service holds the business rules between HTTP handlers and storage.
package service

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// MaxSyncBatch bounds one request so a device that was offline all day
// uploads in chunks instead of one huge transaction.
const MaxSyncBatch = 500

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func IsUUID(s string) bool { return uuidRe.MatchString(s) }

type ParticipantStore interface {
	ListConfirmedManifest(ctx context.Context) ([]domain.ParticipantManifest, error)
	GetPass(ctx context.Context, membershipID, email string) (*domain.ParticipantPass, error)
}

type ScanStore interface {
	SyncBatch(ctx context.Context, adminID string, scans []domain.SyncScanPayload) ([]domain.SyncResult, error)
	ListClaims(ctx context.Context, since time.Time) ([]domain.Claim, error)
}

type ScenarioLister interface {
	List(ctx context.Context, activeOnly bool) ([]domain.Scenario, error)
}

// GrantStore answers which scenarios a scanner account may scan.
type GrantStore interface {
	GrantedScenarioIDs(ctx context.Context, adminID string) (map[string]struct{}, error)
}

type SyncService struct {
	participants ParticipantStore
	scans        ScanStore
	scenarios    ScenarioLister
	grants       GrantStore
}

func NewSyncService(p ParticipantStore, s ScanStore, sc ScenarioLister, g GrantStore) *SyncService {
	return &SyncService{participants: p, scans: s, scenarios: sc, grants: g}
}

// GetConfirmedManifest exports confirmed delegates for Dexie.js pre-hydration.
func (s *SyncService) GetConfirmedManifest(ctx context.Context) ([]domain.ParticipantManifest, error) {
	return s.participants.ListConfirmedManifest(ctx)
}

// ListClaims returns server-side claims so devices learn about other devices' scans.
func (s *SyncService) ListClaims(ctx context.Context, since time.Time) ([]domain.Claim, error) {
	return s.scans.ListClaims(ctx, since)
}

// ProcessBatchSync validates an uploaded batch and persists the valid rows.
//
// adminID comes from the verified JWT, never from the request body, so one
// device cannot record scans in another admin's name. role is the account's
// current role: scanner accounts may only upload scans for scenarios granted
// to them. Results are returned in the same order as the input so the client
// can match them up.
func (s *SyncService) ProcessBatchSync(ctx context.Context, adminID, role string, scans []domain.SyncScanPayload) ([]domain.SyncResult, error) {
	if len(scans) == 0 {
		return []domain.SyncResult{}, nil
	}
	if len(scans) > MaxSyncBatch {
		return nil, fmt.Errorf("%w: batch exceeds %d scans", domain.ErrValidation, MaxSyncBatch)
	}

	// Scenarios are checked against the full list, not just active ones: a
	// scan taken while a scenario was active must still count if an admin
	// deactivated it before the device came back online.
	all, err := s.scenarios.List(ctx, false)
	if err != nil {
		return nil, err
	}
	known := make(map[string]struct{}, len(all))
	for _, sc := range all {
		known[strings.ToLower(sc.ID)] = struct{}{}
	}

	// nil means "every scenario" (admins and superadmins).
	var granted map[string]struct{}
	if !domain.IsManager(role) {
		if granted, err = s.grants.GrantedScenarioIDs(ctx, adminID); err != nil {
			return nil, err
		}
	}

	results := make([]domain.SyncResult, len(scans))
	valid := make([]domain.SyncScanPayload, 0, len(scans))
	validIdx := make([]int, 0, len(scans))
	seen := make(map[string]struct{}, len(scans))

	for i, scan := range scans {
		if msg := validateScan(adminID, scan, known, granted, seen); msg != "" {
			results[i] = domain.SyncResult{
				IdempotencyKey: scan.IdempotencyKey,
				Status:         domain.SyncInvalid,
				Message:        msg,
			}
			continue
		}
		seen[scan.IdempotencyKey] = struct{}{}
		valid = append(valid, scan)
		validIdx = append(validIdx, i)
	}

	if len(valid) > 0 {
		dbResults, err := s.scans.SyncBatch(ctx, adminID, valid)
		if err != nil {
			return nil, err
		}
		for j, r := range dbResults {
			results[validIdx[j]] = r
		}
	}
	return results, nil
}

func validateScan(adminID string, s domain.SyncScanPayload, known, granted, seen map[string]struct{}) string {
	switch {
	case !IsUUID(s.ParticipantID):
		return "participant_id is not a UUID"
	case !IsUUID(s.ScenarioID):
		return "scenario_id is not a UUID"
	case s.ScannedAtDevice.IsZero():
		return "scanned_at_device is required"
	case len(s.IdempotencyKey) == 0 || len(s.IdempotencyKey) > 200:
		return "idempotency_key must be 1-200 characters"
	}
	if _, ok := known[strings.ToLower(s.ScenarioID)]; !ok {
		return "unknown scenario"
	}
	// Enforced here, not just by hiding the scenario on the phone. A grant
	// removed while a phone was offline makes its queued scans for that
	// checkpoint INVALID; the dashboard shows them in the phone's invalid count.
	if granted != nil {
		if _, ok := granted[strings.ToLower(s.ScenarioID)]; !ok {
			return "this checkpoint is not assigned to your account"
		}
	}
	if _, dup := seen[s.IdempotencyKey]; dup {
		return "idempotency_key repeated within batch"
	}

	// The key must be bound to the authenticated admin and the scan's own
	// participant/scenario: exactly the prefix (single-claim) or the prefix
	// plus "_<suffix>" (repeatable scenarios add a per-scan UUID).
	prefix := adminID + "_" + s.ParticipantID + "_" + s.ScenarioID
	if s.IdempotencyKey != prefix && !strings.HasPrefix(s.IdempotencyKey, prefix+"_") {
		return "idempotency_key does not match admin, participant and scenario"
	}
	return ""
}
