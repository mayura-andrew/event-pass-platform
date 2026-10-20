// Command loadtest is the spec's nightly load test (milestone M4 gate): 50
// orgs with live events, 30 scanners at 20 scans a minute, and public
// registrations at 1,000 per 10 minutes, against a running API. It seeds its
// own data straight into the database, then reports p50/p95/p99 and exits 1
// if scan p95 is over the limit or anything failed.
//
//	export DATABASE_URL='postgres://...'   # the API's database
//	export JWT_SECRET=...                  # or PASS_SIGNING_KEYS, as the API has
//	go run ./scripts/loadtest -base http://localhost:8080 -duration 10m
//
// Use a disposable database: it creates orgs named lt-<run>-*. The API needs
// PASS_RATE_PER_MIN high enough for the registration rate from one address.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	mrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/internal/passes"
	"github.com/nasa-spaceapps/nsac-qr/internal/repository/postgres"
	"github.com/nasa-spaceapps/nsac-qr/internal/service"
)

type target struct {
	slug, eventID, eventSlug, checkpoint string
	cookie, csrf                         string
	tokens                               []string
}

type sample struct {
	op  string
	d   time.Duration
	err error
}

func main() {
	base := flag.String("base", "http://localhost:8080", "API base URL")
	orgs := flag.Int("orgs", 50, "orgs, each with one live event")
	people := flag.Int("people", 20, "registered participants per event")
	scanners := flag.Int("scanners", 30, "scanning phones")
	perMin := flag.Float64("scan-rate", 20, "scans per minute per scanner")
	regsPer10 := flag.Float64("registrations", 1000, "public registrations per 10 minutes")
	duration := flag.Duration("duration", 10*time.Minute, "how long to run")
	limit := flag.Duration("p95", 300*time.Millisecond, "fail if scan p95 is above this")
	flag.Parse()

	if err := run(*base, *orgs, *people, *scanners, *perMin, *regsPer10, *duration, *limit); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

func run(base string, nOrgs, nPeople, nScanners int, perMin, regsPer10 float64, duration, limit time.Duration) error {
	ctx := context.Background()
	db, err := postgres.Open(ctx, os.Getenv("DATABASE_URL"), 8)
	if err != nil {
		return err
	}
	defer db.Close()
	keys := passes.DerivedKeyRing([]byte(os.Getenv("JWT_SECRET")))
	if spec := os.Getenv("PASS_SIGNING_KEYS"); spec != "" {
		if keys, err = passes.ParseKeyRing(spec); err != nil {
			return err
		}
	}
	ps := service.NewPassService(keys)

	run := randHex(3)
	fmt.Printf("seeding %d orgs x %d participants (run %s)...\n", nOrgs, nPeople, run)
	targets, err := seed(ctx, db, ps, run, nOrgs, nPeople)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 100}}
	samples := make(chan sample, 10000)
	stop := time.Now().Add(duration)
	var wg sync.WaitGroup

	// Scanners: each at perMin, spread over the orgs' events.
	interval := time.Duration(float64(time.Minute) / perMin)
	for i := 0; i < nScanners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			t := targets[i%len(targets)]
			time.Sleep(time.Duration(mrand.Int63n(int64(interval)))) // don't all fire at once
			for time.Now().Before(stop) {
				body, _ := json.Marshal(map[string]any{"payload": t.tokens[mrand.Intn(len(t.tokens))], "checkpoint_id": t.checkpoint,
					"record": true, "idempotency_key": randHex(12), "device_id": fmt.Sprintf("lt-%d", i)})
				req, _ := http.NewRequest("POST", base+"/api/v1/orgs/"+t.slug+"/events/"+t.eventID+"/scan", strings.NewReader(string(body)))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Cookie", "sid="+t.cookie)
				req.Header.Set("X-CSRF-Token", t.csrf)
				samples <- timed("scan", client, req)
				time.Sleep(interval)
			}
		}(i)
	}

	// Registrations: regsPer10 per 10 minutes, across the events.
	if regsPer10 > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gap := time.Duration(float64(10*time.Minute) / regsPer10)
			for n := 0; time.Now().Before(stop); n++ {
				t := targets[n%len(targets)]
				form := url.Values{"full_name": {fmt.Sprintf("Load Person %d", n)}, "email": {fmt.Sprintf("lt-%s-r%d@load.test", run, n)}}
				req, _ := http.NewRequest("POST", base+"/e/"+t.slug+"/"+t.eventSlug+"/register", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				go func() { samples <- timed("register", client, req) }()
				time.Sleep(gap)
			}
		}()
	}

	results := map[string][]time.Duration{}
	failures := map[string][]string{}
	done := make(chan struct{})
	go func() {
		for s := range samples {
			results[s.op] = append(results[s.op], s.d)
			if s.err != nil && len(failures[s.op]) < 5 {
				failures[s.op] = append(failures[s.op], s.err.Error())
			}
			if s.err != nil {
				failures[s.op+" count"] = append(failures[s.op+" count"], "")
			}
		}
		close(done)
	}()
	wg.Wait()
	time.Sleep(2 * time.Second) // let in-flight registrations finish
	close(samples)
	<-done

	failed := false
	for _, op := range []string{"scan", "register"} {
		d := results[op]
		if len(d) == 0 {
			continue
		}
		slices.Sort(d)
		pct := func(p float64) time.Duration { return d[min(len(d)-1, int(float64(len(d))*p))] }
		errs := len(failures[op+" count"])
		fmt.Printf("%-9s n=%-6d errors=%-4d p50=%-8s p95=%-8s p99=%-8s max=%s\n", op, len(d), errs,
			pct(.5).Round(time.Millisecond), pct(.95).Round(time.Millisecond), pct(.99).Round(time.Millisecond), d[len(d)-1].Round(time.Millisecond))
		for _, f := range failures[op] {
			fmt.Println("   ", f)
		}
		if errs > 0 || (op == "scan" && pct(.95) > limit) {
			failed = true
		}
	}
	if failed {
		return fmt.Errorf("failed: errors, or scan p95 over %s", limit)
	}
	fmt.Println("ok")
	return nil
}

