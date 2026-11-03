package service

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/o/acme":                      "/o/acme",
		"/invite?token=abc":            "/invite?token=abc",
		"":                             "",
		"o/acme":                       "",
		"//evil.example/x":             "",
		"/\\evil.example":              "",
		"https://evil.example/":        "",
		"/a\r\nSet-Cookie: x":          "",
		"/" + strings.Repeat("a", 500): "",
	} {
		if got := SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	if got, err := NormalizeEmail("  Ann@Example.ORG "); err != nil || got != "ann@example.org" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "ann", "Ann <ann@example.org>", "a@b.c\r\nBcc: x@y.z"} {
		if _, err := NormalizeEmail(bad); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("NormalizeEmail(%q) = %v, want ErrValidation", bad, err)
		}
	}
}

func idToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(claims)
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func TestCheckIDToken(t *testing.T) {
	g := NewGoogleOAuth("client-1", "secret", "https://app.example/auth/google/callback")
	now := time.Unix(1_800_000_000, 0)
	good := func() map[string]any {
		return map[string]any{
			"iss": "https://accounts.google.com", "aud": "client-1", "exp": now.Unix() + 60,
			"sub": "123", "email": "Ann@Example.org", "email_verified": true, "name": "Ann", "nonce": "n1",
		}
	}
	id, err := g.checkIDToken(idToken(t, good()), "n1", now)
	if err != nil || id.Subject != "123" || id.Email != "ann@example.org" || id.Name != "Ann" {
		t.Fatalf("valid token: %+v, %v", id, err)
	}
	c := good()
	c["email_verified"] = "true" // some Google responses use a string
	if _, err := g.checkIDToken(idToken(t, c), "n1", now); err != nil {
		t.Fatalf("string email_verified: %v", err)
	}

	for name, mutate := range map[string]func(map[string]any){
		"issuer":     func(c map[string]any) { c["iss"] = "https://evil.example" },
		"audience":   func(c map[string]any) { c["aud"] = "client-2" },
		"expired":    func(c map[string]any) { c["exp"] = now.Unix() - 1 },
		"nonce":      func(c map[string]any) { c["nonce"] = "other" },
		"no nonce":   func(c map[string]any) { delete(c, "nonce") },
		"unverified": func(c map[string]any) { c["email_verified"] = false },
		"no subject": func(c map[string]any) { c["sub"] = "" },
	} {
		c := good()
		mutate(c)
		if _, err := g.checkIDToken(idToken(t, c), "n1", now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := g.checkIDToken("not-a-jwt", "n1", now); err == nil {
		t.Error("malformed token accepted")
	}
}

func TestAuthCodeURLUsesPKCE(t *testing.T) {
	g := NewGoogleOAuth("client-1", "secret", "https://app.example/auth/google/callback")
	u := g.AuthCodeURL("st", "nn", "verifier")
	for _, want := range []string{"code_challenge_method=S256", "state=st", "nonce=nn", "client_id=client-1",
		"redirect_uri=https%3A%2F%2Fapp.example%2Fauth%2Fgoogle%2Fcallback"} {
		if !strings.Contains(u, want) {
			t.Errorf("auth URL %s lacks %s", u, want)
		}
	}
	if strings.Contains(u, "verifier") {
		t.Error("auth URL leaks the PKCE verifier")
	}
}
