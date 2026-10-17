package httphandler

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	cases := []struct {
		name, header, value, remote, want string
	}{
		{"no header configured uses socket", "", "", "10.244.6.1:5555", "10.244.6.1"},
		{"configured header wins", "DO-Connecting-IP", "203.0.113.7", "10.244.6.1:5555", "203.0.113.7"},
		{"first of a list", "DO-Connecting-IP", "203.0.113.7, 10.0.0.1", "10.244.6.1:5555", "203.0.113.7"},
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
