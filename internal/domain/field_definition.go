package domain

import "time"

// ---- Custom delegate fields (participants.custom_attributes) -----------------

// Field data types. They decide how a value is validated and stored in the
// JSON object: text -> string, number -> number, boolean -> true/false,
// date -> "YYYY-MM-DD" string.
const (
	FieldText    = "text"
	FieldNumber  = "number"
	FieldBoolean = "boolean"
	FieldDate    = "date"
)

// ValidFieldType reports whether t is one of the four field data types.
func ValidFieldType(t string) bool {
	return t == FieldText || t == FieldNumber || t == FieldBoolean || t == FieldDate
}

// Where a field definition came from.
const (
	FieldSourceSeed   = "seed"   // migrations/005
	FieldSourceManual = "manual" // added on the Fields page
	FieldSourceCSV    = "csv"    // found as an extra column in an upload
)

// FieldDefinition describes one key of participants.custom_attributes.
type FieldDefinition struct {
	ID                    string    `json:"id"`
	Key                   string    `json:"field_key"`
	Label                 string    `json:"field_label"`
	DataType              string    `json:"data_type"`
	ShowInScannerPopup    bool      `json:"show_in_scanner_popup"`
	ShowInDashboardFilter bool      `json:"show_in_dashboard_filter"`
	SortOrder             int       `json:"sort_order"`
	Source                string    `json:"source"`
	CreatedAt             time.Time `json:"created_at"`

	// Filled by the admin listing only.
	UsageCount int `json:"usage_count"` // delegates with a value for this key
	// Most common values, for the dashboard filter dropdown (filter fields
	// only). OptionsTruncated means there are more than MaxFilterOptions, so
	// the dashboard offers a text box instead.
	Options          []string `json:"options,omitempty"`
	OptionsTruncated bool     `json:"options_truncated,omitempty"`
}

// ScannerField is what a scanner phone needs to label popup values.
type ScannerField struct {
	Key      string `json:"field_key"`
	Label    string `json:"field_label"`
	DataType string `json:"data_type"`
}

// FieldCreateRequest is the body of POST /api/v1/admin/fields.
type FieldCreateRequest struct {
	Key                   string `json:"field_key"`
	Label                 string `json:"field_label"`
	DataType              string `json:"data_type"`
	ShowInScannerPopup    bool   `json:"show_in_scanner_popup"`
	ShowInDashboardFilter bool   `json:"show_in_dashboard_filter"`
	SortOrder             *int   `json:"sort_order"`
}

// FieldUpdateRequest is the body of PATCH /api/v1/admin/fields/{key}. Nil
// fields are left unchanged. The key itself can't change.
type FieldUpdateRequest struct {
	Label                 *string `json:"field_label"`
	DataType              *string `json:"data_type"` // only while no delegate has a value
	ShowInScannerPopup    *bool   `json:"show_in_scanner_popup"`
	ShowInDashboardFilter *bool   `json:"show_in_dashboard_filter"`
	SortOrder             *int    `json:"sort_order"`
}

// NewField is a validated definition ready to insert.
type NewField struct {
	Key                   string
	Label                 string
	DataType              string
	ShowInScannerPopup    bool
	ShowInDashboardFilter bool
	SortOrder             int
	Source                string
	CreatedBy             string // admin ID; empty for CSV-registered fields
}

// ---- Dashboard filter criteria ------------------------------------------------

// Custom filter operators.
const (
	CustomEquals   = "eq"       // value equals (uses the GIN index)
	CustomContains = "contains" // text value contains, case-insensitive
	CustomMissing  = "missing"  // no value given
)

// CustomFilter narrows the delegate table by one custom field. Value is the
// text the organizer picked or typed; the service converts it to the field's
// type before it reaches SQL.
type CustomFilter struct {
	Key   string `json:"key"`
	Op    string `json:"op"` // CustomEquals (default) | CustomContains | CustomMissing
	Value string `json:"value"`

	// Set by the service: Value converted to the field's JSON type.
	Typed any `json:"-"`
}

// ---- Limits ---------------------------------------------------------------------

const (
	MaxFieldKeyLen        = 50
	MaxFieldLabelLen      = 100
	MaxCustomValueLen     = 500 // characters per value
	MaxFieldDefinitions   = 50  // in total, so a wide spreadsheet can't flood the schema
	MaxCustomFilters      = 10  // per dashboard request
	MaxFilterOptions      = 40  // dropdown entries before falling back to a text box
	DefaultFieldSortOrder = 100
)

// ReservedFieldKeys can't be custom fields: they are participant columns, or
// columns of the scripts/seed CSV format. A CSV header with one of these names
// is never packed into custom_attributes. Keep in step with the CHECK in
// migrations/005_jsonb_dynamic_schema.sql.
var ReservedFieldKeys = map[string]bool{
	"id": true, "membership_id": true, "email": true, "full_name": true,
	"team_name": true, "date_of_birth": true, "venue_id": true, "assigned_venue_id": true,
	"is_confirmed": true, "qr_signature": true, "created_at": true, "custom_attributes": true,
	"venue_name": true, "hall_section": true,
}
