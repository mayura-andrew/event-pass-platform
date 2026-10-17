// Command seed ingests confirmed participants (and optionally venues) from CSV
// exports of the registration Google Form into Supabase.
//
// Usage:
//
//	export DATABASE_URL=postgres://...   HMAC_SECRET_KEY=<32+ random bytes>
//	go run ./scripts/seed -participants participants.csv [-venues venues.csv] [-dry-run]
//
// participants.csv header (column order is free, names are matched):
//
//	membership_id,email,full_name,team_name,venue_name,hall_section,date_of_birth
//
// date_of_birth is optional and read as YYYY-MM-DD unless -dob-layout says
// otherwise (e.g. -dob-layout 1/2/2006 for a US-locale Google Sheets export).
// A blank or unreadable date is reported and stored as unknown; the delegate
// is still imported, and a blank cell never erases a date already stored.
//
// venues.csv header:
//
//	name,hall_section,latitude,longitude,description
//
// Re-running is safe: rows are upserted on membership_id / (name, hall_section).
// If a participant's email changes, their signature changes too and the old
// QR stops verifying, which is what you want.
package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/nasa-spaceapps/nsac-qr/internal/auth"
)

func main() {
	participantsPath := flag.String("participants", "", "path to participants CSV (required)")
	venuesPath := flag.String("venues", "", "optional path to venues CSV, loaded first")
	dryRun := flag.Bool("dry-run", false, "validate and print, but roll back")
	dobLayout := flag.String("dob-layout", "2006-01-02", "Go time layout of the date_of_birth column")
	flag.Parse()

	if *participantsPath == "" {
		flag.Usage()
		os.Exit(2)
	}
	dsn := os.Getenv("DATABASE_URL")
	secret := []byte(os.Getenv("HMAC_SECRET_KEY"))
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	if len(secret) < auth.MinQRKeyBytes {
		log.Fatal("HMAC_SECRET_KEY must be at least 32 bytes (e.g. `openssl rand -hex 32`)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("ping db: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		log.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	if *venuesPath != "" {
		n, err := loadVenues(ctx, tx, *venuesPath)
		if err != nil {
			log.Fatalf("venues: %v", err)
		}
		log.Printf("venues upserted: %d", n)
	}

	stats, err := loadParticipants(ctx, tx, *participantsPath, secret, *dobLayout)
	if err != nil {
		log.Fatalf("participants: %v", err)
	}
	log.Printf("participants upserted: %d, skipped: %d, without date of birth: %d", stats.ok, stats.skipped, stats.noDOB)

	if *dryRun {
		log.Print("dry run: rolling back")
		return
	}
	if err := tx.Commit(); err != nil {
		log.Fatalf("commit: %v", err)
	}
	log.Print("done")
}

// ---- CSV helpers -------------------------------------------------------------

type csvFile struct {
	r   *csv.Reader
	f   *os.File
	col map[string]int
}

func openCSV(path string, required ...string) (*csvFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := csv.NewReader(f)
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = -1 // Google Sheets exports can have ragged trailing columns

	header, err := r.Read()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("read header: %w", err)
	}
	col := make(map[string]int, len(header))
	for i, h := range header {
		// Strip a UTF-8 BOM (Excel) and normalize "Full Name" -> "full_name".
		h = strings.TrimPrefix(h, "\ufeff")
		h = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(h), " ", "_"))
		col[h] = i
	}
	for _, name := range required {
		if _, ok := col[name]; !ok {
			f.Close()
			return nil, fmt.Errorf("missing required column %q", name)
		}
	}
	return &csvFile{r: r, f: f, col: col}, nil
}

func (c *csvFile) get(rec []string, name string) string {
	i, ok := c.col[name]
	if !ok || i >= len(rec) {
		return ""
	}
	return strings.TrimSpace(rec[i])
}

// ---- Venues ------------------------------------------------------------------

