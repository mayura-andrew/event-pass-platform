package service

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

func TestParseParticipantCSV(t *testing.T) {
	participants, err := ParseParticipantCSV(strings.NewReader("\ufeffEmail,Title,Name\r\nANN@example.com,Engineer,Ann Perera\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(participants) != 1 || participants[0].Email != "ann@example.com" ||
		participants[0].FullName != "Ann Perera" || participants[0].Source != "import" ||
		participants[0].Answers["title"] != "Engineer" {
		t.Fatalf("parsed participants: %+v", participants)
	}

	participants, err = ParseParticipantCSV(strings.NewReader("full_name,email,title\nBen Silva,ben@example.com,\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(participants) != 1 || len(participants[0].Answers) != 0 {
		t.Fatalf("blank title: %+v", participants)
	}
}

func TestParseParticipantCSVRejectsInvalidRows(t *testing.T) {
	tests := []struct {
		name string
		csv  string
	}{
		{"missing header", "Name,Email\nAnn,ann@example.com\n"},
		{"invalid email", "Name,Email,Title\nAnn Perera,not-an-email,Engineer\n"},
		{"duplicate email", "Name,Email,Title\nAnn Perera,ann@example.com,Engineer\nAnn P,ANN@example.com,Designer\n"},
		{"invalid title", "Name,Email,Title\nAnn Perera,ann@example.com," + strings.Repeat("x", 151) + "\n"},
		{"empty rows", "Name,Email,Title\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseParticipantCSV(strings.NewReader(tt.csv))
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("error = %v, want validation error", err)
			}
		})
	}
}

func TestParseParticipantCSVRowLimit(t *testing.T) {
	var csv strings.Builder
	csv.WriteString("Name,Email,Title\n")
	for i := 0; i <= MaxParticipantImportRows; i++ {
		fmt.Fprintf(&csv, "Person %d,person%d@example.com,Title\n", i, i)
	}
	_, err := ParseParticipantCSV(strings.NewReader(csv.String()))
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation error", err)
	}
}
