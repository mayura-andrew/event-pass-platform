package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// fakeQueue is an in-memory EmailQueueStore holding at most one due job.
type fakeQueue struct {
	state   domain.EmailQueueState
	sent24h int
	job     *domain.EmailJob
	fetched int
	sentIDs []string
	failed  []struct {
		permanent bool
		msg       string
	}
	released  []string
	cancelled []string
}

func (f *fakeQueue) FetchNextJob(context.Context) (*domain.EmailJob, error) {
	f.fetched++
	j := f.job
	return j, nil
}
func (f *fakeQueue) MarkSent(_ context.Context, id string) error {
	f.sentIDs = append(f.sentIDs, id)
	return nil
}
func (f *fakeQueue) MarkFailed(_ context.Context, id, msg string, permanent bool, _ time.Duration) (string, error) {
	f.failed = append(f.failed, struct {
		permanent bool
		msg       string
	}{permanent, msg})
	return "pending", nil
}
func (f *fakeQueue) Release(_ context.Context, id, _ string, _ time.Duration) error {
	f.released = append(f.released, id)
	return nil
}
func (f *fakeQueue) Cancel(_ context.Context, id, _ string) error {
	f.cancelled = append(f.cancelled, id)
	return nil
}
func (f *fakeQueue) ReclaimStale(context.Context, time.Duration) (int, error) { return 0, nil }
func (f *fakeQueue) SentSince(context.Context, time.Time) (int, error)        { return f.sent24h, nil }
func (f *fakeQueue) State(context.Context) (domain.EmailQueueState, error)    { return f.state, nil }
func (f *fakeQueue) SetPaused(_ context.Context, paused bool, reason, _ string) (domain.EmailQueueState, error) {
	f.state = domain.EmailQueueState{Paused: paused, PauseReason: reason}
	return f.state, nil
}
func (f *fakeQueue) EnqueueSingle(context.Context, string, string, string) (*domain.EmailEnqueueResult, error) {
	return &domain.EmailEnqueueResult{Queued: 1}, nil
}
func (f *fakeQueue) EnqueueBulk(context.Context, string) (int, error) { return 300, nil }
func (f *fakeQueue) Stats(context.Context, time.Time) (*domain.EmailQueueStats, error) {
	return &domain.EmailQueueStats{Pending: 30}, nil
}

type fakeMailer struct {
	err  error
	got  []Message
	ctxs []error // ctx.Err() seen by each Send
}

func (m *fakeMailer) Send(ctx context.Context, msg Message) error {
	m.got = append(m.got, msg)
	m.ctxs = append(m.ctxs, ctx.Err())
	return m.err
}

func testJob() *domain.EmailJob {
	return &domain.EmailJob{
		ID: "job-1", ParticipantID: "6f1c2a9e-0b7d-4c1e-9a55-1d2e3f4a5b6c", RecipientEmail: "ada@example.org",
		Attempts: 1, MaxAttempts: 3, FullName: "Ada <b>Reyes</b>", MembershipID: "NSAC-001000",
		Email: "ada@example.org", TeamName: "Orion", VenueName: "Innovation Center", HallSection: "Hall A",
		QRSignature: "abc123", IsConfirmed: true,
	}
}

func newTestWorker(q *fakeQueue, m *fakeMailer, limit int) *EmailWorker {
	return NewEmailWorker(q, m, EmailWorkerConfig{DailyLimit: limit, BaseURL: "https://pass.example.org", EventName: "Space Apps Colombo"})
}

