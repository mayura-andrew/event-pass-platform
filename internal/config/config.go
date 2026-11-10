// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port             string // HTTP listen port
	DatabaseURL      string // Supabase Postgres connection string
	JWTSecret        []byte // signing key for session and development encryption
	PublicDir        string // static PWA / portal files
	GoogleMapsAPIKey string // browser key, restrict by HTTP referrer in GCP
	GoogleMapsMapID  string // optional; enables Advanced Markers
	AllowedOrigin    string // optional CORS origin when the frontend is hosted elsewhere
	DBMaxConns       int    // database connections this process may open

	ClientIPHeader  string // header the load balancer sets to the client IP
	LoginRatePerMin int    // sign-in attempts per client IP per minute
	PassRatePerMin  int    // public registration attempts per client IP per minute

	// Staff credential emails. SMTPHost empty disables staff provisioning.
	AppBaseURL   string // public URL used in emailed sign-in links
	EventName    string
	SMTPHost     string
	SMTPPort     string
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string // "Event Pass <no-reply@example.org>"
	SMTPSecurity string // starttls | tls | none

	// Pass email queue: one message per EmailSendInterval, at most
	// EmailDailyLimit per rolling 24 hours (0 = no cap).
	EmailSendInterval time.Duration
	EmailDailyLimit   int

	// SaaS sign-in (milestone M1). Google sign-in is off unless both are set;
	// the OAuth client's redirect URI is APP_BASE_URL + /auth/google/callback.
	GoogleClientID     string
	GoogleClientSecret string
	ProductName        string        // platform name in pages and emails
	SessionTTL         time.Duration // signed-in session lifetime

	// PlatformAdminEmails (PLATFORM_ADMIN_EMAILS, comma-separated) are the
	// only accounts that may create organizations. Everyone else signs in
	// for their passes only, or joins an org by invite. Empty: nobody can
	// create an organization from the app.
	PlatformAdminEmails []string

	// Ed25519 seeds for pass QR tokens, "1:<base64>,2:<base64>"; the highest
	// ID signs new passes, the others still verify. Empty: a key derived from
	// JWT_SECRET (local development only).
	PassSigningKeys string

	// AES-256 key (base64) that encrypts Google refresh tokens for Sheets
	// sync and Form import. Empty: derived from JWT_SECRET (development).
	IntegrationsKey string

	// Public status page (Better Stack, Instatus, ...), linked from help.
	StatusPageURL string

	// Map tiles for the participant map (beta): a {z}/{x}/{y} URL template
	// and its attribution (HTML). Default: OpenStreetMap's own servers,
	// which allow light use only; use a tile provider for real events.
	MapTileURL         string
	MapTileAttribution string
}

func Load() (*Config, error) {
	c := &Config{
		Port:             getenv("PORT", "8080"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		JWTSecret:        []byte(os.Getenv("JWT_SECRET")),
		PublicDir:        getenv("PUBLIC_DIR", "./public"),
		GoogleMapsAPIKey: os.Getenv("GOOGLE_MAPS_API_KEY"),
		GoogleMapsMapID:  os.Getenv("GOOGLE_MAPS_MAP_ID"),
		AllowedOrigin:    os.Getenv("ALLOWED_ORIGIN"),
		AppBaseURL:       strings.TrimRight(os.Getenv("APP_BASE_URL"), "/"),
		EventName:        getenv("EVENT_NAME", "Event Pass"),
		SMTPHost:         os.Getenv("SMTP_HOST"),
		SMTPPort:         getenv("SMTP_PORT", "587"),
		SMTPUsername:     os.Getenv("SMTP_USERNAME"),
		SMTPPassword:     os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:         os.Getenv("SMTP_FROM"),
		SMTPSecurity:     strings.ToLower(os.Getenv("SMTP_SECURITY")),

		GoogleClientID:     os.Getenv("GOOGLE_OAUTH_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET"),
		ProductName:        getenv("PRODUCT_NAME", "Event Pass Platform"),
		PassSigningKeys:    os.Getenv("PASS_SIGNING_KEYS"),
		IntegrationsKey:    os.Getenv("INTEGRATIONS_KEY"),
		StatusPageURL:      os.Getenv("STATUS_PAGE_URL"),
		MapTileURL:         os.Getenv("MAP_TILE_URL"),
		MapTileAttribution: os.Getenv("MAP_TILE_ATTRIBUTION"),
	}
	c.ClientIPHeader = os.Getenv("CLIENT_IP_HEADER")
	for _, e := range strings.Split(os.Getenv("PLATFORM_ADMIN_EMAILS"), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			c.PlatformAdminEmails = append(c.PlatformAdminEmails, e)
		}
	}
	var err error
	if c.LoginRatePerMin, err = getInt("LOGIN_RATE_PER_MIN", 30); err != nil {
		return nil, err
	}
	if c.PassRatePerMin, err = getInt("PASS_RATE_PER_MIN", 120); err != nil {
		return nil, err
	}
	if c.DBMaxConns, err = getInt("DB_MAX_CONNS", 10); err != nil {
		return nil, err
	}

	if c.SMTPSecurity == "" {
		// Port 465 is implicit TLS by convention; everything else upgrades.
		c.SMTPSecurity = "starttls"
		if c.SMTPPort == "465" {
			c.SMTPSecurity = "tls"
		}
	}
	if c.SMTPHost != "" {
		u, err := url.Parse(c.AppBaseURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, errors.New("APP_BASE_URL must be the site's public URL (e.g. https://pass.example.org) when SMTP_HOST is set")
		}
		if err := validateAppBaseURL(u); err != nil {
			return nil, err
		}
	}

	if c.EmailSendInterval, err = time.ParseDuration(getenv("EMAIL_SEND_INTERVAL", "2s")); err != nil || c.EmailSendInterval < time.Second {
		return nil, errors.New("EMAIL_SEND_INTERVAL must be a duration of at least 1s (e.g. 2s)")
	}
	// Gmail allows about 500 a day (2,000 on Workspace); staff credential
	// emails come out of the same allowance, hence the margin.
	if v := os.Getenv("EMAIL_DAILY_LIMIT"); v == "" {
		c.EmailDailyLimit = 450
	} else if n, err := strconv.Atoi(v); err != nil || n < 0 {
		return nil, errors.New("EMAIL_DAILY_LIMIT must be a whole number (0 for no limit)")
	} else {
		c.EmailDailyLimit = n
	}

	if c.SessionTTL, err = time.ParseDuration(getenv("SESSION_TTL", "12h")); err != nil || c.SessionTTL < 5*time.Minute {
		return nil, errors.New("SESSION_TTL must be a duration of at least 5m (e.g. 12h)")
	}
	if (c.GoogleClientID == "") != (c.GoogleClientSecret == "") {
		return nil, errors.New("set both GOOGLE_OAUTH_CLIENT_ID and GOOGLE_OAUTH_CLIENT_SECRET, or neither")
	}
	if c.GoogleClientID != "" {
		u, err := url.Parse(c.AppBaseURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, errors.New("APP_BASE_URL must be the site's public URL when Google sign-in is configured (the OAuth redirect URI is built from it)")
		}
		if err := validateAppBaseURL(u); err != nil {
			return nil, err
		}
	}

	if c.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	if len(c.JWTSecret) < 32 {
		return nil, errors.New("JWT_SECRET must be at least 32 bytes")
	}
	return c, nil
}

func validateAppBaseURL(u *url.URL) error {
	if u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return errors.New("APP_BASE_URL must use HTTPS except for localhost when authentication or email links are enabled")
	}
	return nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive whole number", key)
	}
	return n, nil
}
