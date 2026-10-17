package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// EmailQueueStore is implemented by postgres.EmailRepo.
type EmailQueueStore interface {
	FetchNextJob(ctx context.Context) (*domain.EmailJob, error)
	MarkSent(ctx context.Context, id string) error
	MarkFailed(ctx context.Context, id, errMsg string, permanent bool, retryIn time.Duration) (string, error)
	Release(ctx context.Context, id, errMsg string, delay time.Duration) error
	Cancel(ctx context.Context, id, reason string) error
	ReclaimStale(ctx context.Context, olderThan time.Duration) (int, error)
	SentSince(ctx context.Context, t time.Time) (int, error)
	State(ctx context.Context) (domain.EmailQueueState, error)
	SetPaused(ctx context.Context, paused bool, reason, actorID string) (domain.EmailQueueState, error)
	EnqueueSingle(ctx context.Context, participantID, membershipID, actorID string) (*domain.EmailEnqueueResult, error)
	EnqueueBulk(ctx context.Context, actorID string) (int, error)
	Stats(ctx context.Context, since time.Time) (*domain.EmailQueueStats, error)
}

// EmailWorkerConfig tunes the sender.
type EmailWorkerConfig struct {
	// One message per Interval: 2 s is 30 a minute, 300 passes in about 10
	// minutes, slow enough that Gmail doesn't throttle a burst.
	Interval time.Duration
	// Messages per rolling 24 hours, 0 for no cap. Gmail allows about 500
	// (2,000 on Workspace); staff credential emails share that allowance.
	DailyLimit int
	// Wait before retrying a transient failure.
	RetryDelay time.Duration
	// After this many failures in a row (of any kind) the queue pauses
	// itself: something is wrong for everyone, not one address.
	MaxConsecutiveFailures int

	BaseURL   string // for pass links
	EventName string
}

// DefaultEmailWorkerConfig fills in the zero values.
func (c EmailWorkerConfig) withDefaults() EmailWorkerConfig {
	if c.Interval <= 0 {
		c.Interval = 2 * time.Second
	}
	if c.RetryDelay <= 0 {
		c.RetryDelay = time.Minute
	}
	if c.MaxConsecutiveFailures <= 0 {
		c.MaxConsecutiveFailures = 5
	}
	return c
}

// sendTimeout bounds one message, from connecting to the server's reply.
const sendTimeout = 45 * time.Second

// EmailWorker sends queued pass emails one at a time on a ticker.
type EmailWorker struct {
	store  EmailQueueStore
	mailer Mailer
	cfg    EmailWorkerConfig
	now    func() time.Time

	mu              sync.Mutex
	running         bool
	waitingForLimit bool
	consecutive     int // failures in a row
}

func NewEmailWorker(store EmailQueueStore, mailer Mailer, cfg EmailWorkerConfig) *EmailWorker {
	return &EmailWorker{store: store, mailer: mailer, cfg: cfg.withDefaults(), now: time.Now}
}

// Run sends until ctx is cancelled. A message being sent when ctx ends is
// finished and recorded first, so shutdown never leaves one sent but marked
// pending (which would send it twice).
func (w *EmailWorker) Run(ctx context.Context) {
	w.setRunning(true)
	defer w.setRunning(false)

	w.reclaim(ctx)
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	reclaim := time.NewTicker(time.Minute)
	defer reclaim.Stop()

	slog.Info("email worker started", "interval", w.cfg.Interval, "daily_limit", w.cfg.DailyLimit)
	for {
		select {
		case <-ctx.Done():
			slog.Info("email worker stopped")
			return
		case <-reclaim.C:
			w.reclaim(ctx)
		case <-ticker.C:
			w.Tick(ctx)
		}
	}
}

func (w *EmailWorker) setRunning(v bool) {
	w.mu.Lock()
	w.running = v
	w.mu.Unlock()
}

// Running and WaitingForLimit report state for the dashboard.
func (w *EmailWorker) Running() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

func (w *EmailWorker) WaitingForLimit() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.waitingForLimit
}

func (w *EmailWorker) reclaim(ctx context.Context) {
	n, err := w.store.ReclaimStale(ctx, domain.EmailStaleClaimAfter)
	if err != nil {
		slog.Error("reclaim email jobs", "err", err)
	} else if n > 0 {
		slog.Warn("email jobs reclaimed after an interrupted send", "count", n)
	}
}