func TestEmailWorkerSends(t *testing.T) {
	q := &fakeQueue{job: testJob()}
	m := &fakeMailer{}
	if !newTestWorker(q, m, 450).Tick(context.Background()) {
		t.Fatal("Tick reported nothing sent")
	}
	if len(q.sentIDs) != 1 || len(m.got) != 1 {
		t.Fatalf("sent %v, mailer got %d", q.sentIDs, len(m.got))
	}
	msg := m.got[0]
	if msg.To.Address != "ada@example.org" || !strings.Contains(msg.Subject, "NSAC-001000") {
		t.Errorf("to %v subject %q", msg.To, msg.Subject)
	}
	if len(msg.Inline) != 1 || msg.Inline[0].ContentType != "image/png" {
		t.Fatalf("inline = %+v", msg.Inline)
	}
	if !strings.Contains(msg.HTML, `src="cid:`+msg.Inline[0].ContentID+`"`) {
		t.Error("HTML doesn't reference the inline QR")
	}
	if strings.Contains(msg.HTML, "<b>Reyes</b>") || !strings.Contains(msg.HTML, "Ada &lt;b&gt;Reyes&lt;/b&gt;") {
		t.Error("the name must be HTML-escaped")
	}
	if want := "https://pass.example.org/pass.html?email=ada%40example.org&id=NSAC-001000"; !strings.Contains(msg.Text, want) {
		t.Errorf("text body lacks the pass link %s", want)
	}
}

func TestEmailWorkerSkips(t *testing.T) {
	t.Run("paused", func(t *testing.T) {
		q := &fakeQueue{job: testJob(), state: domain.EmailQueueState{Paused: true}}
		newTestWorker(q, &fakeMailer{}, 450).Tick(context.Background())
		if q.fetched != 0 {
			t.Fatal("a paused queue must not claim jobs")
		}
	})
	t.Run("daily limit", func(t *testing.T) {
		q := &fakeQueue{job: testJob(), sent24h: 450}
		w := newTestWorker(q, &fakeMailer{}, 450)
		w.Tick(context.Background())
		if q.fetched != 0 || !w.WaitingForLimit() {
			t.Fatalf("fetched %d, waiting %v", q.fetched, w.WaitingForLimit())
		}
		q.sent24h = 449
		w.Tick(context.Background())
		if q.fetched != 1 || w.WaitingForLimit() {
			t.Fatal("must resume once the window frees a slot")
		}
	})
	t.Run("unconfirmed", func(t *testing.T) {
		j := testJob()
		j.IsConfirmed = false
		q, m := &fakeQueue{job: j}, &fakeMailer{}
		newTestWorker(q, m, 0).Tick(context.Background())
		if len(m.got) != 0 || len(q.cancelled) != 1 {
			t.Fatalf("mailed %d, cancelled %v", len(m.got), q.cancelled)
		}
	})
}

func TestEmailWorkerFailures(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		permanent bool // MarkFailed(permanent)
		released  bool // Release + pause instead
	}{
		{"network", errors.New("connect to smtp.gmail.com:587: i/o timeout"), false, false},
		{"greylisted 451", &textproto.Error{Code: 451, Msg: "4.3.0 try again"}, false, false},
		{"gmail rate 421", &textproto.Error{Code: 421, Msg: "4.7.0 Try again later"}, false, false},
		{"no such user", wrapped("smtp RCPT TO", &textproto.Error{Code: 550, Msg: "5.1.1 The email account that you tried to reach does not exist"}), true, false},
		{"bad app password", wrapped("smtp auth", &textproto.Error{Code: 535, Msg: "5.7.8 Username and Password not accepted"}), false, true},
		{"daily cap", wrapped("smtp send", &textproto.Error{Code: 550, Msg: "5.4.5 Daily user sending limit exceeded"}), false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &fakeQueue{job: testJob()}
			newTestWorker(q, &fakeMailer{err: c.err}, 0).Tick(context.Background())
			if c.released {
				if len(q.released) != 1 || len(q.failed) != 0 || !q.state.Paused || q.state.PauseReason == "" {
					t.Fatalf("released %v failed %v state %+v; want released and paused with a reason", q.released, q.failed, q.state)
				}
				return
			}
			if len(q.failed) != 1 || q.failed[0].permanent != c.permanent || q.state.Paused {
				t.Fatalf("failed %+v paused %v; want permanent=%v, not paused", q.failed, q.state.Paused, c.permanent)
			}
		})
	}
}

func wrapped(prefix string, err error) error { return &wrapErr{prefix, err} }

