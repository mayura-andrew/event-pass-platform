package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// ProfileRepo stores participant profiles, live locations and beta feedback
// (migration 015). Participant methods take the org and event the service
// has already checked the caller is registered for, and run on the org's
// system path; organizer methods take an event scope.
type ProfileRepo struct{ db *sql.DB }

func NewProfileRepo(db *sql.DB) *ProfileRepo { return &ProfileRepo{db: db} }

// Profile is a participant's profile (an empty private one if they have
// none yet).
func (r *ProfileRepo) Profile(ctx context.Context, orgID, participantID string) (*domain.ParticipantProfile, error) {
	p := &domain.ParticipantProfile{ParticipantID: participantID, Visibility: domain.ProfilePrivate}
	err := withTenant(ctx, r.db, orgID, "", func(tx *sql.Tx) error {
		var links []byte
		err := tx.QueryRowContext(ctx, `SELECT headline, links, photo IS NOT NULL, photo_updated_at, visibility
			FROM participant_profiles WHERE org_id = $1 AND participant_id = $2`, orgID, participantID).
			Scan(&p.Headline, &links, &p.HasPhoto, &p.PhotoUpdatedAt, &p.Visibility)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return json.Unmarshal(links, &p.Links)
	})
	return p, err
}

// ProfileUpdate is a save from the pass page. Photo nil keeps the current
// one unless RemovePhoto.
type ProfileUpdate struct {
	Headline    string
	Links       domain.ProfileLinks
	Visibility  string
	Photo       []byte
	RemovePhoto bool
}

// SaveProfile writes a profile. Going private also stops location sharing.
func (r *ProfileRepo) SaveProfile(ctx context.Context, orgID, eventID, participantID string, u ProfileUpdate) error {
	links, _ := json.Marshal(u.Links)
	return withTenant(ctx, r.db, orgID, "", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO participant_profiles (participant_id, org_id, event_id, headline, links, visibility, photo, photo_updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, CASE WHEN $7::bytea IS NULL THEN NULL ELSE NOW() END)
			ON CONFLICT (participant_id) DO UPDATE SET headline = EXCLUDED.headline, links = EXCLUDED.links,
			       visibility = EXCLUDED.visibility, updated_at = NOW(),
			       photo = CASE WHEN $8 THEN NULL WHEN $7::bytea IS NULL THEN participant_profiles.photo ELSE $7::bytea END,
			       photo_updated_at = CASE WHEN $8 THEN NULL WHEN $7::bytea IS NULL THEN participant_profiles.photo_updated_at ELSE NOW() END`,
			participantID, orgID, eventID, u.Headline, links, u.Visibility, u.Photo, u.RemovePhoto); err != nil {
			return err
		}
		if u.Visibility != domain.ProfilePublic {
			_, err := tx.ExecContext(ctx, `DELETE FROM participant_locations WHERE org_id = $1 AND participant_id = $2`, orgID, participantID)
			return err
		}
		return nil
	})
}

// Photo returns a participant's photo; with publicOnly, only when their
// profile is public.
func (r *ProfileRepo) Photo(ctx context.Context, orgID, eventID, participantID string, publicOnly bool) ([]byte, error) {
	var photo []byte
	err := withTenant(ctx, r.db, orgID, "", func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT photo FROM participant_profiles
			WHERE org_id = $1 AND event_id = $2 AND participant_id = $3 AND photo IS NOT NULL
			  AND (NOT $4 OR visibility = 'public')`, orgID, eventID, participantID, publicOnly).Scan(&photo)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return photo, err
}

