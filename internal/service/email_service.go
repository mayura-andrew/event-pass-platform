package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
	"unicode"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// ---- Temporary passwords -------------------------------------------------------

// tempPasswordAlphabet leaves out characters that are easy to misread when
// typed from an email on a phone: 0/O/o, 1/l/I, and quote-like symbols.
const (
	tempUpper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	tempLower   = "abcdefghijkmnpqrstuvwxyz"
	tempDigits  = "23456789"
	tempSymbols = "!@#$%*?"
	// TempPasswordLength: 12 characters from 63 symbols is about 71 bits.
	TempPasswordLength = 12
)

// GenerateTempPassword returns a random 12-character password containing at
// least one upper-case letter, lower-case letter, digit and symbol.
func GenerateTempPassword() (string, error) {
	alphabet := tempUpper + tempLower + tempDigits + tempSymbols
	for {
		buf := make([]byte, TempPasswordLength)
		for i := range buf {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			if err != nil {
				return "", fmt.Errorf("generate password: %w", err)
			}
			buf[i] = alphabet[n.Int64()]
		}
		p := string(buf)
		if strings.ContainsAny(p, tempUpper) && strings.ContainsAny(p, tempLower) &&
			strings.ContainsAny(p, tempDigits) && strings.ContainsAny(p, tempSymbols) {
			return p, nil
		}
		// Rejection sampling keeps the distribution uniform over valid passwords.
	}
}

// ---- Mailer --------------------------------------------------------------------

// Message is one outgoing email with plain-text and HTML bodies.
type Message struct {
	To      mail.Address
	Subject string
	Text    string
	HTML    string
}

type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// SMTP security modes.
const (
	SMTPStartTLS = "starttls" // port 587: plain connect, then upgrade (required)
	SMTPTLS      = "tls"      // port 465: TLS from the first byte
	SMTPNone     = "none"     // local test servers only (e.g. Mailpit)
)

type SMTPConfig struct {
	Host     string
	Port     string
	Username string
	Password string
	From     mail.Address
	Security string // SMTPStartTLS | SMTPTLS | SMTPNone
}

// SMTPMailer sends mail through one SMTP relay (Gmail/Workspace, SES, Resend,
// Mailgun, Postmark, ...). It opens a connection per message, which is plenty
// for staff provisioning volumes.
type SMTPMailer struct{ cfg SMTPConfig }

func NewSMTPMailer(cfg SMTPConfig) (*SMTPMailer, error) {
	switch cfg.Security {
	case SMTPStartTLS, SMTPTLS, SMTPNone:
	default:
		return nil, fmt.Errorf("SMTP_SECURITY must be starttls, tls or none, got %q", cfg.Security)
	}
	if cfg.From.Address == "" {
		return nil, errors.New("SMTP_FROM is required when SMTP_HOST is set")
	}
	if cfg.Security == SMTPNone && cfg.Username != "" {
		// Never send credentials over an unencrypted connection.
		return nil, errors.New("SMTP_SECURITY=none cannot be combined with SMTP_USERNAME")
	}
	return &SMTPMailer{cfg: cfg}, nil
}

const smtpTimeout = 20 * time.Second

// Probe connects, upgrades to TLS and authenticates without sending
// anything. Run at startup, it reports the two usual deployment problems
// early: the hosting provider blocking outbound SMTP ports, and a wrong
// password (for Gmail, a missing or mistyped app password).
func (s *SMTPMailer) Probe(ctx context.Context) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Quit()
}