func loadVenues(ctx context.Context, tx *sql.Tx, path string) (int, error) {
	c, err := openCSV(path, "name", "hall_section", "latitude", "longitude")
	if err != nil {
		return 0, err
	}
	defer c.f.Close()

	n, line := 0, 1
	for {
		rec, err := c.r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		line++
		if err != nil {
			return n, fmt.Errorf("line %d: %w", line, err)
		}
		lat, err1 := strconv.ParseFloat(c.get(rec, "latitude"), 64)
		lng, err2 := strconv.ParseFloat(c.get(rec, "longitude"), 64)
		if err1 != nil || err2 != nil {
			return n, fmt.Errorf("line %d: latitude/longitude must be numbers", line)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO venues (name, hall_section, latitude, longitude, description)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''))
			ON CONFLICT (name, hall_section) DO UPDATE SET
				latitude = EXCLUDED.latitude,
				longitude = EXCLUDED.longitude,
				description = EXCLUDED.description`,
			c.get(rec, "name"), c.get(rec, "hall_section"), lat, lng, c.get(rec, "description"))
		if err != nil {
			return n, fmt.Errorf("line %d: %w", line, err)
		}
		n++
	}
	return n, nil
}

// ---- Participants ------------------------------------------------------------

type loadStats struct{ ok, skipped, noDOB int }

// parseDOB returns a valid date of birth or an invalid NullTime. Dates in the
// future or before 1900 are typos; the database CHECK would reject them too.
func parseDOB(raw, layout string) (sql.NullTime, error) {
	if raw == "" {
		return sql.NullTime{}, nil
	}
	t, err := time.Parse(layout, raw)
	if err != nil {
		return sql.NullTime{}, fmt.Errorf("date_of_birth %q does not match layout %q", raw, layout)
	}
	if t.Year() < 1900 || t.After(time.Now()) {
		return sql.NullTime{}, fmt.Errorf("date_of_birth %q is out of range", raw)
	}
	return sql.NullTime{Time: t, Valid: true}, nil
}

func loadParticipants(ctx context.Context, tx *sql.Tx, path string, secret []byte, dobLayout string) (loadStats, error) {
	var st loadStats
	c, err := openCSV(path, "membership_id", "email", "full_name")
	if err != nil {
		return st, err
	}
	defer c.f.Close()

	// Cache venue IDs by "name|hall_section".
	venues := map[string]string{}
	rows, err := tx.QueryContext(ctx, `SELECT id, name, hall_section FROM venues`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var id, name, hall string
		if err := rows.Scan(&id, &name, &hall); err != nil {
			rows.Close()
			return st, err
		}
		venues[strings.ToLower(name+"|"+hall)] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	line := 1
	for {
		rec, err := c.r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		line++
		if err != nil {
			return st, fmt.Errorf("line %d: %w", line, err)
		}

		membershipID := c.get(rec, "membership_id")
		email := strings.ToLower(c.get(rec, "email"))
		fullName := c.get(rec, "full_name")
		team := c.get(rec, "team_name")

		if membershipID == "" || email == "" || fullName == "" || !strings.Contains(email, "@") {
			log.Printf("line %d: skipped (missing membership_id, email or full_name)", line)
			st.skipped++
			continue
		}

		var venueID sql.NullString
		if vn := c.get(rec, "venue_name"); vn != "" {
			key := strings.ToLower(vn + "|" + c.get(rec, "hall_section"))
			if id, ok := venues[key]; ok {
				venueID = sql.NullString{String: id, Valid: true}
			} else {
				log.Printf("line %d: venue %q / %q not found; leaving unassigned", line, vn, c.get(rec, "hall_section"))
			}
		}

		dob, err := parseDOB(c.get(rec, "date_of_birth"), dobLayout)
		if err != nil {
			log.Printf("line %d (%s): %v; storing as unknown", line, membershipID, err)
		}
		if !dob.Valid {
			st.noDOB++
		}

		sig := auth.QRSignature(membershipID, email, secret)

		// A savepoint per row means one bad row (e.g. an email already used by
		// a different membership ID) is reported and skipped instead of
		// aborting the whole import.
		if _, err := tx.ExecContext(ctx, "SAVEPOINT row"); err != nil {
			return st, err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO participants
				(membership_id, email, full_name, team_name, is_confirmed, qr_signature,
				 assigned_venue_id, date_of_birth)
			VALUES ($1, $2, $3, NULLIF($4, ''), true, $5, $6, $7::date)
			ON CONFLICT (membership_id) DO UPDATE SET
				email             = EXCLUDED.email,
				full_name         = EXCLUDED.full_name,
				team_name         = EXCLUDED.team_name,
				is_confirmed      = true,
				qr_signature      = EXCLUDED.qr_signature,
				assigned_venue_id = EXCLUDED.assigned_venue_id,
				-- A CSV without the column (or a blank cell) keeps the stored date.
				date_of_birth     = COALESCE(EXCLUDED.date_of_birth, participants.date_of_birth)`,
			membershipID, email, fullName, team, sig, venueID, dob)
		if err != nil {
			if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT row"); rbErr != nil {
				return st, rbErr
			}
			log.Printf("line %d (%s): skipped: %v", line, membershipID, err)
			st.skipped++
			continue
		}
		if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT row"); err != nil {
			return st, err
		}
		st.ok++
	}
	return st, nil
}
