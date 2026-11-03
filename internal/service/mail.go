package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime/multipart"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

type Message struct {
	To          mail.Address
	Subject     string
	Text        string
	HTML        string
	Inline      []InlineImage
	Attachments []Attachment
	ReplyTo     *mail.Address
}

type InlineImage struct {
	ContentID   string
	ContentType string
	Filename    string
	Data        []byte
}

type Attachment struct {
	ContentType string
	Filename    string
	Data        []byte
}

type Mailer interface {
	Send(context.Context, Message) error
}

const (
	SMTPStartTLS = "starttls"
	SMTPTLS      = "tls"
	SMTPNone     = "none"
)

type SMTPConfig struct {
	Host, Port, Username, Password string
	From                           mail.Address
	Security                       string
}

type SMTPMailer struct{ cfg SMTPConfig }

func NewSMTPMailer(cfg SMTPConfig) (*SMTPMailer, error) {
	if cfg.From.Address == "" {
		return nil, errors.New("SMTP_FROM is required when SMTP_HOST is set")
	}
	if cfg.Security != SMTPStartTLS && cfg.Security != SMTPTLS && cfg.Security != SMTPNone {
		return nil, fmt.Errorf("unsupported SMTP_SECURITY %q", cfg.Security)
	}
	if cfg.Security == SMTPNone && cfg.Username != "" {
		return nil, errors.New("SMTP_SECURITY=none cannot be combined with SMTP_USERNAME")
	}
	return &SMTPMailer{cfg: cfg}, nil
}

func (s *SMTPMailer) Probe(ctx context.Context) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Quit()
}

func (s *SMTPMailer) Send(ctx context.Context, m Message) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Mail(s.cfg.From.Address); err != nil {
		return err
	}
	if err := c.Rcpt(m.To.Address); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	body, err := buildMIME(s.cfg.From, m)
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}

	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func buildMIME(from mail.Address, m Message) ([]byte, error) {
	var body bytes.Buffer
	fmt.Fprintf(&body, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\n", from.String(), m.To.String(), m.Subject)
	if m.ReplyTo != nil {
		fmt.Fprintf(&body, "Reply-To: %s\r\n", m.ReplyTo.String())
	}

	var alternative bytes.Buffer
	alt := multipart.NewWriter(&alternative)
	if err := writeTextParts(alt, m); err != nil {
		return nil, err
	}
	if err := alt.Close(); err != nil {
		return nil, err
	}

	content := alternative.Bytes()
	contentType := "multipart/alternative; boundary=" + fmt.Sprintf("%q", alt.Boundary())
	if len(m.Inline) > 0 {
		var related bytes.Buffer
		relatedWriter := multipart.NewWriter(&related)
		part, err := relatedWriter.CreatePart(textproto.MIMEHeader{
			"Content-Type": []string{contentType},
		})
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(content); err != nil {
			return nil, err
		}
		for _, image := range m.Inline {
			if err := writeBinaryPart(relatedWriter, image.ContentType, image.Filename, image.ContentID, image.Data); err != nil {
				return nil, err
			}
		}
		if err := relatedWriter.Close(); err != nil {
			return nil, err
		}
		content = related.Bytes()
		contentType = "multipart/related; boundary=" + fmt.Sprintf("%q", relatedWriter.Boundary())
	}
	if len(m.Attachments) == 0 {
		fmt.Fprintf(&body, "Content-Type: %s\r\n\r\n", contentType)
		body.Write(content)
		return body.Bytes(), nil
	}

	mixed := multipart.NewWriter(&body)
	body.WriteString(fmt.Sprintf("Content-Type: multipart/mixed; boundary=%q\r\n\r\n", mixed.Boundary()))
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", contentType)
	part, err := mixed.CreatePart(h)
	if err != nil {
		return nil, err
	}
	_, _ = part.Write(content)
	for _, attachment := range m.Attachments {
		if err := writeBinaryPart(mixed, attachment.ContentType, attachment.Filename, "", attachment.Data); err != nil {
			return nil, err
		}
	}
	if err := mixed.Close(); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}

func writeTextParts(w *multipart.Writer, m Message) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	part, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	_, _ = part.Write([]byte(m.Text))
	if m.HTML == "" {
		return nil
	}
	h = textproto.MIMEHeader{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	part, err = w.CreatePart(h)
	if err != nil {
		return err
	}
	_, _ = part.Write([]byte(m.HTML))
	return nil
}

func writeBinaryPart(w *multipart.Writer, contentType, filename, contentID string, data []byte) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", contentType)
	h.Set("Content-Transfer-Encoding", "base64")
	if filename != "" {
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, strings.ReplaceAll(filename, `"`, "")))
	}
	if contentID != "" {
		h.Set("Content-ID", "<"+contentID+">")
		h.Set("Content-Disposition", "inline")
	}
	part, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	encoder := base64.NewEncoder(base64.StdEncoding, part)
	if _, err := encoder.Write(data); err != nil {
		return err
	}
	return encoder.Close()
}

func (s *SMTPMailer) dial(ctx context.Context) (*smtp.Client, error) {
	addr := net.JoinHostPort(s.cfg.Host, s.cfg.Port)
	deadline := time.Now().Add(20 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialer := &net.Dialer{Deadline: deadline}
	tlsConfig := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	var err error
	if s.cfg.Security == SMTPTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if s.cfg.Security == SMTPStartTLS {
		if err := c.StartTLS(tlsConfig); err != nil {
			c.Close()
			return nil, err
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			c.Close()
			return nil, err
		}
	}
	return c, nil
}

type disabledMailer struct{}

func (disabledMailer) Send(context.Context, Message) error {
	return fmt.Errorf("%w: email is not configured on the server", domain.ErrEmailUnavailable)
}

func DisabledMailer() Mailer { return disabledMailer{} }