func (r *ProfileRepo) MapEnabled(ctx context.Context, orgID, eventID string) (bool, error) {
	var on bool
	err := withTenant(ctx, r.db, orgID, "", func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT enabled FROM participant_map_settings WHERE org_id = $1 AND event_id = $2`,
			orgID, eventID).Scan(&on)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return on, err
}

// People lists the event's registered participants with public profiles,
// and the caller (whatever their visibility). Locations older than fresh
// are left out.
func (r *ProfileRepo) People(ctx context.Context, orgID, eventID, meID string, fresh time.Duration) ([]domain.MapPerson, error) {
	out := []domain.MapPerson{}
	err := withTenant(ctx, r.db, orgID, "", func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT p.id, p.full_name, COALESCE(pr.headline, ''), COALESCE(pr.links, '{}'::jsonb),
			       COALESCE(EXTRACT(EPOCH FROM pr.photo_updated_at)::bigint, 0),
			       l.lat, l.lng, l.accuracy_m, l.updated_at
			FROM event_participants p
			LEFT JOIN participant_profiles pr ON pr.participant_id = p.id
			LEFT JOIN participant_locations l ON l.participant_id = p.id AND l.updated_at > NOW() - $4 * interval '1 second'
			WHERE p.org_id = $1 AND p.event_id = $2
			  AND (p.id::text = $3 OR (p.status = 'registered' AND pr.visibility = 'public'))
			ORDER BY (l.updated_at IS NULL), p.full_name
			LIMIT 2000`, orgID, eventID, meID, fresh.Seconds())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m domain.MapPerson
			var links []byte
			var acc sql.NullFloat64
			if err := rows.Scan(&m.ParticipantID, &m.FullName, &m.Headline, &links, &m.PhotoVersion,
				&m.Lat, &m.Lng, &acc, &m.LocatedAt); err != nil {
				return err
			}
			if acc.Valid {
				m.AccuracyM = &acc.Float64
			}
			_ = json.Unmarshal(links, &m.Links)
			m.Me = m.ParticipantID == meID
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// SetLocation records where a sharing participant is. Updates closer than
// a few seconds apart are ignored, and stale rows from anyone are cleared.
func (r *ProfileRepo) SetLocation(ctx context.Context, orgID, eventID, participantID string, lat, lng, accuracy float64) error {
	return withTenant(ctx, r.db, orgID, "", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO participant_locations (participant_id, org_id, event_id, lat, lng, accuracy_m)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, 0))
			ON CONFLICT (participant_id) DO UPDATE SET lat = EXCLUDED.lat, lng = EXCLUDED.lng,
			       accuracy_m = EXCLUDED.accuracy_m, updated_at = NOW()
			WHERE participant_locations.updated_at < NOW() - interval '3 seconds'`,
			participantID, orgID, eventID, lat, lng, accuracy); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM participant_locations WHERE org_id = $1 AND event_id = $2
			AND updated_at < NOW() - interval '1 hour'`, orgID, eventID)
		return err
	})
}

func (r *ProfileRepo) ClearLocation(ctx context.Context, orgID, participantID string) error {
	return withTenant(ctx, r.db, orgID, "", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM participant_locations WHERE org_id = $1 AND participant_id = $2`, orgID, participantID)
		return err
	})
}

func (r *ProfileRepo) AddFeedback(ctx context.Context, orgID, eventID, participantID string, rating int, comment string) error {
	return withTenant(ctx, r.db, orgID, "", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO beta_feedback (id, org_id, event_id, participant_id, rating, comment)
			VALUES ($1, $2, $3, $4, $5, $6)`, newID(), orgID, eventID, participantID, rating, comment)
		return err
	})
}

// ---- Organizers -----------------------------------------------------------------

func (r *ProfileRepo) SetMapEnabled(ctx context.Context, es authz.EventScope, enabled bool) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO participant_map_settings (event_id, org_id, enabled) VALUES ($1, $2, $3)
			ON CONFLICT (event_id) DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = NOW()`,
			es.EventID(), es.OrgID(), enabled); err != nil {
			return err
		}
		if !enabled { // off means nobody's location is kept
			if _, err := tx.ExecContext(ctx, `DELETE FROM participant_locations WHERE org_id = $1 AND event_id = $2`,
				es.OrgID(), es.EventID()); err != nil {
				return err
			}
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "event.participant_map", TargetType: "event", TargetID: es.EventID(),
			Diff: map[string]any{"enabled": enabled},
		})
	})
}

// MapStats is the beta's state at an event, with the latest feedback.
func (r *ProfileRepo) MapStats(ctx context.Context, es authz.EventScope, fresh time.Duration) (*domain.MapStats, error) {
	s := &domain.MapStats{Feedback: []domain.BetaFeedback{}}
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT
			COALESCE((SELECT enabled FROM participant_map_settings WHERE event_id = $2), FALSE),
			(SELECT count(*) FROM participant_profiles WHERE org_id = $1 AND event_id = $2 AND visibility = 'public'),
			(SELECT count(*) FROM participant_locations WHERE org_id = $1 AND event_id = $2 AND updated_at > NOW() - $3 * interval '1 second'),
			(SELECT count(*) FROM beta_feedback WHERE org_id = $1 AND event_id = $2 AND rating = 1),
			(SELECT count(*) FROM beta_feedback WHERE org_id = $1 AND event_id = $2 AND rating = -1)`,
			es.OrgID(), es.EventID(), fresh.Seconds()).Scan(&s.Enabled, &s.PublicProfiles, &s.SharingNow, &s.Up, &s.Down); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT f.rating, f.comment, COALESCE(p.full_name, ''), f.created_at
			FROM beta_feedback f LEFT JOIN event_participants p ON p.id = f.participant_id
			WHERE f.org_id = $1 AND f.event_id = $2 ORDER BY f.created_at DESC LIMIT 200`, es.OrgID(), es.EventID())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f domain.BetaFeedback
			if err := rows.Scan(&f.Rating, &f.Comment, &f.FullName, &f.CreatedAt); err != nil {
				return err
			}
			s.Feedback = append(s.Feedback, f)
		}
		return rows.Err()
	})
	return s, err
}
