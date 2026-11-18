package httphandler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	cases := []struct {
		name, header, value, remote, want string
	}{
		{"no header configured uses socket", "", "", "10.244.6.1:5555", "10.244.6.1"},
		{"configured header wins", "DO-Connecting-IP", "203.0.113.7", "10.244.6.1:5555", "203.0.113.7"},
		{"a list is not trusted", "DO-Connecting-IP", "198.51.100.99, 203.0.113.7", "10.244.6.1:5555", "10.244.6.1"},
		{"not an IP is not trusted", "DO-Connecting-IP", "evil", "10.244.6.1:5555", "10.244.6.1"},
		{"IPv6 header", "DO-Connecting-IP", "2001:db8::7", "10.244.6.1:5555", "2001:db8::7"},
		{"header missing falls back", "DO-Connecting-IP", "", "10.244.6.1:5555", "10.244.6.1"},
		{"IPv6 socket", "", "", "[2001:db8::1]:443", "2001:db8::1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &Handler{ClientIPHeader: c.header}
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.remote
			if c.value != "" {
				r.Header.Set("DO-Connecting-IP", c.value)
			}
			// A spoofed X-Forwarded-For must never be trusted implicitly.
			r.Header.Set("X-Forwarded-For", "198.51.100.99")
			if got := h.clientIP(r); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

type fakeRateStore struct {
	n   map[string]int
	err error
}

func (f *fakeRateStore) Hit(_ context.Context, bucket, key string, _ time.Duration) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.n[bucket+"|"+key]++
	return f.n[bucket+"|"+key], nil
}

func TestRateLimiterSharedStore(t *testing.T) {
	store := &fakeRateStore{n: map[string]int{}}
	key := func(*http.Request) string { return "k" }
	// Two "replicas" with one store share the limit.
	a := newRateLimiter("login", 2, time.Minute, key, store)
	b := newRateLimiter("login", 2, time.Minute, key, store)
	ctx := context.Background()
	if !a.allow(ctx, "1.2.3.4") || !b.allow(ctx, "1.2.3.4") {
		t.Fatal("first two hits refused")
	}
	if a.allow(ctx, "1.2.3.4") || b.allow(ctx, "1.2.3.4") {
		t.Fatal("third hit across replicas allowed")
	}
	// A failing store falls back to the process's own window, not to "allow".
	store.err = errors.New("db down")
	c := newRateLimiter("login", 1, time.Minute, key, store)
	if !c.allow(ctx, "5.6.7.8") || c.allow(ctx, "5.6.7.8") {
		t.Fatal("fallback window did not limit")
	}
}

func TestLimitBodies(t *testing.T) {
	h := limitBodies(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}
	}))
	big := strings.NewReader("a=" + strings.Repeat("x", maxFormBody+10))
	r := httptest.NewRequest("POST", "/login", big)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized form: HTTP %d, want 413", w.Code)
	}
}