// Tick sends at most one message. It returns true when one was sent.
// Exported so tests can drive the worker without a ticker.
func (w *EmailWorker) Tick(ctx context.Context) bool {
	state, err := w.store.State(ctx)
	if err != nil {
		slog.Error("email queue state", "err", err)
		return false
	}
	if state.Paused {
		return false
	}

	if w.cfg.DailyLimit > 0 {
		n, err := w.store.SentSince(ctx, w.now().Add(-24*time.Hour))
		if err != nil {
			slog.Error("count sent emails", "err", err)
			return false
		}
		w.mu.Lock()
		w.waitingForLimit = n >= w.cfg.DailyLimit
		w.mu.Unlock()
		if n >= w.cfg.DailyLimit {
			return false // resumes on its own as the 24-hour window moves
		}
	}

	job, err := w.store.FetchNextJob(ctx)
	if err != nil {
		slog.Error("fetch email job", "err", err)
		return false
	}
	if job == nil {
		return false
	}

	// From here on the job is ours: finish and record it even if ctx ends.
	done, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout+15*time.Second)
	defer cancel()

	if !job.IsConfirmed {
		if err := w.store.Cancel(done, job.ID, "delegate is no longer confirmed"); err != nil {
			slog.Error("cancel email job", "job", job.ID, "err", err)
		}
		return false
	}
	msg, err := RenderPassEmail(job, w.cfg.BaseURL, w.cfg.EventName)
	if err != nil {
		w.recordFailure(done, job, err.Error(), true)
		return false
	}

	sendCtx, cancelSend := context.WithTimeout(done, sendTimeout)
	err = w.mailer.Send(sendCtx, msg)
	cancelSend()
	if err == nil {
		if err := w.store.MarkSent(done, job.ID); err != nil {
			slog.Error("mark email sent", "job", job.ID, "err", err)
		}
		w.mu.Lock()
		w.consecutive = 0
		w.mu.Unlock()
		slog.Info("pass email sent", "job", job.ID, "membership_id", job.MembershipID, "attempt", job.Attempts)
		return true
	}

	kind, reason := classifySendError(err)
	switch kind {
	case failSender:
		// Not this delegate's fault: put the job back untouched and stop.
		if rerr := w.store.Release(done, job.ID, err.Error(), w.cfg.RetryDelay); rerr != nil {
			slog.Error("release email job", "job", job.ID, "err", rerr)
		}
		w.pause(done, reason)
		slog.Error("email queue paused", "reason", reason, "err", err)
	case failRecipient:
		w.recordFailure(done, job, err.Error(), true)
	default:
		w.recordFailure(done, job, err.Error(), false)
	}
	return false
}

// recordFailure stores a failed attempt and trips the breaker after too many
// failures in a row.
func (w *EmailWorker) recordFailure(ctx context.Context, job *domain.EmailJob, msg string, permanent bool) {
	status, err := w.store.MarkFailed(ctx, job.ID, msg, permanent, w.cfg.RetryDelay)
	if err != nil {
		slog.Error("mark email failed", "job", job.ID, "err", err)
	}
	slog.Warn("pass email failed", "job", job.ID, "membership_id", job.MembershipID,
		"attempt", job.Attempts, "permanent", permanent, "now", status, "err", msg)

	w.mu.Lock()
	w.consecutive++
	trip := w.consecutive >= w.cfg.MaxConsecutiveFailures
	if trip {
		w.consecutive = 0
	}
	w.mu.Unlock()
	if trip {
		w.pause(ctx, fmt.Sprintf("%d emails in a row failed, so sending stopped. Last error: %s",
			w.cfg.MaxConsecutiveFailures, firstLine(msg)))
	}
}

