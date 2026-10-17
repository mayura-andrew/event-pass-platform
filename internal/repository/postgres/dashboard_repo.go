package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type DashboardRepo struct{ db *sql.DB }

func NewDashboardRepo(db *sql.DB) *DashboardRepo { return &DashboardRepo{db: db} }

// Age cut-off shared by every query. A delegate is a minor when born after
// this date. Written inline (rather than calling participant_age_group()) so
// the planner can use idx_participants_dob directly.
const minorCutoff = `(CURRENT_DATE - INTERVAL '18 years')`

// ---- Dynamic filter ------------------------------------------------------------

// queryBuilder collects WHERE clauses and their positional arguments so every
// user-supplied value is bound as a parameter, never spliced into the SQL.
type queryBuilder struct {
	where []string
	args  []any
}

// arg appends a bind value and returns its placeholder ($1, $2, ...).
func (b *queryBuilder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

func (b *queryBuilder) and(clause string) { b.where = append(b.where, clause) }

func (b *queryBuilder) whereSQL() string {
	if len(b.where) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(b.where, "\n  AND ")
}

// escapeLike makes user input match literally inside a LIKE pattern.
// PostgreSQL's default LIKE escape character is the backslash.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// GetFilteredParticipants returns one page of delegates matching the filter.
// The filter must already be validated by the service layer (enum values,
// UUIDs, page bounds); this method only translates it into SQL.
//
// Two steps in one statement:
//  1. page: filter, count and sort participants only, and keep the ids of
//     one page. The claim filter is an EXISTS on the partial index
//     idx_scans_scenario_valid, not a per-row aggregate.
//  2. details: venue and claim details (count, first scan, who, how) for
//     those page rows only.
//
// Computing the claim aggregate before LIMIT, as COUNT(*) OVER () and the sort
// otherwise force, ran it for every delegate: ~210-360 ms for 3,000 delegates
// on each table load, against ~10 ms this way.
func (r *DashboardRepo) GetFilteredParticipants(ctx context.Context, f domain.ParticipantFilter) (*domain.ParticipantPage, error) {
	var b queryBuilder

	// ---- Step 1 filters (participants only) ----
	if f.ClaimStatus != domain.ClaimAll {
		claimed := `EXISTS (SELECT 1 FROM scans s
			WHERE s.scenario_id = ` + b.arg(f.ScenarioID) + `::uuid
			  AND s.participant_id = p.id AND s.status = 'VALID')`
		if f.ClaimStatus == domain.ClaimUnclaimed {
			claimed = "NOT " + claimed
		}
		b.and(claimed)
	}

	if q := strings.TrimSpace(f.Search); q != "" {
		ph := b.arg("%" + escapeLike(q) + "%")
		b.and(`(p.full_name ILIKE ` + ph + ` OR p.email ILIKE ` + ph +
			` OR p.membership_id ILIKE ` + ph + ` OR p.team_name ILIKE ` + ph + `)`)
	}

	switch f.AgeGroup {
	case domain.AgeUnder18:
		b.and(`p.date_of_birth > ` + minorCutoff)
	case domain.AgeAbove18:
		b.and(`p.date_of_birth <= ` + minorCutoff)
	case domain.AgeUnknown:
		b.and(`p.date_of_birth IS NULL`)
	}

	switch f.VenueID {
	case "":
	case domain.VenueUnassigned:
		b.and(`p.assigned_venue_id IS NULL`)
	default:
		b.and(`p.assigned_venue_id = ` + b.arg(f.VenueID) + `::uuid`)
	}

	switch f.Confirmation {
	case domain.ConfirmConfirmed:
		b.and(`p.is_confirmed`)
	case domain.ConfirmUnconfirmed:
		b.and(`NOT p.is_confirmed`)
	}

	where := b.whereSQL()
	whereArgs := len(b.args) // the fallback count below binds only these
	limit := b.arg(f.PageSize)
	offset := b.arg((f.Page - 1) * f.PageSize)

	// ---- Step 2 claim details, page rows only ----
	// With no matching scans the aggregate still returns one row (count 0,
	// NULLs), so the LEFT JOIN never drops a delegate. The earliest VALID scan
	// is the claim; repeatable scenarios can have more than one.
	claimJoin := `LEFT JOIN LATERAL (
		SELECT 0::int AS claim_count, NULL::timestamptz AS first_at,
		       NULL::text AS first_by, NULL::text AS first_method
	) c ON true`
	if f.ScenarioID != "" {
		claimJoin = `LEFT JOIN LATERAL (
		SELECT count(*)::int                                      AS claim_count,
		       min(s.synced_at)                                   AS first_at,
		       (array_agg(a.full_name    ORDER BY s.synced_at))[1] AS first_by,
		       (array_agg(s.entry_method ORDER BY s.synced_at))[1] AS first_method
		FROM scans s
		JOIN admins a ON a.id = s.scanned_by_admin_id
		WHERE s.participant_id = p.id
		  AND s.scenario_id = ` + b.arg(f.ScenarioID) + `::uuid
		  AND s.status = 'VALID'
	) c ON true`
	}

	query := `
WITH page AS (
	SELECT p.id, p.full_name, p.membership_id, COUNT(*) OVER () AS total
	FROM participants p
	` + where + `
	ORDER BY p.full_name, p.membership_id
	LIMIT ` + limit + ` OFFSET ` + offset + `
)
SELECT p.id, p.membership_id, p.full_name, p.email, COALESCE(p.team_name, ''),
       p.is_confirmed,
       to_char(p.date_of_birth, 'YYYY-MM-DD'),
       CASE WHEN p.date_of_birth IS NULL THEN NULL
            ELSE date_part('year', age(CURRENT_DATE, p.date_of_birth))::int END,
       COALESCE(p.date_of_birth > ` + minorCutoff + `, false),
       p.assigned_venue_id, COALESCE(v.name, ''), COALESCE(v.hall_section, ''),
       c.claim_count, c.first_at, COALESCE(c.first_by, ''), COALESCE(c.first_method, ''),
       pg.total
FROM page pg
JOIN participants p ON p.id = pg.id
LEFT JOIN venues v ON v.id = p.assigned_venue_id
` + claimJoin + `
ORDER BY pg.full_name, pg.membership_id`

	rows, err := r.db.QueryContext(ctx, query, b.args...)
	if err != nil {
		return nil, fmt.Errorf("filter participants: %w", err)
	}
	defer rows.Close()

	page := &domain.ParticipantPage{
		Rows:     make([]domain.ParticipantRow, 0, f.PageSize),
		Page:     f.Page,
		PageSize: f.PageSize,
	}
	for rows.Next() {
		var (
			p       domain.ParticipantRow
			dob     sql.NullString
			age     sql.NullInt32
			venueID sql.NullString
			firstAt sql.NullTime
		)
		if err := rows.Scan(&p.ID, &p.MembershipID, &p.FullName, &p.Email, &p.TeamName,
			&p.IsConfirmed, &dob, &age, &p.IsMinor,
			&venueID, &p.VenueName, &p.HallSection,
			&p.ClaimCount, &firstAt, &p.ClaimedBy, &p.ClaimMethod,
			&page.Total); err != nil {
			return nil, err
		}
		if dob.Valid {
			p.DateOfBirth = &dob.String
		}
		if age.Valid {
			a := int(age.Int32)
			p.AgeYears = &a
		}
		if venueID.Valid {
			p.VenueID = &venueID.String
		}
		if firstAt.Valid {
			t := firstAt.Time
			p.ClaimedAt = &t
		}
		p.Claimed = p.ClaimCount > 0
		page.Rows = append(page.Rows, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// A page past the end returns no rows, and so no window total. Count
	// separately so the client can step back to the last real page.
	if len(page.Rows) == 0 && f.Page > 1 {
		if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM participants p `+where,
			b.args[:whereArgs]...).Scan(&page.Total); err != nil {
			return nil, fmt.Errorf("count participants: %w", err)
		}
	}
	return page, nil
}

// ---- Stats ---------------------------------------------------------------------

// GetDashboardStats aggregates every metric card in one read-only snapshot, so
// the numbers on screen agree with each other (checked-in can't exceed
// confirmed because a scan landed between two queries).
func (r *DashboardRepo) GetDashboardStats(ctx context.Context, checkInCode string) (*domain.DashboardStats, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin stats tx: %w", err)
	}
	defer tx.Rollback()

	st := &domain.DashboardStats{CheckInScenarioCode: checkInCode}

	// 1. Delegates and age groups (age counts cover confirmed delegates only).
	err = tx.QueryRowContext(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE is_confirmed),
		       count(*) FILTER (WHERE is_confirmed AND date_of_birth >  `+minorCutoff+`),
		       count(*) FILTER (WHERE is_confirmed AND date_of_birth <= `+minorCutoff+`),
		       count(*) FILTER (WHERE is_confirmed AND date_of_birth IS NULL)
		FROM participants`,
	).Scan(&st.TotalParticipants, &st.TotalConfirmed, &st.MinorCount, &st.AdultCount, &st.UnknownAgeCount)
	if err != nil {
		return nil, fmt.Errorf("participant counts: %w", err)
	}

	// 2. Checked in: distinct confirmed delegates with a VALID check-in scan.
	err = tx.QueryRowContext(ctx, `
		SELECT sc.id,
		       count(DISTINCT p.id),
		       count(DISTINCT p.id) FILTER (WHERE p.date_of_birth > `+minorCutoff+`)
		FROM scan_scenarios sc
		LEFT JOIN scans x        ON x.scenario_id = sc.id AND x.status = 'VALID'
		LEFT JOIN participants p ON p.id = x.participant_id AND p.is_confirmed
		WHERE sc.code = $1
		GROUP BY sc.id`, checkInCode,
	).Scan(&st.CheckInScenarioID, &st.CheckedIn, &st.MinorsCheckedIn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check-in counts: %w", err)
	}

	// 3. Per-scenario claim summary.
	rows, err := tx.QueryContext(ctx, `
		SELECT sc.id, sc.name, sc.code, sc.is_active, sc.allow_duplicates,
		       count(x.id)                      FILTER (WHERE x.status = 'VALID'),
		       count(DISTINCT x.participant_id) FILTER (WHERE x.status = 'VALID'),
		       count(x.id)                      FILTER (WHERE x.status = 'DUPLICATE_REJECTED'),
		       count(x.id)                      FILTER (WHERE x.entry_method = 'MANUAL_OVERRIDE')
		FROM scan_scenarios sc
		LEFT JOIN scans x ON x.scenario_id = sc.id
		GROUP BY sc.id
		ORDER BY sc.created_at`)
	if err != nil {
		return nil, fmt.Errorf("scenario stats: %w", err)
	}
	st.Scenarios = make([]domain.ScenarioStat, 0, 8)
	for rows.Next() {
		var s domain.ScenarioStat
		if err := rows.Scan(&s.ID, &s.Name, &s.Code, &s.IsActive, &s.AllowDuplicates,
			&s.ValidScans, &s.UniqueDelegates, &s.RejectedAttempts, &s.ManualOverrides); err != nil {
			rows.Close()
			return nil, err
		}
		st.TotalScenarios++
		if s.IsActive {
			st.ActiveScenarios++
		}
		st.Scenarios = append(st.Scenarios, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 4. Throughput: 15 one-minute slots ending with the current minute.
	//    generate_series fills empty minutes so the chart has no gaps.
	rows, err = tx.QueryContext(ctx, `
		SELECT gs.minute, count(s.id)
		FROM generate_series(date_trunc('minute', now()) - INTERVAL '14 minutes',
		                     date_trunc('minute', now()),
		                     INTERVAL '1 minute') AS gs(minute)
		LEFT JOIN scans s
		       ON s.synced_at >= gs.minute AND s.synced_at < gs.minute + INTERVAL '1 minute'
		GROUP BY gs.minute
		ORDER BY gs.minute`)
	if err != nil {
		return nil, fmt.Errorf("throughput: %w", err)
	}
	st.PerMinute = make([]domain.ThroughputSlot, 0, 15)
	for rows.Next() {
		var slot domain.ThroughputSlot
		if err := rows.Scan(&slot.Minute, &slot.Count); err != nil {
			rows.Close()
			return nil, err
		}
		st.ScansLast15Min += slot.Count
		st.PerMinute = append(st.PerMinute, slot)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Rolling 60 seconds, not the current calendar minute, so the rate doesn't
	// drop to zero at the top of every minute.
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM scans WHERE synced_at > now() - INTERVAL '1 minute'`,
	).Scan(&st.ScansLastMinute); err != nil {
		return nil, fmt.Errorf("scans last minute: %w", err)
	}

	// 5. Scanner fleet, from the latest heartbeat of each tracked device.
	err = tx.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE last_seen_at >  now() - make_interval(secs => $1)),
		       count(*) FILTER (WHERE last_seen_at <= now() - make_interval(secs => $1)),
		       COALESCE(sum(pending_count + pending_others), 0),
		       count(*) FILTER (WHERE last_seen_at <= now() - make_interval(secs => $1)
		                          AND pending_count + pending_others > 0),
		       COALESCE(sum(rejected_count), 0)
		FROM scanner_devices
		WHERE last_seen_at > now() - make_interval(secs => $2)`,
		domain.DeviceOnlineWindow.Seconds(), domain.DeviceTrackWindow.Seconds(),
	).Scan(&st.Sync.DevicesOnline, &st.Sync.DevicesStale, &st.Sync.PendingOnDevices,
		&st.Sync.StaleWithPending, &st.Sync.RejectedOnDevices)
	if err != nil {
		return nil, fmt.Errorf("sync health: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	st.GeneratedAt = time.Now().UTC()
	return st, nil
}

// ---- Manual override -----------------------------------------------------------

// ManualScanOverride records a scan made from the dashboard without a QR code,
// with the admin's written rationale, in one transaction.
//
// It goes through the same "first scan wins" rule as phone syncs: for a
// single-claim scenario the INSERT is absorbed by idx_unique_valid_claim if
// any device already claimed it, and the caller gets a ClaimConflictError
// naming the winner. A phone that later uploads a scan for the same claim
// receives DUPLICATE_REJECTED, exactly as if another phone had won.
func (r *DashboardRepo) ManualScanOverride(ctx context.Context, o domain.ManualOverride) (*domain.ManualOverrideResult, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin override tx: %w", err)
	}
	defer tx.Rollback()

	res := &domain.ManualOverrideResult{}

	// 1. The delegate must exist and be confirmed: an override replaces the
	//    QR, not the registration check.
	var confirmed bool
	err = tx.QueryRowContext(ctx,
		`SELECT full_name, is_confirmed FROM participants WHERE id = $1`, o.ParticipantID,
	).Scan(&res.ParticipantName, &confirmed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: participant does not exist", domain.ErrValidation)
	}
	if err != nil {
		return nil, err
	}
	if !confirmed {
		return nil, fmt.Errorf("%w: %s is not a confirmed delegate", domain.ErrValidation, res.ParticipantName)
	}

	// 2. The checkpoint must exist and be open. FOR SHARE stops an admin from
	//    flipping allow_duplicates between this read and the insert below.
	var active, allowDup bool
	err = tx.QueryRowContext(ctx,
		`SELECT name, is_active, allow_duplicates FROM scan_scenarios WHERE id = $1 FOR SHARE`, o.ScenarioID,
	).Scan(&res.ScenarioName, &active, &allowDup)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: scenario does not exist", domain.ErrValidation)
	}
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, fmt.Errorf("%w: %s is switched off; activate it first", domain.ErrValidation, res.ScenarioName)
	}

	// 3. Insert. The "override_" key prefix can never collide with a phone key
	//    ({admin}_{participant}_{scenario}), so a retry from a phone is still
	//    recognised as its own scan. Device time and server time are both now.
	err = tx.QueryRowContext(ctx, `
		INSERT INTO scans (idempotency_key, participant_id, scenario_id, scanned_by_admin_id,
		                   scanned_at_device, status, enforce_unique, entry_method, override_reason)
		VALUES ('override_' || uuid_generate_v4(), $1, $2, $3,
		        NOW(), 'VALID', $4, 'MANUAL_OVERRIDE', $5)
		ON CONFLICT DO NOTHING
		RETURNING id, synced_at`,
		o.ParticipantID, o.ScenarioID, o.AdminID, !allowDup, o.Rationale,
	).Scan(&res.ScanID, &res.RecordedAt)

	if errors.Is(err, sql.ErrNoRows) {
		// Absorbed by the unique claim index: report who holds the claim.
		var c domain.ClaimConflictError
		if err := tx.QueryRowContext(ctx, `
			SELECT a.full_name, s.synced_at, s.entry_method
			FROM scans s
			JOIN admins a ON a.id = s.scanned_by_admin_id
			WHERE s.participant_id = $1 AND s.scenario_id = $2
			  AND s.status = 'VALID' AND s.enforce_unique
			LIMIT 1`, o.ParticipantID, o.ScenarioID,
		).Scan(&c.ClaimedBy, &c.ClaimedAt, &c.Method); err != nil {
			return nil, fmt.Errorf("look up existing claim: %w", err)
		}
		return nil, &c
	}
	if pgCode(err) == pgForeignKeyViolation {
		// Only the admin FK can fail here; the other two were just read.
		return nil, fmt.Errorf("%w: your admin account no longer exists", domain.ErrValidation)
	}
	if err != nil {
		return nil, fmt.Errorf("insert override: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit override: %w", err)
	}
	return res, nil
}

// ---- Live feed -----------------------------------------------------------------

const scanEventSelect = `
	SELECT s.id, s.participant_id, p.full_name, p.membership_id,
	       COALESCE(p.date_of_birth > ` + minorCutoff + `, false),
	       s.scenario_id, sc.name, sc.code, a.full_name,
	       s.status, s.entry_method, COALESCE(s.override_reason, ''),
	       s.scanned_at_device, s.synced_at
	FROM scans s
	JOIN participants   p  ON p.id  = s.participant_id
	JOIN scan_scenarios sc ON sc.id = s.scenario_id
	JOIN admins         a  ON a.id  = s.scanned_by_admin_id`

func scanEvents(rows *sql.Rows) ([]domain.ScanEvent, error) {
	defer rows.Close()
	out := make([]domain.ScanEvent, 0, 64)
	for rows.Next() {
		var e domain.ScanEvent
		if err := rows.Scan(&e.ID, &e.ParticipantID, &e.ParticipantName, &e.MembershipID,
			&e.IsMinor, &e.ScenarioID, &e.ScenarioName, &e.ScenarioCode, &e.AdminName,
			&e.Status, &e.EntryMethod, &e.OverrideReason, &e.ScannedAt, &e.SyncedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecentScans returns the newest scans, newest first (initial feed backlog).
func (r *DashboardRepo) RecentScans(ctx context.Context, limit int) ([]domain.ScanEvent, error) {
	rows, err := r.db.QueryContext(ctx, scanEventSelect+`
	ORDER BY s.synced_at DESC, s.id
	LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// ScansSince returns up to limit of the newest scans with synced_at after
// since, newest first. The feed poller de-duplicates by ID.
func (r *DashboardRepo) ScansSince(ctx context.Context, since time.Time, limit int) ([]domain.ScanEvent, error) {
	rows, err := r.db.QueryContext(ctx, scanEventSelect+`
	WHERE s.synced_at > $1
	ORDER BY s.synced_at DESC, s.id
	LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// ---- Devices -------------------------------------------------------------------

// UpsertHeartbeat stores a scanner's latest queue report. An unknown or
// deleted scenario is stored as NULL instead of failing the heartbeat.
func (r *DashboardRepo) UpsertHeartbeat(ctx context.Context, adminID string, hb domain.DeviceHeartbeat) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO scanner_devices
			(device_id, admin_id, scenario_id, pending_count, pending_others, rejected_count,
			 invalid_count, manifest_count, last_sync_at, is_online, user_agent, last_seen_at)
		VALUES ($1, $2, (SELECT id FROM scan_scenarios WHERE id = NULLIF($3, '')::uuid),
		        $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''), NOW())
		ON CONFLICT (device_id) DO UPDATE SET
			admin_id       = EXCLUDED.admin_id,
			scenario_id    = EXCLUDED.scenario_id,
			pending_count  = EXCLUDED.pending_count,
			pending_others = EXCLUDED.pending_others,
			rejected_count = EXCLUDED.rejected_count,
			invalid_count  = EXCLUDED.invalid_count,
			manifest_count = EXCLUDED.manifest_count,
			last_sync_at   = EXCLUDED.last_sync_at,
			is_online      = EXCLUDED.is_online,
			user_agent     = EXCLUDED.user_agent,
			last_seen_at   = NOW()`,
		hb.DeviceID, adminID, hb.ScenarioID, hb.PendingCount, hb.PendingOthers, hb.RejectedCount,
		hb.InvalidCount, hb.ManifestCount, hb.LastSyncAt, hb.IsOnline, hb.UserAgent)
	if pgCode(err) == pgForeignKeyViolation {
		return fmt.Errorf("%w: admin account no longer exists", domain.ErrValidation)
	}
	return err
}

// ListDevices returns every scanner seen recently, most recent first. State is
// derived from the server-side last_seen_at, not the phone's own clock.
func (r *DashboardRepo) ListDevices(ctx context.Context, limit int) ([]domain.DeviceHealthItem, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT d.device_id, d.admin_id, a.full_name, a.email, COALESCE(sc.name, ''),
		       d.pending_count, d.pending_others, d.rejected_count, d.invalid_count,
		       d.manifest_count, d.last_sync_at, d.last_seen_at, d.is_online,
		       COALESCE(d.user_agent, ''),
		       CASE WHEN d.last_seen_at > now() - make_interval(secs => $1) THEN $3
		            WHEN d.last_seen_at > now() - make_interval(secs => $2) THEN $4
		            ELSE $5 END
		FROM scanner_devices d
		JOIN admins a ON a.id = d.admin_id
		LEFT JOIN scan_scenarios sc ON sc.id = d.scenario_id
		ORDER BY d.last_seen_at DESC
		LIMIT $6`,
		domain.DeviceOnlineWindow.Seconds(), domain.DeviceTrackWindow.Seconds(),
		domain.DeviceOnline, domain.DeviceStale, domain.DeviceOffline, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]domain.DeviceHealthItem, 0, 16)
	for rows.Next() {
		var (
			d        domain.DeviceHealthItem
			lastSync sql.NullTime
		)
		if err := rows.Scan(&d.DeviceID, &d.AdminID, &d.AdminName, &d.AdminEmail, &d.ScenarioName,
			&d.PendingCount, &d.PendingOthers, &d.RejectedCount, &d.InvalidCount,
			&d.ManifestCount, &lastSync, &d.LastSeenAt, &d.IsOnline, &d.UserAgent, &d.State); err != nil {
			return nil, err
		}
		if lastSync.Valid {
			t := lastSync.Time
			d.LastSyncAt = &t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---- Venues --------------------------------------------------------------------

// ListVenues returns every venue for the dashboard's venue filter.
func (r *DashboardRepo) ListVenues(ctx context.Context) ([]domain.Venue, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, hall_section, latitude::float8, longitude::float8, COALESCE(description, '')
		FROM venues
		ORDER BY name, hall_section`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]domain.Venue, 0, 16)
	for rows.Next() {
		var v domain.Venue
		if err := rows.Scan(&v.ID, &v.Name, &v.HallSection, &v.Latitude, &v.Longitude, &v.Description); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
