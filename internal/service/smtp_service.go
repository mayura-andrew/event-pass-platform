package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/mail"
	"net/textproto"
	"net/url"
	"strings"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// ---- Pass confirmation email ------------------------------------------------------

// passQRContentID is how the HTML refers to the embedded QR image.
const passQRContentID = "pass-qr@nsac"

// PassQRPayload is exactly what the pass page encodes in its QR
// (JSON.stringify({id, sig}) in public/js/pass.js), so the scanner reads a
// QR from the email the same way.
func PassQRPayload(participantID, signature string) string {
	b, _ := json.Marshal(struct {
		ID  string `json:"id"`
		Sig string `json:"sig"`
	}{participantID, signature})
	return string(b)
}

// PassQRPNG renders the pass QR as a PNG: error correction M like the pass
// page, 480 px so it scans from a phone screen held at a desk.
func PassQRPNG(participantID, signature string) ([]byte, error) {
	png, err := qrcode.Encode(PassQRPayload(participantID, signature), qrcode.Medium, 480)
	if err != nil {
		return nil, fmt.Errorf("render pass QR: %w", err)
	}
	return png, nil
}

// PassURL is the portal deep link: pass.html opens the pass straight away
// with both values filled in (see public/js/pass.js).
func PassURL(baseURL, membershipID, email string) string {
	q := url.Values{"id": {membershipID}, "email": {email}}
	return strings.TrimRight(baseURL, "/") + "/pass.html?" + q.Encode()
}

type passEmailData struct {
	FullName     string
	MembershipID string
	TeamName     string
	Venue        string // "Innovation Center, Hall A", empty when unassigned
	VenueNote    string
	PassURL      string
	EventName    string
	QRContentID  string
}

var passHTML = template.Must(template.New("pass").Parse(`<!DOCTYPE html>
<html lang="en"><body style="margin:0;padding:0;background:#EEF1F5;font-family:Arial,Helvetica,sans-serif;color:#13233D;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#EEF1F5;padding:24px 12px;">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:520px;background:#FFFFFF;border-radius:14px;overflow:hidden;">
  <tr><td style="background:#0D173E;padding:22px 26px;color:#FFFFFF;">
    <div style="font-size:12px;letter-spacing:2px;color:#E8392B;font-weight:bold;">NASA</div>
    <div style="font-size:22px;font-weight:bold;letter-spacing:.5px;">SPACE APPS COLOMBO</div>
    <div style="font-size:14px;opacity:.85;margin-top:4px;">Your delegate pass is ready</div>
  </td></tr>
  <tr><td style="padding:24px 26px 6px;font-size:15px;line-height:1.55;">
    <p style="margin:0 0 14px;">Hi {{.FullName}},</p>
    <p style="margin:0 0 14px;">You're confirmed for {{.EventName}}. Show the QR code below at the check-in desk and at each checkpoint.</p>
  </td></tr>
  <tr><td style="padding:0 26px;" align="center">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#F4F6FA;border:1px solid #C6CFDB;border-radius:10px;">
      <tr><td align="center" style="padding:18px 16px 6px;">
        <img src="cid:{{.QRContentID}}" width="220" height="220" alt="Your pass QR code ({{.MembershipID}})" style="display:block;width:220px;height:220px;border:0;background:#FFFFFF;">
      </td></tr>
      <tr><td align="center" style="padding:4px 16px 2px;font-size:13px;color:#4A5A73;">Membership ID</td></tr>
      <tr><td align="center" style="padding:0 16px 14px;font-size:24px;font-weight:bold;letter-spacing:1px;font-family:'Courier New',Courier,monospace;">{{.MembershipID}}</td></tr>
    </table>
  </td></tr>
  <tr><td style="padding:16px 26px 0;font-size:15px;line-height:1.5;">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0">
      <tr><td style="padding:4px 0;color:#4A5A73;width:90px;vertical-align:top;">Team</td><td style="padding:4px 0;font-weight:bold;">{{if .TeamName}}{{.TeamName}}{{else}}Not in a team yet{{end}}</td></tr>
      <tr><td style="padding:4px 0;color:#4A5A73;vertical-align:top;">Venue</td><td style="padding:4px 0;font-weight:bold;">{{if .Venue}}{{.Venue}}{{if .VenueNote}}<div style="font-weight:normal;color:#4A5A73;font-size:13px;">{{.VenueNote}}</div>{{end}}{{else}}To be announced{{end}}</td></tr>
    </table>
  </td></tr>
  <tr><td style="padding:22px 26px 6px;" align="center">
    <a href="{{.PassURL}}" style="display:inline-block;background:#1F4FD1;color:#FFFFFF;text-decoration:none;font-weight:bold;padding:13px 26px;border-radius:8px;font-size:15px;">Open my pass</a>
    <div style="font-size:12px;color:#4A5A73;margin-top:8px;">Save it to your phone or print it from there.</div>
  </td></tr>
  <tr><td style="padding:16px 26px 24px;font-size:13px;line-height:1.55;color:#4A5A73;">
    <p style="margin:0 0 10px;">Can't see the QR code? Open your pass with the button above, or give your membership ID at the desk.</p>
    <p style="margin:0;">Your pass is personal: please don't share this email.</p>
  </td></tr>
</table>
</td></tr></table>
</body></html>`))