func (s *SMTPMailer) Send(ctx context.Context, m Message) error {
	raw, err := buildMIME(s.cfg.From, m)
	if err != nil {
		return err
	}
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.Mail(s.cfg.From.Address); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	if err := c.Rcpt(m.To.Address); err != nil {
		return fmt.Errorf("smtp RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return c.Quit()
}

// dial opens a connection that is encrypted (unless SMTP_SECURITY=none) and
// authenticated, ready for MAIL FROM.
func (s *SMTPMailer) dial(ctx context.Context) (*smtp.Client, error) {
	addr := net.JoinHostPort(s.cfg.Host, s.cfg.Port)
	deadline := time.Now().Add(smtpTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialer := &net.Dialer{Deadline: deadline}
	tlsCfg := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}

	var (
		conn net.Conn
		err  error
	)
	if s.cfg.Security == SMTPTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	// net/smtp has no context support; the deadline bounds the whole exchange.
	_ = conn.SetDeadline(deadline)

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("smtp handshake: %w", err)
	}
	if s.cfg.Security == SMTPStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			c.Close()
			return nil, errors.New("smtp server does not offer STARTTLS")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			c.Close()
			return nil, fmt.Errorf("starttls: %w", err)
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			c.Close()
			return nil, fmt.Errorf("smtp auth: %w", err)
		}
	}
	return c, nil
}

// disabledMailer is used when SMTP_HOST is unset. Staff creation then fails
// clearly instead of creating accounts nobody can sign in to.
type disabledMailer struct{}

func (disabledMailer) Send(context.Context, Message) error {
	return fmt.Errorf("%w: email is not configured on the server (set SMTP_HOST)", domain.ErrEmailUnavailable)
}

// DisabledMailer returns a Mailer that always reports email as unavailable.
func DisabledMailer() Mailer { return disabledMailer{} }

