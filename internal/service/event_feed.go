package service

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// ActivityStore finds events with recent activity (postgres.CheckinRepo).
type ActivityStore interface {
	ActiveEvents(ctx context.Context, since time.Time) ([]string, error)
}

// EventFeed tells live dashboards and scanners that their event changed, so
// they fetch fresh numbers. One poller serves every subscriber, like the
// current build's ScanFeed, and for the same reason: polling works through
// every Supabase connection mode, where LISTEN/NOTIFY does not
// (docs/decisions/0008-checkin-and-live-feed.md).
type EventFeed struct {
	store    ActivityStore
	Interval time.Duration

	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{} // event ID -> subscribers
}

func NewEventFeed(store ActivityStore) *EventFeed {
	return &EventFeed{store: store, Interval: 2 * time.Second, subs: map[string]map[chan struct{}]struct{}{}}
}

// Subscribe returns a channel that receives a signal (coalesced: at most one
// pending) whenever the event changes, and a function to stop.
func (f *EventFeed) Subscribe(eventID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	if f.subs[eventID] == nil {
		f.subs[eventID] = map[chan struct{}]struct{}{}
	}
	f.subs[eventID][ch] = struct{}{}
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		delete(f.subs[eventID], ch)
		if len(f.subs[eventID]) == 0 {
			delete(f.subs, eventID)
		}
		f.mu.Unlock()
	}
}

// Notify signals an event's subscribers now, without waiting for the poll
// (used after changes made by this server).
func (f *EventFeed) Notify(eventID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs[eventID] {
		select {
		case ch <- struct{}{}:
		default: // a signal is already pending
		}
	}
}

func (f *EventFeed) watching() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs) > 0
}

// Run polls until ctx ends. Each poll looks back a little further than the
// last one, so changes committed late by a slow transaction still count.
func (f *EventFeed) Run(ctx context.Context) {
	t := time.NewTicker(f.Interval)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !f.watching() {
			last = time.Now()
			continue
		}
		now := time.Now()
		ids, err := f.store.ActiveEvents(ctx, last.Add(-3*time.Second))
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("event feed poll", "err", err)
			}
			continue
		}
		last = now
		for _, id := range ids {
			f.Notify(id)
		}
	}
}
