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

// IntegrationRepo stores Google connections (migration 013). Tenant methods
// never read the secret; the worker's methods run as the connecting user.
type IntegrationRepo struct{ db *sql.DB }

func NewIntegrationRepo(db *sql.DB) *IntegrationRepo { return &IntegrationRepo{db: db} }

const integrationCols = `i.id, i.event_id, i.kind, i.config, i.google_email, i.status, i.last_synced_at, i.last_error, i.imported, i.created_at`

func scanIntegration(row interface{ Scan(...any) error }) (domain.Integration, error) {
	var x domain.Integration
	var cfg []byte
	err := row.Scan(&x.ID, &x.EventID, &x.Kind, &cfg, &x.GoogleEmail, &x.Status, &x.LastSyncedAt, &x.LastError, &x.Imported, &x.CreatedAt)
	x.Config = cfg
	return x, err
}

func (r *IntegrationRepo) List(ctx context.Context, es authz.EventScope) ([]domain.Integration, error) {
	out := []domain.Integration{}
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+integrationCols+` FROM integrations i WHERE i.org_id = $1 AND i.event_id = $2 ORDER BY i.kind`,
			es.OrgID(), es.EventID())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			x, err := scanIntegration(rows)
			if err != nil {
				return err
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, err
}

func (r *IntegrationRepo) Get(ctx context.Context, es authz.EventScope, kind string) (*domain.Integration, error) {
	var x domain.Integration
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var err error
		x, err = scanIntegration(tx.QueryRowContext(ctx, `SELECT `+integrationCols+` FROM integrations i
			WHERE i.org_id = $1 AND i.event_id = $2 AND i.kind = $3`, es.OrgID(), es.EventID(), kind))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &x, nil
}

// Connect creates or replaces the event's integration of kind. seal encrypts
// the refresh token bound to the row's ID (kept when reconnecting).
func (r *IntegrationRepo) Connect(ctx context.Context, es authz.EventScope, kind, email string, config json.RawMessage,
	seal func(id string) ([]byte, error)) (*domain.Integration, error) {
	var out domain.Integration
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM integrations WHERE event_id = $1 AND kind = $2 FOR UPDATE`, es.EventID(), kind).Scan(&id)
		exists := err == nil
		if errors.Is(err, sql.ErrNoRows) {
			id = newID()
		} else if err != nil {
			return err
		}
		secret, err := seal(id)
		if err != nil {
			return err
		}
		// No ON CONFLICT: reading EXCLUDED.secret would need SELECT on the
		// secret column, which the tenant role deliberately lacks.
		if exists {
			_, err = tx.ExecContext(ctx, `UPDATE integrations SET config = $2, secret = $3, google_email = $4, status = 'connected',
				last_error = '', connected_by = $5, last_synced_at = NULL, updated_at = NOW() WHERE id = $1`,
				id, []byte(config), secret, email, es.AccountID())
		} else {
			_, err = tx.ExecContext(ctx, `
				INSERT INTO integrations (id, org_id, event_id, kind, config, secret, google_email, status, connected_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7, 'connected', $8)`,
				id, es.OrgID(), es.EventID(), kind, []byte(config), secret, email, es.AccountID())
		}
		if err != nil {
			return err
		}
		if out, err = scanIntegration(tx.QueryRowContext(ctx, `SELECT `+integrationCols+` FROM integrations i WHERE i.id = $1`, id)); err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "integration.connect", TargetType: "integration", TargetID: id,
			Diff: map[string]any{"kind": kind, "google_email": email},
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetConfig replaces an integration's config (e.g. the form's question map).
func (r *IntegrationRepo) SetConfig(ctx context.Context, es authz.EventScope, kind string, config json.RawMessage) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `UPDATE integrations SET config = $4, last_error = '', status = 'connected', updated_at = NOW()
			WHERE org_id = $1 AND event_id = $2 AND kind = $3 RETURNING id`, es.OrgID(), es.EventID(), kind, []byte(config)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "integration.settings", TargetType: "integration", TargetID: id, Diff: map[string]any{"kind": kind},
		})
	})
}

// SyncNow makes the integration due at once.
func (r *IntegrationRepo) SyncNow(ctx context.Context, es authz.EventScope, kind string) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE integrations SET last_synced_at = NULL, updated_at = NOW()
			WHERE org_id = $1 AND event_id = $2 AND kind = $3`, es.OrgID(), es.EventID(), kind)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

func (r *IntegrationRepo) Disconnect(ctx context.Context, es authz.EventScope, kind string) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `DELETE FROM integrations WHERE org_id = $1 AND event_id = $2 AND kind = $3 RETURNING id`,
			es.OrgID(), es.EventID(), kind).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "integration.disconnect", TargetType: "integration", TargetID: id, Diff: map[string]any{"kind": kind},
		})
	})
}

// ---- Worker -----------------------------------------------------------------------