func (w *EmailWorker) pause(ctx context.Context, reason string) {
	if _, err := w.store.SetPaused(ctx, true, reason, ""); err != nil {
		slog.Error("pause email queue", "err", err)
	}
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// ---- Queue service (dashboard) ------------------------------------------------------

// EmailQueueService is what the dashboard endpoints use. worker is nil when
// SMTP isn't configured: the status still reports, queueing refuses.
type EmailQueueService struct {
	store  EmailQueueStore
	worker *EmailWorker
	cfg    EmailWorkerConfig
	now    func() time.Time
}

func NewEmailQueueService(store EmailQueueStore, worker *EmailWorker, cfg EmailWorkerConfig) *EmailQueueService {
	return &EmailQueueService{store: store, worker: worker, cfg: cfg.withDefaults(), now: time.Now}
}

func (s *EmailQueueService) requireEnabled() error {
	if s.worker == nil {
		return fmt.Errorf("%w: email is not configured on the server (set SMTP_HOST)", domain.ErrEmailUnavailable)
	}
	return nil
}

// SendSingle queues one delegate's pass email (also a resend).
func (s *EmailQueueService) SendSingle(ctx context.Context, actorID string, req domain.EmailSingleRequest) (*domain.EmailEnqueueResult, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, err
	}
	pid := strings.ToLower(strings.TrimSpace(req.ParticipantID))
	mid := strings.TrimSpace(req.MembershipID)
	switch {
	case pid != "" && !IsUUID(pid):
		return nil, fmt.Errorf("%w: participant_id is not a UUID", domain.ErrValidation)
	case pid == "" && mid == "":
		return nil, fmt.Errorf("%w: give participant_id or membership_id", domain.ErrValidation)
	case len(mid) > 100:
		return nil, fmt.Errorf("%w: membership_id is too long", domain.ErrValidation)
	}
	res, err := s.store.EnqueueSingle(ctx, pid, mid, actorID)
	if err != nil {
		return nil, err
	}
	slog.Info("pass email queued", "actor", actorID, "participant", pid, "membership_id", mid)
	return res, nil
}

// SendBulk queues every confirmed delegate who hasn't had a pass email.
func (s *EmailQueueService) SendBulk(ctx context.Context, actorID string) (*domain.EmailEnqueueResult, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, err
	}
	n, err := s.store.EnqueueBulk(ctx, actorID)
	if err != nil {
		return nil, err
	}
	slog.Info("pass emails queued in bulk", "actor", actorID, "count", n)
	msg := "Every confirmed delegate already has a pass email sent or queued."
	if n > 0 {
		minutes := (time.Duration(n) * s.cfg.Interval).Round(time.Minute)
		msg = fmt.Sprintf("%d pass email%s queued. At one every %s that takes about %s.",
			n, plural(n), s.cfg.Interval, humanMinutes(minutes))
	}
	return &domain.EmailEnqueueResult{Queued: n, Message: msg}, nil
}

// SetPaused pauses or resumes sending. Resuming clears a reason the worker
// left (the organizer has dealt with it).
func (s *EmailQueueService) SetPaused(ctx context.Context, actorID string, paused bool) (domain.EmailQueueState, error) {
	st, err := s.store.SetPaused(ctx, paused, "", actorID)
	if err != nil {
		return st, err
	}
	slog.Info("email queue switched", "actor", actorID, "paused", paused)
	return st, nil
}

// Status gathers the counts and the worker's view for the dashboard.
func (s *EmailQueueService) Status(ctx context.Context) (*domain.EmailQueueStats, error) {
	now := s.now()
	st, err := s.store.Stats(ctx, now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	state, err := s.store.State(ctx)
	if err != nil {
		return nil, err
	}
	st.EmailQueueState = state
	st.DailyLimit = s.cfg.DailyLimit
	st.SendInterval = s.cfg.Interval.Seconds()
	st.Enabled = s.worker != nil
	if s.worker != nil {
		st.WorkerRunning = s.worker.Running()
		st.WaitingForLimit = s.worker.WaitingForLimit()
	}
	// An estimate only when the rest fits in today's allowance; otherwise
	// the finish depends on when old messages age out of the 24-hour window.
	left := st.Pending + st.Sending
	fitsToday := s.cfg.DailyLimit == 0 || left <= s.cfg.DailyLimit-st.SentLast24h
	if left > 0 && !state.Paused && st.Enabled && !st.WaitingForLimit && fitsToday {
		eta := now.Add(time.Duration(left) * s.cfg.Interval)
		st.EstimatedFinish = &eta
	}
	return st, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func humanMinutes(d time.Duration) string {
	m := int(d.Minutes())
	switch {
	case m < 1:
		return "under a minute"
	case m < 60:
		return fmt.Sprintf("%d minute%s", m, plural(m))
	default:
		return fmt.Sprintf("%d h %02d min", m/60, m%60)
	}
}
