package httphandler

// Integration test for M3 pass emails: registering queues an email in the
// same transaction, the worker renders the pass and sends it, statuses get
// their own emails, and failures retry.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/passes"
	"github.com/mayura-andrew/event-pass-platform/internal/repository/postgres"
	"github.com/mayura-andrew/event-pass-platform/internal/service"
)

type failingMailer struct{ err error }

func (f failingMailer) Send(context.Context, service.Message) error { return f.err }

func TestPassEmails(t *testing.T) {
	env := newSaaSEnv(t)
	ctx := context.Background()
	ann := env.newBrowser(t, "ann")
	ann.signIn("")
	org := ann.createOrg("mail")
	base := "/api/v1/orgs/" + org.Slug + "/events"
	_, b := ann.api("POST", base, map[string]any{"name": "Example Event", "visibility": "unlisted",
		"starts_at": time.Now().Add(10 * 24 * time.Hour), "venue": map[string]any{"name": "BMICH"}})
	var ev domain.Event
	json.Unmarshal(b, &ev)
	evAPI := base + "/" + ev.ID
	ann.api("POST", evAPI+"/fields", map[string]any{"label": "T-shirt size", "type": "select", "options": []string{"S", "M"}, "visibility": "pass"})
	ann.api("POST", evAPI+"/publish", nil)

	keys := passes.DerivedKeyRing([]byte("test-pass-key"))
	ps := service.NewPassService(keys)
	outbox := postgres.NewOutboxRepo(env.db)
	worker := service.NewOutboxWorker(outbox, env.mail, ps, "http://pass.test", "Event Pass")
	page := "/e/" + org.Slug + "/" + ev.Slug
	anon := env.newBrowser(t, "anon")
	register := func(name, email string) {
		t.Helper()
		res := anon.form("POST", page+"/register", url.Values{"full_name": {name}, "email": {email}, "q.t_shirt_size": {"M"}})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("register %s: HTTP %d", email, res.StatusCode)
		}
		res.Body.Close()
	}
	sent := func(to string) []service.Message {
		env.mail.mu.Lock()
		defer env.mail.mu.Unlock()
		var out []service.Message
		for _, m := range env.mail.sent {
			if m.To.Address == to {
				out = append(out, m)
			}
		}
		return out
	}
	drain := func() {
		t.Helper()
		for i := 0; i < 5; i++ {
			if n, err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			} else if n == 0 {
				return
			}
		}
	}

	// Register: within one worker pass the email is out, with the pass.
	ben := env.prefix + "ben@test.local"
	start := time.Now()
	register("Ben Silva", ben)
	drain()
	msgs := sent(ben)
	if len(msgs) != 1 {
		t.Fatalf("pass emails to ben: %d, want 1", len(msgs))
	}
	m := msgs[0]
	if time.Since(start) > 60*time.Second {
		t.Errorf("pass email took %s (spec: within 60 s)", time.Since(start))
	}
	if m.Subject != "Your pass for Example Event" || len(m.Inline) != 1 || len(m.Attachments) != 1 ||
		!bytes.HasPrefix(m.Inline[0].Data, []byte("\x89PNG")) || !bytes.HasPrefix(m.Attachments[0].Data, []byte("%PDF")) {
		t.Fatalf("pass email: subject %q, %d inline, %d attachments", m.Subject, len(m.Inline), len(m.Attachments))
	}
	if !strings.Contains(m.Text, "Pass ID: SAC-") || !strings.Contains(m.Text, "Ben Silva") || m.ReplyTo == nil || m.ReplyTo.Address != ann.email {
		t.Errorf("text version or reply-to missing: %q reply-to %v", m.Text, m.ReplyTo)
	}
	if !strings.Contains(m.HTML, `src="cid:pass@event"`) {
		t.Error("HTML doesn't show the inline pass")
	}

	// The QR token on that pass verifies and names this event and participant.
	_, b = ann.api("GET", evAPI+"/participants?q=ben", nil)
	var list struct{ Participants []domain.EventParticipant }
	json.Unmarshal(b, &list)
	p := list.Participants[0]
	pc, err := outbox.PassContext(ctx, org.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := ps.Token(pc)
	claims, err := keys.Verify(tok)
	if err != nil || claims.EventID != ev.ID || claims.ParticipantID != p.ID || claims.PassID != p.PassID || claims.OrgID != org.ID {
		t.Fatalf("pass token: %+v, %v", claims, err)
	}
	card, _ := ps.Card(pc)
	if len(card.Lines) != 1 || card.Lines[0].Value != "M" {
		t.Errorf("pass-visible answers on the card: %+v", card.Lines)
	}

	// Registering again re-sends, but not twice in a row.
	register("Ben Silva", ben)
	register("Ben Silva", ben)
	drain()
	if n := len(sent(ben)); n != 1 {
		t.Errorf("re-registering right after: %d emails, want 1 (the resend limit)", n)
	}
	env.db.ExecContext(ctx, `UPDATE outbox SET created_at = NOW() - INTERVAL '10 minutes' WHERE to_email = $1`, ben)
	register("Ben Silva", ben)
	drain()
	if n := len(sent(ben)); n != 2 {
		t.Errorf("re-registering later: %d emails, want 2", n)
	}

	// Approval: "received" first, the pass once approved.
	ann.api("PATCH", evAPI+"/registration", map[string]any{"reg_mode": "approval"})
	cha := env.prefix + "chamari@test.local"
	register("Chamari Fernando", cha)
	drain()
	if msgs := sent(cha); len(msgs) != 1 || !strings.HasPrefix(msgs[0].Subject, "Registration received") || len(msgs[0].Attachments) != 0 {
		t.Fatalf("pending registration email: %d, %v", len(msgs), msgs)
	}
	_, b = ann.api("GET", evAPI+"/participants?q=chamari", nil)
	json.Unmarshal(b, &list)
	chaID := list.Participants[0].ID
	ann.api("PATCH", evAPI+"/participants/"+chaID, map[string]string{"status": "registered"})
	drain()
	if msgs := sent(cha); len(msgs) != 2 || msgs[1].Subject != "Your pass for Example Event" {
		t.Fatalf("after approval: %d emails", len(msgs))
	}

	// Approve then cancel before the worker runs: only the cancellation goes.
	dil := env.prefix + "dilan@test.local"
	register("Dilan Perera", dil)
	drain()
	_, b = ann.api("GET", evAPI+"/participants?q=dilan", nil)
	json.Unmarshal(b, &list)
	ann.api("PATCH", evAPI+"/participants/"+list.Participants[0].ID, map[string]string{"status": "registered"})
	ann.api("PATCH", evAPI+"/participants/"+list.Participants[0].ID, map[string]string{"status": "cancelled"})
	drain()
	msgs = sent(dil)
	if len(msgs) != 2 || !strings.HasPrefix(msgs[1].Subject, "Registration cancelled") {
		var subjects []string
		for _, m := range msgs {
			subjects = append(subjects, m.Subject)
		}
		t.Errorf("approve then cancel: %v, want received then cancelled", subjects)
	}

	// A failing mail server: the email goes back in the queue for later.
	broken := service.NewOutboxWorker(outbox, failingMailer{errors.New("smtp: 421 try later")}, ps, "http://pass.test", "Event Pass")
	esha := env.prefix + "esha@test.local"
	register("Esha Raj", esha)
	if _, err := broken.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var status, lastErr string
	var attempts int
	var next time.Time
	env.db.QueryRowContext(ctx, `SELECT status, attempts, next_attempt_at, COALESCE(last_error, '') FROM outbox WHERE to_email = $1`, esha).
		Scan(&status, &attempts, &next, &lastErr)
	if status != "queued" || attempts != 1 || time.Until(next) < 20*time.Second || !strings.Contains(lastErr, "421") {
		t.Errorf("after a failed send: status %s, attempts %d, next in %s, error %q", status, attempts, time.Until(next), lastErr)
	}

	// Organizers can preview and download any pass.
	res := ann.get("/o/" + org.Slug + "/events/" + ev.ID + "/participants/" + p.ID + "/pass.png")
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/png" {
		t.Errorf("pass preview: HTTP %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	res.Body.Close()
	res = ann.get("/o/" + org.Slug + "/events/" + ev.ID + "/participants/" + p.ID + "/pass.pdf?download=1")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("pass PDF download: HTTP %d %s", res.StatusCode, res.Header.Get("Content-Disposition"))
	}
	res.Body.Close()
}
