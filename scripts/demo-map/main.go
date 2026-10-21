// Command demo-map brings the participant map beta to life for a demo or a
// feedback session: it adds a handful of clearly marked demo participants
// with public profiles to an event, turns the map on, and walks them around
// the venue, updating their locations every few seconds until stopped.
//
//	export DATABASE_URL='postgres://...'   # the API's database
//	go run ./scripts/demo-map -event <event-id>            # Ctrl+C to stop
//	go run ./scripts/demo-map -event <event-id> -cleanup   # remove them again
//
// For local and staging databases only: the people it adds are fake
// (emails at example.invalid, "(demo)" after their names).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var demoPeople = []struct{ name, headline, linkedin, github, website string }{
	{"Kavindi Perera", "Robotics, University of Moratuwa", "https://www.linkedin.com/in/example-kavindi", "https://github.com/example-kavindi", ""},
	{"Tharindu Silva", "Frontend developer, looking for a team", "https://www.linkedin.com/in/example-tharindu", "", "https://tharindu.example.org"},
	{"Ishara Fernando", "Remote sensing and GIS", "", "https://github.com/example-ishara", ""},
	{"Nimesh Jayawardena", "Mentor · cloud and data", "https://www.linkedin.com/in/example-nimesh", "", ""},
	{"Sanduni Wickramasinghe", "Designer: UI, posters, pitch decks", "", "", "https://sanduni.example.org"},
	{"Ruwan Bandara", "Space Apps 2025 winner, ask me anything", "https://www.linkedin.com/in/example-ruwan", "https://github.com/example-ruwan", ""},
}

func main() {
	eventID := flag.String("event", "", "event ID (required)")
	n := flag.Int("n", len(demoPeople), "how many demo participants (at most 6)")
	lat := flag.Float64("lat", 6.9010, "venue latitude, when the event has none (default: BMICH)")
	lng := flag.Float64("lng", 79.8722, "venue longitude")
	cleanup := flag.Bool("cleanup", false, "remove the demo participants and stop")
	flag.Parse()
	if *eventID == "" || os.Getenv("DATABASE_URL") == "" {
		fmt.Fprintln(os.Stderr, "usage: DATABASE_URL=... go run ./scripts/demo-map -event <event-id> [-cleanup]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	check(err)
	defer db.Close()

	if *cleanup {
		res, err := db.ExecContext(ctx, `DELETE FROM event_participants WHERE event_id = $1 AND email LIKE 'demo-map-%@example.invalid'`, *eventID)
		check(err)
		n, _ := res.RowsAffected()
		fmt.Printf("removed %d demo participants\n", n)
		return
	}

	var orgID, prefix string
	var venue []byte
	check(db.QueryRowContext(ctx, `SELECT org_id, pass_prefix, venue FROM events WHERE id = $1`, *eventID).Scan(&orgID, &prefix, &venue))
	var v struct{ Lat, Lng *float64 }
	_ = json.Unmarshal(venue, &v)
	if v.Lat != nil && v.Lng != nil {
		*lat, *lng = *v.Lat, *v.Lng
	}

	type walker struct {
		id       string
		lat, lng float64
		heading  float64
	}
	var people []*walker
	for i, p := range demoPeople[:min(*n, len(demoPeople))] {
		email := fmt.Sprintf("demo-map-%d@example.invalid", i+1)
		links, _ := json.Marshal(map[string]string{"linkedin": p.linkedin, "github": p.github, "website": p.website})
		var id string
		check(db.QueryRowContext(ctx, `
			INSERT INTO event_participants (id, org_id, event_id, email, full_name, status, pass_id, answers, source)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, 'registered', $5, '{}', 'import')
			ON CONFLICT (event_id, email) DO UPDATE SET full_name = EXCLUDED.full_name, status = 'registered'
			RETURNING id`, orgID, *eventID, email, p.name+" (demo)", fmt.Sprintf("%s-DEMO-%04d", prefix, i+1)).Scan(&id))
		_, err := db.ExecContext(ctx, `
			INSERT INTO participant_profiles (participant_id, org_id, event_id, headline, links, visibility)
			VALUES ($1, $2, $3, $4, $5, 'public')
			ON CONFLICT (participant_id) DO UPDATE SET headline = EXCLUDED.headline, links = EXCLUDED.links, visibility = 'public'`,
			id, orgID, *eventID, p.headline, links)
		check(err)
		// Start somewhere within about 120 m of the venue.
		people = append(people, &walker{id: id, lat: *lat + (rand.Float64()-0.5)*0.002, lng: *lng + (rand.Float64()-0.5)*0.002,
			heading: rand.Float64() * 2 * math.Pi})
	}
	_, err = db.ExecContext(ctx, `INSERT INTO participant_map_settings (event_id, org_id, enabled) VALUES ($1, $2, TRUE)
		ON CONFLICT (event_id) DO UPDATE SET enabled = TRUE, updated_at = NOW()`, *eventID, orgID)
	check(err)
	fmt.Printf("%d demo participants walking around %.4f, %.4f; the map is on. Ctrl+C to stop (they vanish from the map 3 minutes later).\n",
		len(people), *lat, *lng)

	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		for _, w := range people {
			// A slow walk (about 1 m/s) that turns now and then and stays near the venue.
			w.heading += (rand.Float64() - 0.5) * 0.8
			if math.Hypot(w.lat-*lat, w.lng-*lng) > 0.0015 {
				w.heading = math.Atan2(*lat-w.lat, *lng-w.lng)
			}
			w.lat += math.Sin(w.heading) * 0.000045
			w.lng += math.Cos(w.heading) * 0.000045
			_, err := db.ExecContext(ctx, `
				INSERT INTO participant_locations (participant_id, org_id, event_id, lat, lng, accuracy_m)
				VALUES ($1, $2, $3, $4, $5, 8)
				ON CONFLICT (participant_id) DO UPDATE SET lat = EXCLUDED.lat, lng = EXCLUDED.lng, updated_at = NOW()`,
				w.id, orgID, *eventID, w.lat, w.lng)
			if ctx.Err() != nil {
				return
			}
			check(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo-map:", err)
		os.Exit(1)
	}
}
