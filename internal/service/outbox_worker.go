package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// OutboxStore is the worker's queue (postgres.OutboxRepo).
type OutboxStore interface {
	Claim(ctx context.Context, limit int, lease time.Duration) ([]domain.OutboxJob, error)
	MarkSent(ctx context.Context, id string) error
	MarkFailed(ctx context.Context, id, msg string, retryAt *time.Time) error
	PassContext(ctx context.Context, orgID, participantID string) (*domain.PassContext, error)
}

// OutboxWorker sends queued participant emails. Spec gate: a pass email
// arrives within 60 seconds of registering; the queue is polled every
// couple of seconds.
type OutboxWorker struct {
	store    OutboxStore
	mailer   Mailer
	passes   *PassService
	baseURL  string
	product  string
	Interval time.Duration
}

func NewOutboxWorker(store OutboxStore, mailer Mailer, passes *PassService, baseURL, product string) *OutboxWorker {
	return &OutboxWorker{store: store, mailer: mailer, passes: passes, baseURL: baseURL, product: product, Interval: 2 * time.Second}
}

// retryDelays: after the first failure wait 30 s, then 2 min, ...; after the
// last the email is given up and stays "failed" for organizers to see.
var retryDelays = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}

func (w *OutboxWorker) Run(ctx context.Context) {
	slog.Info("outbox worker started", "interval", w.Interval)
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		if n, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Error("outbox", "err", err)
		} else if n > 0 {
			slog.Info("outbox sent", "emails", n)
		}
		select {
		case <-ctx.Done():
			slog.Info("outbox worker stopped")
			return
		case <-t.C:
		}
	}
}

// RunOnce sends what is due and returns how many went out.
func (w *OutboxWorker) RunOnce(ctx context.Context) (int, error) {
	jobs, err := w.store.Claim(ctx, 10, 2*time.Minute)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		if err := w.send(ctx, j); err != nil {
			var retry *time.Time
			if j.Attempts <= len(retryDelays) {
				t := time.Now().Add(retryDelays[j.Attempts-1])
				retry = &t
			}
			msg := err.Error()
			if len(msg) > 500 {
				msg = msg[:500]
			}
			slog.Warn("outbox email failed", "job", j.ID, "attempt", j.Attempts, "retry", retry != nil, "err", err)
			if err := w.store.MarkFailed(ctx, j.ID, msg, retry); err != nil {
				return sent, err
			}
			continue
		}
		if err := w.store.MarkSent(ctx, j.ID); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}

var errSkipped = errors.New("skipped")

func (w *OutboxWorker) send(ctx context.Context, j domain.OutboxJob) error {
	pc, err := w.store.PassContext(ctx, j.OrgID, j.ParticipantID)
	if err != nil {
		return err
	}
	// The status changed since this was queued (approved, then cancelled):
	// the later email says what's true now.
	if domain.MailForStatus[pc.Participant.Status] != j.Template {
		slog.Info("outbox email skipped: status changed", "job", j.ID, "status", pc.Participant.Status)
		return nil
	}
	m, err := w.Message(pc, j.Template)
	if err != nil {
		return err
	}
	m.To = mail.Address{Name: pc.Participant.FullName, Address: j.ToEmail}
	return w.mailer.Send(ctx, m)
}