func timed(op string, c *http.Client, req *http.Request) sample {
	start := time.Now()
	res, err := c.Do(req)
	d := time.Since(start)
	if err != nil {
		return sample{op, d, err}
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return sample{op, d, fmt.Errorf("HTTP %d: %.120s", res.StatusCode, b)}
	}
	if op == "scan" {
		var r struct{ Result string }
		json.Unmarshal(b, &r)
		if r.Result != domain.ScanRecorded && r.Result != domain.ScanDuplicate {
			return sample{op, d, fmt.Errorf("scan result %q", r.Result)}
		}
	}
	return sample{op, d, nil}
}

// seed makes nOrgs orgs, each with an owner, a published event open for
// registration, nPeople participants with valid passes and a scanner
// session (the owner's).
func seed(ctx context.Context, db *sql.DB, ps *service.PassService, run string, nOrgs, nPeople int) ([]target, error) {
	accounts, orgs, events := postgres.NewAccountRepo(db), postgres.NewOrgRepo(db), postgres.NewEventRepo(db)
	regs, checkins, outbox := postgres.NewRegistrationRepo(db), postgres.NewCheckinRepo(db), postgres.NewOutboxRepo(db)
	var out []target
	for i := 0; i < nOrgs; i++ {
		email := fmt.Sprintf("lt-%s-o%d@load.test", run, i)
		acct, err := accounts.SignIn(ctx, domain.ProviderEmail, email, email, fmt.Sprintf("Scanner %d", i), "")
		if err != nil {
			return nil, err
		}
		as := authz.NewAccountScope(acct.ID)
		m, err := orgs.CreateOrg(ctx, as, fmt.Sprintf("Load org %d", i), fmt.Sprintf("lt-%s-%d", run, i))
		if err != nil {
			return nil, err
		}
		sc := authz.ForMember(as, m.ID, domain.OrgRoleOwner)
		starts := time.Now().Add(time.Hour)
		ev, err := events.CreateEvent(ctx, sc, &domain.Event{Slug: "live", Name: "Live event", Type: "hackathon", TimeZone: "UTC",
			Visibility: "unlisted", Language: "en", Tags: []string{}, StartsAt: &starts, RegMode: domain.RegOpen, Waitlist: true,
			PassPrefix: "LT"}, "", domain.Audit{Action: "event.create", TargetType: "event"})
		if err != nil {
			return nil, err
		}
		es, _ := authz.ForEvent(sc, ev.ID, ev.OwnerID, "", ev.Settings)
		if err := events.SetPublished(ctx, es, true); err != nil {
			return nil, err
		}
		t := target{slug: m.Slug, eventID: ev.ID, eventSlug: ev.Slug}
		cps, err := checkins.Checkpoints(ctx, es, false)
		if err != nil {
			return nil, err
		}
		for _, c := range cps {
			if c.Kind == domain.KindCheckIn {
				t.checkpoint = c.ID
			}
		}
		for j := 0; j < nPeople; j++ {
			p, err := regs.Register(ctx, ev, domain.EventParticipant{Email: fmt.Sprintf("lt-%s-o%d-p%d@load.test", run, i, j),
				FullName: fmt.Sprintf("Person %d", j), Answers: map[string]any{}}, func() string { return service.NewPassID("LT") })
			if err != nil {
				return nil, err
			}
			pc, err := outbox.PassContext(ctx, m.ID, p.ID)
			if err != nil {
				return nil, err
			}
			tok, err := ps.Token(pc)
			if err != nil {
				return nil, err
			}
			t.tokens = append(t.tokens, tok)
		}
		t.cookie, t.csrf = randHex(32), randHex(16)
		if _, err := accounts.CreateSession(ctx, acct.ID, service.HashToken(t.cookie), t.csrf, time.Now().Add(24*time.Hour), "loadtest", ""); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
