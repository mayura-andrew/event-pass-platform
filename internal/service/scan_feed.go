package service

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type ScanFeedStore interface {
	ScansSince(ctx context.Context, since time.Time, limit int) ([]domain.ScanEvent, error)
}

const (
	feedPollInterval = 2 * time.Second
	// synced_at is the inserting transaction's start time, and a long sync
	// batch can commit after shorter ones that started later. Each poll
	// re-reads this far behind the newest scan already seen and drops IDs it
	// already sent, so late commits still appear.
	feedOverlap = 2 * time.Minute
	// Rows read per poll. A phone uploading a day of offline scans can exceed
	// this; the feed then shows the newest ones and skips the rest of the
	// burst. The feed is a display, so a gap there loses no data.
	feedBatchLimit = 500
	// Batches buffered per dashboard. A client this far behind is dropped
	// and reconnects with a fresh backlog instead of slowing everyone down.
	feedSubscriberBuffer = 32
)

// ScanFeed fans newly recorded scans out to every connected dashboard.
//
// One poller queries the database for all subscribers, so the database load
// doesn't grow with the number of open dashboards. Polling (rather than
// LISTEN/NOTIFY) works through every Supabase connection mode, including the
// transaction pooler, which drops LISTEN registrations between statements.
type ScanFeed struct {
	store ScanFeedStore

	mu     sync.Mutex
	subs   map[chan []domain.ScanEvent]struct{}
	closed bool

	// Poller state, only touched by Run's goroutine.
	watermark time.Time            // newest synced_at sent so far
	seen      map[string]time.Time // scan ID -> synced_at, within the overlap window
}

func NewScanFeed(store ScanFeedStore) *ScanFeed {
	return &ScanFeed{
		store: store,
		subs:  make(map[chan []domain.ScanEvent]struct{}),
		seen:  make(map[string]time.Time),
	}
}

// Subscribe registers a dashboard. The channel receives batches of new scans,
// oldest first, and is closed when the client falls too far behind or the
// feed shuts down. Call cancel when the client disconnects.
func (f *ScanFeed) Subscribe() (<-chan []domain.ScanEvent, func()) {
	ch := make(chan []domain.ScanEvent, feedSubscriberBuffer)
	f.mu.Lock()
	if f.closed {
		close(ch)
	} else {
		f.subs[ch] = struct{}{}
	}
	f.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			f.mu.Lock()
			if _, ok := f.subs[ch]; ok {
				delete(f.subs, ch)
				close(ch)
			}
			f.mu.Unlock()
		})
	}
}

// Run polls until ctx ends, then closes every subscriber so streaming
// handlers return and the HTTP server can shut down.
func (f *ScanFeed) Run(ctx context.Context) {
	t := time.NewTicker(feedPollInterval)
	defer t.Stop()
	defer f.closeAll()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.poll(ctx)
		}
	}
}

func (f *ScanFeed) poll(ctx context.Context) {
	f.mu.Lock()
	n := len(f.subs)
	f.mu.Unlock()
	if n == 0 {
		// Nobody watching: skip the query and forget state, so the next
		// subscriber starts from "now" instead of replaying a backlog.
		f.watermark = time.Time{}
		clear(f.seen)
		return
	}
	if f.watermark.IsZero() {
		// New subscribers already loaded a backlog via RecentScans; the
		// overlap below re-sends the last two minutes and clients de-duplicate.
		f.watermark = time.Now()
	}

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := f.store.ScansSince(qctx, f.watermark.Add(-feedOverlap), feedBatchLimit)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("scan feed poll failed", "err", err)
		}
		return
	}

	fresh := make([]domain.ScanEvent, 0, len(rows))
	for _, e := range rows {
		if _, dup := f.seen[e.ID]; dup {
			continue
		}
		f.seen[e.ID] = e.SyncedAt
		if e.SyncedAt.After(f.watermark) {
			f.watermark = e.SyncedAt
		}
		fresh = append(fresh, e)
	}

	// Forget IDs that have left the overlap window; they can't be re-read.
	cutoff := f.watermark.Add(-feedOverlap)
	for id, at := range f.seen {
		if at.Before(cutoff) {
			delete(f.seen, id)
		}
	}

	if len(fresh) == 0 {
		return
	}
	slices.Reverse(fresh) // query is newest first; deliver oldest first
	f.broadcast(fresh)
}

func (f *ScanFeed) broadcast(batch []domain.ScanEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		select {
		case ch <- batch:
		default:
			// Slow client: drop it rather than block the poller.
			delete(f.subs, ch)
			close(ch)
		}
	}
}

func (f *ScanFeed) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	for ch := range f.subs {
		delete(f.subs, ch)
		close(ch)
	}
}
