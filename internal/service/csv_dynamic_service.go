package service

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// ---- Dynamic CSV columns -------------------------------------------------------
//
// A delegate CSV has five core columns (ImportColumns). Every other column is
// a custom field: its values go into participants.custom_attributes under a
// key made from the header ("T-Shirt Size" -> tshirt_size... see
// NormalizeFieldKey), and a key seen for the first time is registered in
// participant_field_definitions as a text field with both display toggles off.

var fieldKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,49}$`)

// headerAliases maps common spellings of the core columns onto them, so an
// export with "E-mail" or "DOB" doesn't turn a core column into a custom one.
var headerAliases = map[string]string{
	"name":              "full_name",
	"e_mail":            "email",
	"email_address":     "email",
	"team":              "team_name",
	"dob":               "date_of_birth",
	"birth_date":        "date_of_birth",
	"birthday":          "date_of_birth",
	"assigned_venue_id": "venue_id",
}

// NormalizeFieldKey turns a CSV header into a snake_case key: lower case,
// possessive 's, apostrophes and hyphens dropped, and runs of anything else
// other than a-z and 0-9 become one underscore. Letters
// with accents keep their base letter ("Préférence" -> "preference").
// Returns "" when nothing usable is left or the result doesn't start with a
// letter.
func NormalizeFieldKey(header string) string {
	header = strings.TrimPrefix(header, "\ufeff")
	var b strings.Builder
	underscore := false
	runes := []rune(strings.ToLower(header))
	for i := 0; i < len(runes); i++ {
		r := foldAccent(runes[i])
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			underscore = false
		case r == '\'' || r == '’':
			// A possessive 's is dropped ("Parent's contact" -> parent_contact,
			// the same key as "Parent contact"); other apostrophes join.
			if i+1 < len(runes) && runes[i+1] == 's' && (i+2 == len(runes) || !isKeyRune(foldAccent(runes[i+2]))) {
				i++
			}
		case r == '-':
			// A joiner, not a separator: "T-Shirt" -> tshirt, "E-mail" -> email.
		default:
			if !underscore && b.Len() > 0 {
				b.WriteByte('_')
				underscore = true
			}
		}
	}
	key := strings.TrimRight(b.String(), "_")
	if len(key) > domain.MaxFieldKeyLen {
		key = strings.TrimRight(key[:domain.MaxFieldKeyLen], "_")
	}
	if !fieldKeyRe.MatchString(key) {
		return ""
	}
	return key
}

func isKeyRune(r rune) bool { return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' }

// foldAccent maps common Latin letters with diacritics to their base letter.
func foldAccent(r rune) rune {
	if r < 0x80 {
		return r
	}
	const from = "àáâãäåāçćčèéêëēěìíîïīñńòóôõöøōśšùúûüūýÿžź"
	const to = "aaaaaaaccceeeeeeiiiiinnooooooossuuuuuyyzz"
	if i := strings.IndexRune(from, r); i >= 0 {
		return []rune(to)[utf8.RuneCountInString(from[:i])]
	}
	return r
}

// FieldLabelFromHeader picks a display label: the header as written when it
// already reads like one ("T-Shirt Size"), otherwise the key in title case
// ("food_preference" -> "Food Preference").
func FieldLabelFromHeader(header, key string) string {
	h := strings.Join(strings.Fields(strings.TrimPrefix(header, "\ufeff")), " ")
	if h != "" && !strings.Contains(h, "_") && h != strings.ToLower(h) && ValidDisplayName(h) {
		if utf8.RuneCountInString(h) <= domain.MaxFieldLabelLen {
			return h
		}
	}
	words := strings.Split(key, "_")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// CoerceCustomValue validates raw (as typed in a CSV cell or form) for a
// field of type dataType and returns the value to store in the JSON object.
// Blank input returns (nil, nil): the key is left out.
func CoerceCustomValue(dataType, raw string) (any, error) {
	v := strings.Join(strings.Fields(raw), " ") // line breaks and tabs become spaces
	if v == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(v) > domain.MaxCustomValueLen {
		return nil, fmt.Errorf("is longer than %d characters", domain.MaxCustomValueLen)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return nil, errors.New("contains characters that aren't allowed")
		}
	}
	switch dataType {
	case domain.FieldNumber:
		n, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", ""), 64)
		if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return nil, fmt.Errorf("%q is not a number", v)
		}
		return n, nil
	case domain.FieldBoolean:
		switch strings.ToLower(v) {
		case "true", "yes", "y", "1":
			return true, nil
		case "false", "no", "n", "0":
			return false, nil
		}
		return nil, fmt.Errorf("%q is not yes or no", v)
	case domain.FieldDate:
		d, err := time.Parse("2006-01-02", v)
		if err != nil {
			return nil, fmt.Errorf("%q must be written YYYY-MM-DD", v)
		}
		return d.Format("2006-01-02"), nil
	default:
		return v, nil
	}
}

// customColumn is one non-core CSV column that will be stored.
type customColumn struct {
	index    int
	key      string
	label    string
	dataType string
	isNew    bool // not yet in participant_field_definitions
}

// columnPlan says what to do with each column of an uploaded CSV.
type columnPlan struct {
	core    map[string]int // core column name -> index
	custom  []customColumn
	ignored []string // headers neither core nor custom, as written
}

// planColumns classifies the header row against the existing field
// definitions. It rejects a header that can't be imported safely.
func planColumns(header []string, defs map[string]domain.FieldDefinition) (*columnPlan, error) {
	plan := &columnPlan{core: make(map[string]int)}
	seen := make(map[string]string) // key -> header that produced it
	newCount := 0

	// A header may name a field by its label as shown on the Fields page
	// ("Laptops" for laptop_count), not only by its key.
	byLabel := make(map[string]string, len(defs))
	for k, d := range defs {
		if lk := NormalizeFieldKey(d.Label); lk != "" && lk != k {
			if _, clash := defs[lk]; !clash {
				byLabel[lk] = k
			}
		}
	}

	for i, raw := range header {
		key := NormalizeFieldKey(raw)
		if alias, ok := headerAliases[key]; ok {
			key = alias
		} else if k, ok := byLabel[key]; ok {
			key = k
		}
		label := strings.TrimSpace(strings.TrimPrefix(raw, "\ufeff"))
		if key == "membership_id" {
			return nil, fmt.Errorf("%w: remove the membership_id column; IDs are assigned automatically "+
				"(to import delegates who already have IDs, use scripts/seed)", domain.ErrValidation)
		}
		if key == "" {
			if label != "" {
				plan.ignored = append(plan.ignored, label)
			}
			continue
		}
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("%w: columns %q and %q both mean %q; rename or remove one",
				domain.ErrValidation, prev, label, key)
		}
		seen[key] = label

		switch {
		case isCoreColumn(key):
			plan.core[key] = i
		case domain.ReservedFieldKeys[key]:
			plan.ignored = append(plan.ignored, label)
		default:
			col := customColumn{index: i, key: key, label: FieldLabelFromHeader(raw, key), dataType: domain.FieldText}
			if d, ok := defs[key]; ok {
				col.label, col.dataType = d.Label, d.DataType
			} else {
				col.isNew = true
				newCount++
			}
			plan.custom = append(plan.custom, col)
		}
	}

	var missing []string
	for _, name := range []string{"full_name", "email"} {
		if _, ok := plan.core[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: the header row must include %s (expected: %s, plus any extra columns)",
			domain.ErrValidation, strings.Join(missing, " and "), strings.Join(ImportColumns, ","))
	}
	if len(defs)+newCount > domain.MaxFieldDefinitions {
		return nil, fmt.Errorf("%w: this file adds %d new fields, but at most %d custom fields are allowed (%d exist); "+
			"remove columns you don't need or delete unused fields first",
			domain.ErrValidation, newCount, domain.MaxFieldDefinitions, len(defs))
	}
	return plan, nil
}

func isCoreColumn(key string) bool {
	for _, c := range ImportColumns {
		if c == key {
			return true
		}
	}
	return false
}

// customValues reads one row's custom columns. A value that doesn't fit its
// field's type fails the whole row, naming the column.
func (p *columnPlan) customValues(rec []string) (map[string]any, error) {
	var out map[string]any
	for _, c := range p.custom {
		if c.index >= len(rec) {
			continue
		}
		v, err := CoerceCustomValue(c.dataType, rec[c.index])
		if err != nil {
			return nil, fmt.Errorf("%s %v", c.label, err)
		}
		if v == nil {
			continue
		}
		if out == nil {
			out = make(map[string]any, len(p.custom))
		}
		out[c.key] = v
	}
	return out, nil
}

// customFromForm validates custom values sent with a single entry. Only
// fields that already exist are accepted: new fields come from CSV columns or
// the Fields page, not from a typo in a form.
func customFromForm(in map[string]string, defs map[string]domain.FieldDefinition) (map[string]any, error) {
	var out map[string]any
	for key, raw := range in {
		d, ok := defs[key]
		if !ok {
			return nil, fmt.Errorf("unknown field %q; add it on the Fields page first", key)
		}
		v, err := CoerceCustomValue(d.DataType, raw)
		if err != nil {
			return nil, fmt.Errorf("%s %v", d.Label, err)
		}
		if v == nil {
			continue
		}
		if out == nil {
			out = make(map[string]any, len(in))
		}
		out[key] = v
	}
	return out, nil
}

// fieldsToRegister returns the new custom columns that at least one of rows
// has a value for. A column that is new but empty everywhere isn't
// registered: it would only clutter the Fields page.
func (p *columnPlan) fieldsToRegister(rows []domain.NewParticipant) []domain.NewField {
	var out []domain.NewField
	for i, c := range p.custom {
		if !c.isNew {
			continue
		}
		for _, r := range rows {
			if _, ok := r.CustomAttributes[c.key]; ok {
				out = append(out, domain.NewField{
					Key:       c.key,
					Label:     c.label,
					DataType:  domain.FieldText,
					SortOrder: domain.DefaultFieldSortOrder + i,
					Source:    domain.FieldSourceCSV,
				})
				break
			}
		}
	}
	return out
}
