package postgres

import (
	"context"
	"database/sql"
	"log/slog"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// FeedChannel is the NOTIFY channel for the live feed. Payloads are event
// IDs only: anyone who can LISTEN learns that an event changed, nothing
// more.
const FeedChannel = "event_pass_feed"

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Notify tells every replica's live feed that an event changed.
func (r *CheckinRepo) Notify(ctx context.Context, eventID string) error {
	if !uuidRe.MatchString(eventID) {
		return nil
	}
	return withSystem(ctx, r.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `SELECT pg_notify($1, $2)`, FeedChannel, eventID)
		return err
	})
}

// Listen calls fn with each payload NOTIFYed on channel until ctx ends,
// reconnecting with backoff. It needs a session connection: through a
// transaction pooler (Supabase port 6543) LISTEN never delivers, and the
// live feed's poll is what keeps dashboards current.
func Listen(ctx context.Context, dsn, channel string, fn func(string)) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := listenOnce(ctx, dsn, channel, fn, func() { backoff = time.Second })
		if ctx.Err() != nil {
			return
		}
		slog.Warn("feed LISTEN stopped; the poll covers until it reconnects", "err", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func listenOnce(ctx context.Context, dsn, channel string, fn func(string), connected func()) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return err
	}
	connected()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if uuidRe.MatchString(n.Payload) {
			fn(n.Payload)
		}
	}
}