// RenderPassEmail builds the pass confirmation email for a claimed job,
// with the QR embedded as an inline image.
func RenderPassEmail(j *domain.EmailJob, baseURL, eventName string) (Message, error) {
	to, err := mail.ParseAddress(j.RecipientEmail)
	if err != nil {
		return Message{}, fmt.Errorf("recipient %q: %w", j.RecipientEmail, err)
	}
	// The display name goes through mail.Address encoding; control
	// characters are refused at ingestion, this is a second check.
	if ValidDisplayName(j.FullName) {
		to.Name = j.FullName
	}
	png, err := PassQRPNG(j.ParticipantID, j.QRSignature)
	if err != nil {
		return Message{}, err
	}

	d := passEmailData{
		FullName:     j.FullName,
		MembershipID: j.MembershipID,
		TeamName:     j.TeamName,
		VenueNote:    j.VenueNote,
		// The pass portal looks delegates up by the email they registered
		// with, which is what the job was queued for.
		PassURL:     PassURL(baseURL, j.MembershipID, j.Email),
		EventName:   eventName,
		QRContentID: passQRContentID,
	}
	if j.VenueName != "" {
		d.Venue = j.VenueName
		if j.HallSection != "" {
			d.Venue += ", " + j.HallSection
		}
	}

	var t strings.Builder
	fmt.Fprintf(&t, "Hi %s,\n\n", d.FullName)
	fmt.Fprintf(&t, "You're confirmed for %s. Your delegate pass is ready.\n\n", eventName)
	fmt.Fprintf(&t, "Membership ID: %s\n", d.MembershipID)
	if d.TeamName != "" {
		fmt.Fprintf(&t, "Team: %s\n", d.TeamName)
	}
	if d.Venue != "" {
		fmt.Fprintf(&t, "Venue: %s\n", d.Venue)
	} else {
		t.WriteString("Venue: to be announced\n")
	}
	fmt.Fprintf(&t, "\nOpen your pass (QR code for check-in): %s\n\n", d.PassURL)
	t.WriteString("Show the QR code at the check-in desk and at each checkpoint. Your pass is personal: please don't share this email.\n")

	var h bytes.Buffer
	if err := passHTML.Execute(&h, d); err != nil {
		return Message{}, fmt.Errorf("render pass email: %w", err)
	}
	return Message{
		To:      *to,
		Subject: fmt.Sprintf("Your %s delegate pass (%s)", eventName, d.MembershipID),
		Text:    t.String(),
		HTML:    h.String(),
		Inline: []InlineImage{{
			ContentID:   passQRContentID,
			ContentType: "image/png",
			Filename:    "pass-" + safeFilename(d.MembershipID) + ".png",
			Data:        png,
		}},
	}, nil
}

func safeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "pass"
	}
	return b.String()
}

// ---- Send failures ------------------------------------------------------------------

// sendFailure says what a failed send means for the queue.
type sendFailure int

const (
	// failTransient: try this message again later (network trouble, 4xx).
	failTransient sendFailure = iota
	// failRecipient: this message can never go through (no such mailbox,
	// address rejected). Retrying only burns the daily allowance.
	failRecipient
	// failSender: nothing can be sent right now (wrong app password,
	// Gmail's daily cap). Don't count it against the recipient; pause.
	failSender
)

// classifySendError decides how the queue reacts to err from Mailer.Send.
// net/smtp reports server replies as *textproto.Error with the SMTP code;
// anything else (DNS, TCP, TLS, timeouts) is treated as transient.
func classifySendError(err error) (sendFailure, string) {
	msg := err.Error()
	lower := strings.ToLower(msg)

	// Gmail: "550 5.4.5 Daily user sending limit exceeded" (also 421/451
	// variants). It's a 5xx, but about the account, not the recipient.
	if strings.Contains(lower, "5.4.5") || strings.Contains(lower, "sending limit") ||
		strings.Contains(lower, "sending quota") {
		return failSender, "Gmail's daily sending limit was reached. Resume the queue after it resets (rolling 24 hours)."
	}

	var tp *textproto.Error
	if errors.As(err, &tp) {
		switch {
		case tp.Code == 530 || tp.Code == 534 || tp.Code == 535 || tp.Code == 538:
			return failSender, "The mail server rejected the sign-in (SMTP " + fmt.Sprint(tp.Code) +
				"). Check SMTP_USERNAME and the Gmail app password, then resume."
		case tp.Code >= 500:
			return failRecipient, ""
		default:
			return failTransient, ""
		}
	}
	if strings.Contains(lower, "smtp auth") {
		return failSender, "Couldn't sign in to the mail server. Check SMTP_USERNAME and the Gmail app password, then resume."
	}
	return failTransient, ""
}