// Due lists connected integrations ready to run: Sheets once the event has
// changed since the last sync and that sync is at least debounce old; forms
// every interval.
func (r *IntegrationRepo) Due(ctx context.Context, debounce, interval time.Duration) ([]domain.IntegrationJob, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+integrationCols+`, i.org_id, i.secret FROM integrations i
		WHERE i.status = 'connected' AND i.secret IS NOT NULL AND (
		  (i.kind = 'google_sheets' AND (i.last_synced_at IS NULL OR (i.last_synced_at < NOW() - $1 * interval '1 second' AND (
		      EXISTS (SELECT 1 FROM event_participants p WHERE p.event_id = i.event_id AND GREATEST(p.registered_at, p.updated_at) > i.last_synced_at)
		   OR EXISTS (SELECT 1 FROM checkins k WHERE k.event_id = i.event_id AND (k.created_at > i.last_synced_at OR k.undone_at > i.last_synced_at))
		   OR EXISTS (SELECT 1 FROM workshop_signups s JOIN workshops w ON w.id = s.workshop_id WHERE w.event_id = i.event_id AND s.updated_at > i.last_synced_at)
		   OR EXISTS (SELECT 1 FROM workshops w WHERE w.event_id = i.event_id AND w.updated_at > i.last_synced_at)))))
		  OR (i.kind = 'google_form' AND (i.config->>'ready')::boolean IS TRUE
		      AND (i.last_synced_at IS NULL OR i.last_synced_at < NOW() - $2 * interval '1 second'))
		  -- a failed sync is retried every interval, changes or not
		  OR (i.last_error <> '' AND i.last_synced_at < NOW() - $2 * interval '1 second'))
		ORDER BY i.last_synced_at NULLS FIRST
		LIMIT 50`, debounce.Seconds(), interval.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.IntegrationJob
	for rows.Next() {
		var j domain.IntegrationJob
		var cfg []byte
		if err := rows.Scan(&j.ID, &j.EventID, &j.Kind, &cfg, &j.GoogleEmail, &j.Status, &j.LastSyncedAt, &j.LastError,
			&j.Imported, &j.CreatedAt, &j.OrgID, &j.Secret); err != nil {
			return nil, err
		}
		j.Config = cfg
		out = append(out, j)
	}
	return out, rows.Err()
}

// Finished records a run: the time, the error ("" on success), whether the
// connection is broken, and how many participants it imported.
func (r *IntegrationRepo) Finished(ctx context.Context, id, errMsg string, broken bool, imported int) error {
	status := "connected"
	if broken {
		status = "error"
	}
	_, err := r.db.ExecContext(ctx, `UPDATE integrations SET last_synced_at = NOW(), last_error = $2, status = $3,
		imported = imported + $4, updated_at = NOW() WHERE id = $1`, id, errMsg, status, imported)
	return err
}

// SheetData is everything the spreadsheet shows.
type SheetData struct {
	Event        domain.Event
	Fields       []domain.FormField
	Participants []domain.EventParticipant
	Checkins     []domain.CheckinRow
	Workshops    []domain.Workshop
}

// LoadSheetData reads an event for its spreadsheet (worker, system path).
func (r *IntegrationRepo) LoadSheetData(ctx context.Context, orgID, eventID string) (*SheetData, error) {
	d := &SheetData{}
	err := withTenant(ctx, r.db, orgID, "", func(tx *sql.Tx) error {
		ev, err := getEvent(ctx, tx, orgID, eventID)
		if err != nil {
			return err
		}
		d.Event = *ev
		if d.Fields, err = listFormFields(ctx, tx, orgID, eventID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+participantCols+` FROM event_participants p
			WHERE p.org_id = $1 AND p.event_id = $2 ORDER BY p.registered_at`, orgID, eventID)
		if err != nil {
			return err
		}
		for rows.Next() {
			p, err := scanParticipant(rows)
			if err != nil {
				rows.Close()
				return err
			}
			d.Participants = append(d.Participants, p)
		}
		rows.Close()
		crows, err := tx.QueryContext(ctx, `SELECT `+checkinCols+`, p.full_name, p.pass_id, c.name`+checkinFrom+`
			JOIN event_participants p ON p.id = k.participant_id JOIN checkpoints c ON c.id = k.checkpoint_id
			WHERE k.org_id = $1 AND k.event_id = $2 ORDER BY k.created_at`, orgID, eventID)
		if err != nil {
			return err
		}
		for crows.Next() {
			var row domain.CheckinRow
			c := &row.Checkin
			if err := crows.Scan(&c.ID, &c.ParticipantID, &c.CheckpointID, &c.StaffID, &c.StaffName, &c.DeviceID,
				&c.CreatedAt, &c.UndoneAt, &row.FullName, &row.PassID, &row.Checkpoint); err != nil {
				crows.Close()
				return err
			}
			d.Checkins = append(d.Checkins, row)
		}
		crows.Close()
		d.Workshops, err = listWorkshops(ctx, tx, orgID, eventID, "")
		return err
	})
	return d, err
}

// EventForImport loads the event a form imports into (worker, system path).
func (r *IntegrationRepo) EventForImport(ctx context.Context, orgID, eventID string) (*domain.Event, []domain.FormField, error) {
	var ev *domain.Event
	var fields []domain.FormField
	err := withTenant(ctx, r.db, orgID, "", func(tx *sql.Tx) error {
		var err error
		if ev, err = getEvent(ctx, tx, orgID, eventID); err != nil {
			return err
		}
		fields, err = listFormFields(ctx, tx, orgID, eventID)
		return err
	})
	return ev, fields, err
}

// Secret reads an integration's sealed refresh token. The caller has already
// found the integration through a tenant scope (Get); the tenant role can't
// read the column, so this runs as the platform.
func (r *IntegrationRepo) Secret(ctx context.Context, orgID, id string) ([]byte, error) {
	var b []byte
	err := r.db.QueryRowContext(ctx, `SELECT secret FROM integrations WHERE org_id = $1 AND id = $2`, orgID, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && b == nil) {
		return nil, domain.ErrNotFound
	}
	return b, err
}

// ImportedRefs are the external references already imported into an event.
func (r *IntegrationRepo) ImportedRefs(ctx context.Context, orgID, eventID string) (map[string]bool, error) {
	out := map[string]bool{}
	err := withTenant(ctx, r.db, orgID, "", func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT external_ref FROM event_participants
			WHERE org_id = $1 AND event_id = $2 AND external_ref IS NOT NULL`, orgID, eventID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ref string
			if err := rows.Scan(&ref); err != nil {
				return err
			}
			out[ref] = true
		}
		return rows.Err()
	})
	return out, err
}