// buildMIME renders a multipart/alternative message with CRLF line endings.
// Header values come from mail.Address and mime encoders, so a name or
// subject can't inject extra headers.
func buildMIME(from mail.Address, m Message) ([]byte, error) {
	boundary, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	msgID, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	domainPart := "localhost"
	if at := strings.LastIndexByte(from.Address, '@'); at >= 0 {
		domainPart = from.Address[at+1:]
	}

	var b bytes.Buffer
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	hdr("From", from.String())
	hdr("To", m.To.String())
	hdr("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	hdr("Date", time.Now().Format(time.RFC1123Z))
	hdr("Message-ID", fmt.Sprintf("<%s@%s>", msgID, domainPart))
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", fmt.Sprintf(`multipart/alternative; boundary="%s"`, boundary))
	b.WriteString("\r\n")

	for _, part := range []struct{ ctype, body string }{
		{"text/plain; charset=utf-8", m.Text},
		{"text/html; charset=utf-8", m.HTML},
	} {
		fmt.Fprintf(&b, "--%s\r\n", boundary)
		fmt.Fprintf(&b, "Content-Type: %s\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", part.ctype)
		qp := quotedprintable.NewWriter(&b)
		if _, err := qp.Write([]byte(strings.ReplaceAll(part.body, "\n", "\r\n"))); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes(), nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ---- Credentials email ---------------------------------------------------------

// CredentialsEmail holds what the welcome / reset email shows.
type CredentialsEmail struct {
	FullName     string
	Email        string
	TempPassword string
	RoleLabel    string
	LoginURL     string
	Checkpoints  []string // scanner accounts only
	IsReset      bool     // "resend credentials" rather than a new account
	EventName    string
}

var credentialsHTML = template.Must(template.New("credentials").Parse(`<!DOCTYPE html>
<html lang="en"><body style="margin:0;padding:0;background:#EEF1F5;font-family:Arial,Helvetica,sans-serif;color:#13233D;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#EEF1F5;padding:24px 12px;">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:520px;background:#FFFFFF;border-radius:14px;overflow:hidden;">
  <tr><td style="background:#0D173E;padding:22px 26px;color:#FFFFFF;">
    <div style="font-size:12px;letter-spacing:2px;color:#E8392B;font-weight:bold;">NASA</div>
    <div style="font-size:22px;font-weight:bold;letter-spacing:.5px;">SPACE APPS COLOMBO</div>
    <div style="font-size:14px;opacity:.85;margin-top:4px;">{{if .IsReset}}Your sign-in details were reset{{else}}Your staff account is ready{{end}}</div>
  </td></tr>
  <tr><td style="padding:24px 26px 8px;font-size:15px;line-height:1.55;">
    <p style="margin:0 0 14px;">Hi {{.FullName}},</p>
    {{if .IsReset}}
    <p style="margin:0 0 14px;">An organizer issued you a new temporary password for {{.EventName}}. Your previous password no longer works.</p>
    {{else}}
    <p style="margin:0 0 14px;">You've been added to the {{.EventName}} team as <b>{{.RoleLabel}}</b>.</p>
    {{end}}
    {{if .Checkpoints}}<p style="margin:0 0 14px;">Your checkpoints: <b>{{range $i, $c := .Checkpoints}}{{if $i}}, {{end}}{{$c}}{{end}}</b></p>{{end}}
  </td></tr>
  <tr><td style="padding:0 26px;">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#F4F6FA;border:1px solid #C6CFDB;border-radius:10px;">
      <tr><td style="padding:14px 16px;font-size:13px;color:#4A5A73;">Sign-in email</td></tr>
      <tr><td style="padding:0 16px 12px;font-size:16px;font-weight:bold;">{{.Email}}</td></tr>
      <tr><td style="padding:0 16px;font-size:13px;color:#4A5A73;">Temporary password</td></tr>
      <tr><td style="padding:2px 16px 16px;font-size:22px;font-weight:bold;font-family:'Courier New',Courier,monospace;letter-spacing:2px;">{{.TempPassword}}</td></tr>
    </table>
  </td></tr>
  <tr><td style="padding:20px 26px 6px;" align="center">
    <a href="{{.LoginURL}}" style="display:inline-block;background:#1F4FD1;color:#FFFFFF;text-decoration:none;font-weight:bold;padding:13px 26px;border-radius:8px;font-size:15px;">Sign in</a>
    <div style="font-size:12px;color:#4A5A73;margin-top:8px;word-break:break-all;">{{.LoginURL}}</div>
  </td></tr>
  <tr><td style="padding:16px 26px 24px;font-size:13px;line-height:1.55;color:#4A5A73;">
    <p style="margin:0 0 10px;">When you first sign in you'll be asked to choose your own password. The temporary one stops working after that.</p>
    <p style="margin:0;">Didn't expect this email? Ignore it and tell the organizing team.</p>
  </td></tr>
</table>
</td></tr></table>
</body></html>`))

// RenderCredentialsEmail builds the subject and both bodies.
func RenderCredentialsEmail(c CredentialsEmail) (subject, text, html string, err error) {
	if c.IsReset {
		subject = fmt.Sprintf("Your new %s sign-in details", c.EventName)
	} else {
		subject = fmt.Sprintf("Your %s staff account", c.EventName)
	}

	var t strings.Builder
	fmt.Fprintf(&t, "Hi %s,\n\n", c.FullName)
	if c.IsReset {
		fmt.Fprintf(&t, "An organizer issued you a new temporary password for %s. Your previous password no longer works.\n\n", c.EventName)
	} else {
		fmt.Fprintf(&t, "You've been added to the %s team as %s.\n\n", c.EventName, c.RoleLabel)
	}
	if len(c.Checkpoints) > 0 {
		fmt.Fprintf(&t, "Your checkpoints: %s\n\n", strings.Join(c.Checkpoints, ", "))
	}
	fmt.Fprintf(&t, "Sign in: %s\nEmail: %s\nTemporary password: %s\n\n", c.LoginURL, c.Email, c.TempPassword)
	t.WriteString("When you first sign in you'll be asked to choose your own password. The temporary one stops working after that.\n\n")
	t.WriteString("Didn't expect this email? Ignore it and tell the organizing team.\n")

	var h bytes.Buffer
	if err := credentialsHTML.Execute(&h, c); err != nil {
		return "", "", "", fmt.Errorf("render email: %w", err)
	}
	return subject, t.String(), h.String(), nil
}

// ValidDisplayName rejects control characters (including CR/LF) in names that
// end up in email headers and HTML.
func ValidDisplayName(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
