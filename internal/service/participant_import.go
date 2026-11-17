package service

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

// MaxParticipantImportRows is the largest accepted CSV import batch.
const MaxParticipantImportRows = 500

// ParseParticipantCSV validates the required Name and Category columns.
// Email is optional for imports because passes can be downloaded without
// sending email. The legacy Email and Title column names remain supported.
func ParseParticipantCSV(r io.Reader) ([]domain.EventParticipant, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, invalid("CSV is empty")
	}
	if err != nil {
		return nil, invalid("could not read CSV header: %v", err)
	}

	columns := make(map[string]int, len(header))
	for i, name := range header {
		name = strings.TrimSpace(strings.TrimPrefix(name, "\ufeff"))
		name = strings.ToLower(strings.ReplaceAll(name, " ", "_"))
		switch name {
		case "name", "full_name", "email", "title", "category":
			if _, exists := columns[name]; exists {
				return nil, invalid("CSV has duplicate %q columns", name)
			}
			columns[name] = i
		}
	}
	nameCol, okName := columns["name"]
	fullNameCol, hasFullName := columns["full_name"]
	if okName && hasFullName {
		return nil, invalid("CSV must have either Name or Full_Name, not both")
	}
	if !okName && hasFullName {
		nameCol, okName = fullNameCol, true
	}
	emailCol, okEmail := columns["email"]
	categoryCol, okCategory := columns["category"]
	categoryKey := "category"
	if !okCategory {
		categoryCol, okCategory = columns["title"]
		categoryKey = "title"
	}
	if !okName || !okCategory {
		return nil, invalid("CSV must have Name (or Full_Name) and Category columns")
	}

	participants := make([]domain.EventParticipant, 0)
	seen := make(map[string]bool)
	for line := 2; ; line++ {
		record, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, invalid("CSV row %d: %v", line, err)
		}
		if len(record) <= max(nameCol, categoryCol) || okEmail && len(record) <= emailCol {
			return nil, invalid("CSV row %d does not have all required columns", line)
		}
		name := strings.TrimSpace(record[nameCol])
		if !utf8.ValidString(name) {
			return nil, invalid("CSV row %d: Name must be valid UTF-8", line)
		}
		if n := utf8.RuneCountInString(name); n < 2 || n > 150 || !ValidDisplayName(name) {
			return nil, invalid("CSV row %d: Name must be 2 to 150 characters", line)
		}
		email := ""
		if okEmail {
			email, err = NormalizeEmail(record[emailCol])
			if err != nil {
				return nil, invalid("CSV row %d: enter a valid Email address", line)
			}
		} else {
			email = fmt.Sprintf("imported-%d@event-pass.local", line-1)
		}
		if seen[email] {
			return nil, invalid("CSV row %d: Email %q appears more than once", line, email)
		}
		seen[email] = true
		category := strings.TrimSpace(record[categoryCol])
		if !utf8.ValidString(category) {
			return nil, invalid("CSV row %d: Category must be valid UTF-8", line)
		}
		if utf8.RuneCountInString(category) > 150 || category != "" && !ValidDisplayName(category) {
			return nil, invalid("CSV row %d: Category must be 150 characters or fewer and on one line", line)
		}
		answers := map[string]any{}
		if category != "" {
			answers[categoryKey] = category
		}
		participants = append(participants, domain.EventParticipant{
			FullName: name, Email: email, Answers: answers, Source: "import",
		})
		if len(participants) > MaxParticipantImportRows {
			return nil, invalid("CSV can contain at most %d participant rows", MaxParticipantImportRows)
		}
	}
	if len(participants) == 0 {
		return nil, invalid("CSV has no participant rows")
	}
	return participants, nil
}