type wrapErr struct {
	p string
	e error
}

func (w *wrapErr) Error() string { return w.p + ": " + w.e.Error() }
func (w *wrapErr) Unwrap() error { return w.e }

func TestEmailWorkerBreaker(t *testing.T) {
	q := &fakeQueue{job: testJob()}
	w := newTestWorker(q, &fakeMailer{err: errors.New("connection refused")}, 0)
	for i := 0; i < 4; i++ {
		w.Tick(context.Background())
	}
	if q.state.Paused {
		t.Fatal("paused too early")
	}
	w.Tick(context.Background())
	if !q.state.Paused || !strings.Contains(q.state.PauseReason, "5 emails in a row failed") {
		t.Fatalf("state = %+v, want paused by the breaker", q.state)
	}
}

// A shutdown signal arriving mid-send must not abort the send or the
// bookkeeping, or the message could go out and then be sent again.
func TestEmailWorkerFinishesJobOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q := &fakeQueue{job: testJob()}
	m := &fakeMailer{}
	w := newTestWorker(q, m, 0)
	cancelOnSend := &cancellingMailer{inner: m, cancel: cancel}
	w.mailer = cancelOnSend
	w.Tick(ctx)
	if len(m.ctxs) != 1 || m.ctxs[0] != nil {
		t.Fatalf("send context error = %v, want none", m.ctxs)
	}
	if len(q.sentIDs) != 1 {
		t.Fatal("the sent message wasn't recorded")
	}
}

type cancellingMailer struct {
	inner  *fakeMailer
	cancel context.CancelFunc
}

func (c *cancellingMailer) Send(ctx context.Context, m Message) error {
	c.cancel() // shutdown begins while the message is on its way
	return c.inner.Send(ctx, m)
}

func TestPassQR(t *testing.T) {
	if got := PassQRPayload("id-1", "sig-1"); got != `{"id":"id-1","sig":"sig-1"}` {
		t.Fatalf("payload %s; must match JSON.stringify({id, sig}) on the pass page", got)
	}
	b, err := PassQRPNG("6f1c2a9e-0b7d-4c1e-9a55-1d2e3f4a5b6c", strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil || img.Bounds().Dx() != 480 {
		t.Fatalf("png: %v, %v", err, img.Bounds())
	}
}

func TestBuildMIMEInline(t *testing.T) {
	msg, err := RenderPassEmail(testJob(), "https://pass.example.org", "Space Apps Colombo")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := buildMIME(mail.Address{Name: "Space Apps", Address: "no-reply@example.org"}, msg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, _ := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if mt != "multipart/related" {
		t.Fatalf("top-level type %s", mt)
	}
	mr := multipart.NewReader(parsed.Body, params["boundary"])
	alt, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if at, _, _ := mime.ParseMediaType(alt.Header.Get("Content-Type")); at != "multipart/alternative" {
		t.Fatalf("first part %s", at)
	}
	io.Copy(io.Discard, alt)
	img, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if img.Header.Get("Content-ID") != "<"+passQRContentID+">" || img.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("image headers %v", img.Header)
	}
	data, _ := io.ReadAll(img) // multipart decodes nothing: base64 text
	if _, err := png.Decode(base64Reader(data)); err != nil {
		t.Fatalf("embedded image isn't a PNG: %v", err)
	}

	// Staff emails (no inline images) keep the plain multipart/alternative shape.
	raw, _ = buildMIME(mail.Address{Address: "no-reply@example.org"}, Message{To: mail.Address{Address: "a@b.org"}, Subject: "x", Text: "t", HTML: "h"})
	parsed, _ = mail.ReadMessage(bytes.NewReader(raw))
	if mt, _, _ := mime.ParseMediaType(parsed.Header.Get("Content-Type")); mt != "multipart/alternative" {
		t.Fatalf("staff email became %s", mt)
	}
}

func base64Reader(b []byte) io.Reader {
	return base64.NewDecoder(base64.StdEncoding, bytes.NewReader(b))
}
