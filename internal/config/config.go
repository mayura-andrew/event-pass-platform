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
	Port             string        // HTTP listen port
	DatabaseURL      string        // Supabase Postgres connection string
	JWTSecret        []byte        // HS256 signing key, >= 32 bytes
	JWTTTL           time.Duration // admin token lifetime; must cover an event day offline
	PublicDir        string        // static PWA / portal files
	GoogleMapsAPIKey string        // browser key, restrict by HTTP referrer in GCP
	GoogleMapsMapID  string        // optional; enables Advanced Markers
	AllowedOrigin    string        // optional CORS origin when the frontend is hosted elsewhere
	CheckInCode      string        // scenario code the dashboard counts as "checked in"
	// Signs delegate QR passes. Must equal the key scripts/seed uses. Empty
	// turns off adding delegates from the dashboard.
	HMACSecretKey []byte

	DBMaxConns int // database connections this process may open

	ClientIPHeader  string // header the load balancer sets to the client IP
	LoginRatePerMin int    // sign-in attempts per client IP per minute
	PassRatePerMin  int    // public pass lookups per client IP per minute

	// Staff credential emails. SMTPHost empty disables staff provisioning.
	AppBaseURL   string // public URL used in emailed sign-in links
	EventName    string
	SMTPHost     string
	SMTPPort     string
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string // "Space Apps Colombo <no-reply@example.org>"
	SMTPSecurity string // starttls | tls | none
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
		CheckInCode:      getenv("CHECKIN_SCENARIO_CODE", "CHECK_IN"),
		HMACSecretKey:    []byte(os.Getenv("HMAC_SECRET_KEY")),
		AppBaseURL:       strings.TrimRight(os.Getenv("APP_BASE_URL"), "/"),
		EventName:        getenv("EVENT_NAME", "NASA Space Apps Colombo"),
		SMTPHost:         os.Getenv("SMTP_HOST"),
		SMTPPort:         getenv("SMTP_PORT", "587"),
		SMTPUsername:     os.Getenv("SMTP_USERNAME"),
		SMTPPassword:     os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:         os.Getenv("SMTP_FROM"),
		SMTPSecurity:     strings.ToLower(os.Getenv("SMTP_SECURITY")),
	}
	c.ClientIPHeader = os.Getenv("CLIENT_IP_HEADER")
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
	}

	ttl, err := time.ParseDuration(getenv("JWT_TTL", "14h"))
	if err != nil {
		return nil, fmt.Errorf("JWT_TTL: %w", err)
	}
	c.JWTTTL = ttl

	if c.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	if len(c.JWTSecret) < 32 {
		return nil, errors.New("JWT_SECRET must be at least 32 bytes")
	}
	if n := len(c.HMACSecretKey); n > 0 && n < 32 {
		return nil, errors.New("HMAC_SECRET_KEY must be at least 32 bytes (openssl rand -hex 32)")
	}
	return c, nil
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
