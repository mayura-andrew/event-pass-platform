package httphandler

// Integration test for the M3 gate's second half: the event migrated from
// the current build keeps its old QR codes working.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/auth"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/internal/passes"
	"github.com/nasa-spaceapps/nsac-qr/internal/repository/postgres"
	"github.com/nasa-spaceapps/nsac-qr/internal/service"
)

func TestLegacyImportKeepsOldQRCodes(t *testing.T) {
	env := newSaaSEnv(t)
	ctx := context.Background()
	key := []byte("legacy-hmac-key-0123456789abcdef")
	type legacy struct{ id, membership, email, payload string }
	var people []legacy
	for i, confirmed := range []bool{true, true, false} {
		l := legacy{membership: fmt.Sprintf("IT%s%d", env.prefix[3:9], i), email: fmt.Sprintf("%sdelegate%d@test.local", env.prefix, i)}
		sig := auth.QRSignature(l.membership, l.email, key)
		if err := env.db.QueryRowContext(ctx, `
			INSERT INTO participants (membership_id, email, full_name, team_name, is_confirmed, qr_signature, custom_attributes)
			VALUES ($1, $2, $3, 'Orbiters', $4, $5, '{"food_preference": "Vegetarian"}') RETURNING id`,
			l.membership, l.email, fmt.Sprintf("Delegate %d", i), confirmed, sig).Scan(&l.id); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(map[string]string{"id": l.id, "sig": sig})
		l.payload = string(b)
		people = append(people, l)
	}
	t.Cleanup(func() {
		env.db.ExecContext(ctx, `DELETE FROM participants WHERE email LIKE $1`, env.prefix+"%")
	})

	ann := env.newBrowser(t, "ann")
	ann.signIn("")
	org := ann.createOrg("legacy")
	res, err := postgres.ImportLegacy(ctx, env.db, org.Slug, ann.email, "Space Apps Colombo 2025", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.Imported < 2 || res.Fields < 2 {
		t.Fatalf("import: %+v", res)
	}
	again, err := postgres.ImportLegacy(ctx, env.db, org.Slug, ann.email, "", res.EventID)
	if err != nil || again.Imported != 0 {
		t.Fatalf("re-run imported %d more (%v); want 0", again.Imported, err)
	}
	if _, err := postgres.ImportLegacy(ctx, env.db, org.Slug, env.prefix+"stranger@test.local", "X", ""); err == nil {
		t.Error("import by someone who isn't in the org was allowed")
	}

	evAPI := "/api/v1/orgs/" + org.Slug + "/events/" + res.EventID
	check := func(payload string) service.PassCheck {
		t.Helper()
		status, b := ann.api("POST", evAPI+"/passes/check", map[string]string{"payload": payload})
		if status != http.StatusOK {
			t.Fatalf("check: HTTP %d %s", status, b)
		}
		var c service.PassCheck
		json.Unmarshal(b, &c)
		return c
	}

	// The QR code a delegate already has: valid, same pass ID as their card.
	c := check(people[0].payload)
	if c.Result != service.CheckValid || c.Participant == nil || c.Participant.PassID != people[0].membership ||
		c.Participant.FullName != "Delegate 0" || c.Participant.Answers["food_preference"] != "Vegetarian" {
		t.Fatalf("old QR code: %+v", c)
	}
	// Forged signature, and the unconfirmed delegate who was never imported.
	forged := fmt.Sprintf(`{"id":"%s","sig":"%064x"}`, people[1].id, 1)
	if c := check(forged); c.Result != service.CheckInvalid {
		t.Errorf("forged old QR code: %+v", c)
	}
	if c := check(people[2].payload); c.Result != service.CheckInvalid {
		t.Errorf("unconfirmed delegate's QR code: %+v", c)
	}
	if c := check("hello"); c.Result != service.CheckInvalid {
		t.Errorf("random text: %+v", c)
	}

	// A real platform pass for another event is recognised as such.
	_, b := ann.api("POST", "/api/v1/orgs/"+org.Slug+"/events", map[string]any{"name": "Other", "starts_at": time.Now().Add(48 * time.Hour)})
	var other domain.Event
	json.Unmarshal(b, &other)
	tok, _ := passes.DerivedKeyRing([]byte("test-pass-key")).Issue(org.ID, other.ID, people[0].id, "X-1")
	if c := check(tok); c.Result != service.CheckWrongEvent {
		t.Errorf("pass for another event: %+v", c)
	}

	// Cancelling an imported delegate stops their old QR code.
	ann.api("PATCH", evAPI+"/participants/"+people[1].id, map[string]string{"status": "cancelled"})
	if c := check(people[1].payload); c.Result != service.CheckCancelled {
		t.Errorf("cancelled delegate's old QR code: %+v", c)
	}

	// Their answers and team came across, and a re-sent pass carries the
	// same old QR payload.
	pc, err := postgres.NewOutboxRepo(env.db).PassContext(ctx, org.ID, people[0].id)
	if err != nil {
		t.Fatal(err)
	}
	if pc.Participant.Answers["food_preference"] != "Vegetarian" || pc.Participant.Answers["team_name"] != "Orbiters" {
		t.Errorf("imported answers: %v", pc.Participant.Answers)
	}
	ps := service.NewPassService(passes.DerivedKeyRing([]byte("test-pass-key")))
	if tok, _ := ps.Token(pc); tok != people[0].payload {
		t.Errorf("imported pass token %q, want the old payload %q", tok, people[0].payload)
	}
	if _, err := ps.PNG(pc); err != nil {
		t.Fatal(err)
	}
}
