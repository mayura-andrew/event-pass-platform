package config

import (
	"strings"
	"testing"
)

func setBase(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("JWT_SECRET", strings.Repeat("s", 32))
	for _, k := range []string{"SMTP_HOST", "GOOGLE_OAUTH_CLIENT_ID", "GOOGLE_OAUTH_CLIENT_SECRET", "PASS_SIGNING_KEYS",
		"INTEGRATIONS_KEY", "CLIENT_IP_HEADER", "APP_BASE_URL", "RUN_WORKERS", "SESSION_IDLE_TIMEOUT", "BIRTH_DATE_RETENTION"} {
		t.Setenv(k, "")
	}
}

func TestProductionFailsClosed(t *testing.T) {
	setBase(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("APP_BASE_URL", "http://pass.example.org")
	_, err := Load()
	if err == nil {
		t.Fatal("production started without keys, HTTPS or a client IP header")
	}
	for _, want := range []string{"PASS_SIGNING_KEYS", "INTEGRATIONS_KEY", "APP_BASE_URL", "CLIENT_IP_HEADER"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error doesn't mention %s: %v", want, err)
		}
	}

	t.Setenv("APP_BASE_URL", "https://pass.example.org")
	t.Setenv("PASS_SIGNING_KEYS", "1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("INTEGRATIONS_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("CLIENT_IP_HEADER", "X-Forwarded-For")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "X-Forwarded-For") {
		t.Fatalf("X-Forwarded-For accepted as the client IP header: %v", err)
	}
	t.Setenv("CLIENT_IP_HEADER", "X-Client-IP")
	c, err := Load()
	if err != nil || !c.Production() {
		t.Fatalf("complete production config refused: %v", err)
	}
}

func TestDevelopmentKeepsFallbacks(t *testing.T) {
	setBase(t)
	t.Setenv("APP_ENV", "")
	c, err := Load()
	if err != nil || c.Production() || !c.RunWorkers || c.SessionIdle == 0 {
		t.Fatalf("development config: %+v %v", c, err)
	}
}
