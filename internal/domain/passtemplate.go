package domain

import (
	"encoding/json"
	"time"
)

// ---- Pass templates (SaaS milestone M5) --------------------------------------
// See migrations/012_pass_templates.sql. Designs are JSON for internal/passes.

type PassTemplate struct {
	ID             string                `json:"id"`
	Name           string                `json:"name"`
	CurrentVersion int                   `json:"current_version"`
	Design         json.RawMessage       `json:"design"` // the current version's
	CreatedAt      time.Time             `json:"created_at"`
	UpdatedAt      time.Time             `json:"updated_at"`
	ArchivedAt     *time.Time            `json:"archived_at,omitempty"`
	UsedBy         int                   `json:"used_by"` // events pinned to any version
	Versions       []PassTemplateVersion `json:"versions,omitempty"`
}

type PassTemplateVersion struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by_name"`
}

// PassAsset is an uploaded image a design can show ("asset:<id>").
type PassAsset struct {
	ID          string    `json:"id"`
	Src         string    `json:"src"` // what a design's image element uses
	ContentType string    `json:"content_type"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	CreatedAt   time.Time `json:"created_at"`
}