// Message builds a participant email. The pass email carries the pass as an
// inline image and a PDF, plus a text version with the name, pass ID and
// event for screen readers and image-blocking mail apps.
func (w *OutboxWorker) Message(pc *domain.PassContext, tmpl string) (Message, error) {
	p, e := pc.Participant, pc.Event
	when := e.DateRange()
	if where := e.Where(); where != "" {
		when += " · " + where
	}
	pm := passMail{Product: w.product, OrgName: pc.OrgName, EventName: e.Name, When: when, Name: p.FullName,
		PassID: p.PassID, PortalURL: w.baseURL + "/me"}
	var m Message
	switch tmpl {
	case domain.MailPassIssued:
		png, err := w.passes.PNG(pc)
		if err != nil {
			return m, err
		}
		pdf, err := w.passes.PDF(pc)
		if err != nil {
			return m, err
		}
		m.Subject = "Your pass for " + e.Name
		pm.Heading = "Your pass"
		pm.Paragraphs = []string{
			fmt.Sprintf("Hi %s, you're registered for %s (%s). Show this QR code at the entrance.", p.FullName, e.Name, when),
			"The pass is also attached as a PDF to print at actual size.",
		}
		pm.ImageCID = "pass@event"
		m.Inline = []InlineImage{{ContentID: "pass@event", ContentType: "image/png", Filename: PassFilename(p.PassID, "png"), Data: png}}
		m.Attachments = []Attachment{{ContentType: "application/pdf", Filename: PassFilename(p.PassID, "pdf"), Data: pdf}}
	case domain.MailReceived:
		m.Subject = "Registration received: " + e.Name
		pm.Heading = "Registration received"
		pm.Paragraphs = []string{fmt.Sprintf("Hi %s, thanks for registering for %s (%s). The organizers review each registration; you'll get your pass by email when yours is approved.", p.FullName, e.Name, when)}
	case domain.MailWaitlisted:
		m.Subject = "You're on the waitlist: " + e.Name
		pm.Heading = "You're on the waitlist"
		pm.Paragraphs = []string{fmt.Sprintf("Hi %s, %s is full, so you're on the waitlist. If a place opens up we'll email your pass.", p.FullName, e.Name)}
	case domain.MailCancelled:
		m.Subject = "Registration cancelled: " + e.Name
		pm.Heading = "Registration cancelled"
		pm.Paragraphs = []string{fmt.Sprintf("Hi %s, your registration for %s was cancelled, and its pass no longer works. If this is a mistake, reply to this email.", p.FullName, e.Name)}
	default:
		return m, fmt.Errorf("%w: unknown template %q", errSkipped, tmpl)
	}

	var t strings.Builder
	for _, para := range pm.Paragraphs {
		t.WriteString(para + "\n\n")
	}
	fmt.Fprintf(&t, "Name: %s\nPass ID: %s\nEvent: %s (%s)\n\nYour passes: %s\n", p.FullName, p.PassID, e.Name, when, pm.PortalURL)
	m.Text = t.String()
	var h bytes.Buffer
	if err := passMailHTML.Execute(&h, pm); err != nil {
		return m, err
	}
	m.HTML = h.String()
	if e.OwnerEmail != "" {
		m.ReplyTo = &mail.Address{Name: pc.OrgName, Address: e.OwnerEmail}
	}
	return m, nil
}

type passMail struct {
	Product, OrgName, EventName, When, Name, PassID, PortalURL string
	Heading                                                    string
	Paragraphs                                                 []string
	ImageCID                                                   string
}

var passMailHTML = template.Must(template.New("pass").Parse(`<!DOCTYPE html>
<html lang="en"><body style="margin:0;padding:0;background:#EEF2F0;font-family:Arial,Helvetica,sans-serif;color:#15202B;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#EEF2F0;padding:24px 12px;">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:520px;background:#FFFFFF;border-radius:14px;overflow:hidden;">
  <tr><td style="background:#15202B;padding:20px 26px;color:#FFFFFF;">
    <div style="font-size:13px;letter-spacing:1px;opacity:.8;">{{.OrgName}}</div>
    <div style="font-size:20px;font-weight:bold;margin-top:2px;">{{.EventName}}</div>
    <div style="font-size:14px;opacity:.85;margin-top:4px;">{{.Heading}}</div>
  </td></tr>
  <tr><td style="padding:24px 26px 8px;font-size:15px;line-height:1.55;">
    {{range .Paragraphs}}<p style="margin:0 0 14px;">{{.}}</p>{{end}}
  </td></tr>
  {{if .ImageCID}}<tr><td align="center" style="padding:0 26px 10px;">
    <img src="cid:{{.ImageCID}}" width="300" alt="Pass for {{.Name}}, pass ID {{.PassID}}, with a QR code" style="display:block;width:300px;max-width:100%;height:auto;border:1px solid #D3DBD8;border-radius:12px;">
  </td></tr>{{end}}
  <tr><td style="padding:6px 26px 6px;font-size:14px;line-height:1.6;">
    <b>Pass ID:</b> {{.PassID}}<br><b>When:</b> {{.When}}
  </td></tr>
  <tr><td style="padding:12px 26px 6px;" align="center">
    <a href="{{.PortalURL}}" style="display:inline-block;background:#2350C8;color:#FFFFFF;text-decoration:none;font-weight:bold;padding:13px 26px;border-radius:8px;font-size:15px;">Open my passes</a>
  </td></tr>
  <tr><td style="padding:16px 26px 24px;font-size:12px;line-height:1.55;color:#5B6874;">
    Sent by {{.Product}} for {{.OrgName}}. Reply to this email to reach the organizers.
  </td></tr>
</table>
</td></tr></table>
</body></html>`))
