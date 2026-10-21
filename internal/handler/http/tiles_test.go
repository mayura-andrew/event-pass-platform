package httphandler

import (
	"strings"
	"testing"
)

func TestTileOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://tile.openstreetmap.org/{z}/{x}/{y}.png":                   "https://tile.openstreetmap.org",
		"https://api.maptiler.com/maps/streets-v2/{z}/{x}/{y}.png?key=abc": "https://api.maptiler.com",
		"https://{s}.basemaps.cartocdn.com/light_all/{z}/{x}/{y}{r}.png":   "https://*.basemaps.cartocdn.com",
		"http://insecure.example/{z}/{x}/{y}.png":                          defaultTileOrigin, // not allowed: falls back
		"": defaultTileOrigin,
	} {
		if got := tileOrigin(in); got != want {
			t.Errorf("tileOrigin(%q) = %q, want %q", in, got, want)
		}
	}
	if csp := mapCSP("https://api.maptiler.com/maps/x/{z}/{x}/{y}.png"); !strings.Contains(csp, "img-src 'self' data: https://api.maptiler.com") ||
		!strings.Contains(csp, "script-src 'self';") {
		t.Errorf("map CSP: %s", csp)
	}
}
