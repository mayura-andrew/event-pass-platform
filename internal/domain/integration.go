package domain

import (
	"encoding/json"
	"time"
)

// ---- Integrations (SaaS milestone M5) ----------------------------------------
// See migrations/013_integrations.sql.

const (
	IntegrationSheets = "google_sheets"
	IntegrationForm   = "google_form"
)

// Integration is one connection, without its secret.
type Integration struct {
	ID           string          `json:"id"`
	EventID      string          `json:"event_id"`
	Kind         string          `json:"kind"`
	Config       json.RawMessage `json:"config"`
	GoogleEmail  string          `json:"google_email"`
	Status       string          `json:"status"`
	LastSyncedAt *time.Time      `json:"last_synced_at,omitempty"`
	LastError    string          `json:"last_error,omitempty"`
	Imported     int             `json:"imported"`
	CreatedAt    time.Time       `json:"created_at"`
}

// SheetsConfig is a google_sheets integration's config.
type SheetsConfig struct {
	SpreadsheetID string `json:"spreadsheet_id"`
	URL           string `json:"url"`
	Title         string `json:"title"`
}

// FormConfig is a google_form integration's config.
type FormConfig struct {
	FormID        string            `json:"form_id"`
	Title         string            `json:"title"`
	NameQuestion  string            `json:"name_question"`  // question ID with the full name
	EmailQuestion string            `json:"email_question"` // question ID, or "" for the respondent's email
	Map           map[string]string `json:"map"`            // question ID -> event question key
	Ready         bool              `json:"ready"`          // the mapping is saved; importing runs
	Questions     []FormQuestion    `json:"questions"`      // as read from the form when linked
}

// FormQuestion is one question of a linked Google Form.
type FormQuestion struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// IntegrationJob is a due integration for the worker, with its sealed secret.
type IntegrationJob struct {
	Integration
	OrgID  string
	Secret []byte
}
