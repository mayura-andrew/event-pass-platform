package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type ParticipantRepo struct{ db *sql.DB }

func NewParticipantRepo(db *sql.DB) *ParticipantRepo { return &ParticipantRepo{db: db} }

// ListConfirmedManifest returns every confirmed delegate for scanner hydration.
// Custom attributes are cut down to the scanner popup fields here, in SQL, so
// other values (a guardian's phone number, say) never reach a phone.
func (r *ParticipantRepo) ListConfirmedManifest(ctx context.Context) ([]domain.ParticipantManifest, error) {
	rows, err := r.db.QueryContext(ctx, `
		WITH popup AS (
			SELECT COALESCE(array_agg(field_key), '{}') AS keys
			FROM participant_field_definitions
			WHERE show_in_scanner_popup
		)
		SELECT p.id, p.membership_id, p.full_name, p.email,
		       COALESCE(p.team_name, ''),
		       COALESCE(v.name, 'Unassigned'),
		       COALESCE(v.hall_section, 'N/A'),
		       p.qr_signature,
		       CASE WHEN p.custom_attributes = '{}'::jsonb THEN '{}'::jsonb
		            ELSE (SELECT COALESCE(jsonb_object_agg(e.key, e.value), '{}'::jsonb)
		                  FROM jsonb_each(p.custom_attributes) AS e
		                  WHERE e.key = ANY (popup.keys))
		       END
		FROM participants p
		CROSS JOIN popup
		LEFT JOIN venues v ON v.id = p.assigned_venue_id
		WHERE p.is_confirmed
		ORDER BY p.membership_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Non-nil so an empty event serializes as [] rather than null.
	manifest := make([]domain.ParticipantManifest, 0, 1024)
	for rows.Next() {
		var m domain.ParticipantManifest
		var custom []byte
		if err := rows.Scan(&m.ID, &m.MembershipID, &m.FullName, &m.Email,
			&m.TeamName, &m.VenueName, &m.HallSection, &m.QRSignature, &custom); err != nil {
			return nil, err
		}
		m.CustomAttributes = custom
		manifest = append(manifest, m)
	}
	return manifest, rows.Err()
}

// GetPass looks up a confirmed delegate by membership ID AND email. Requiring
// both stops anyone who guesses a sequential membership ID from pulling
// someone else's QR pass.
func (r *ParticipantRepo) GetPass(ctx context.Context, membershipID, email string) (*domain.ParticipantPass, error) {
	var (
		p                     domain.ParticipantPass
		venueID, vName, vHall sql.NullString
		vDesc                 sql.NullString
		vLat, vLng            sql.NullFloat64
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT p.id, p.membership_id, p.full_name, COALESCE(p.team_name, ''), p.qr_signature,
		       COALESCE(p.custom_attributes->>$3, ''),
		       v.id, v.name, v.hall_section, v.latitude::float8, v.longitude::float8, v.description
		FROM participants p
		LEFT JOIN venues v ON v.id = p.assigned_venue_id
		WHERE p.membership_id = $1 AND p.email = $2 AND p.is_confirmed`,
		membershipID, email, domain.FoodPreferenceKey,
	).Scan(&p.ID, &p.MembershipID, &p.FullName, &p.TeamName, &p.QRSignature, &p.FoodPreference,
		&venueID, &vName, &vHall, &vLat, &vLng, &vDesc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if venueID.Valid {
		p.Venue = &domain.Venue{
			ID: venueID.String, Name: vName.String, HallSection: vHall.String,
			Latitude: vLat.Float64, Longitude: vLng.Float64, Description: vDesc.String,
		}
	}
	return &p, nil
}

// UpdateFoodPreference sets the food_preference custom attribute for the
// delegate matching membership ID AND email. The field definition is
// re-created if an organizer deleted it, so the value stays visible on the
// dashboard and scanners.
func (r *ParticipantRepo) UpdateFoodPreference(ctx context.Context, membershipID, email, value string) (string, error) {
	var saved string
	err := r.db.QueryRowContext(ctx, `
		WITH field AS (
			INSERT INTO participant_field_definitions
			    (field_key, field_label, data_type, show_in_scanner_popup, show_in_dashboard_filter, sort_order, source)
			VALUES ($3, 'Food Preference', 'text', true, true, 10, 'seed')
			ON CONFLICT (field_key) DO NOTHING
		)
		UPDATE participants
		SET custom_attributes = custom_attributes || jsonb_build_object($3::text, $4::text)
		WHERE membership_id = $1 AND email = $2 AND is_confirmed
		RETURNING custom_attributes->>$3`,
		membershipID, email, domain.FoodPreferenceKey, value,
	).Scan(&saved)
	if errors.Is(err, sql.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	return saved, err
}
