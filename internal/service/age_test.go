package service

import (
	"testing"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/repository/postgres"
)

func TestAgeOnDefaultsToStartDate(t *testing.T) {
	// 13 Nov 20:00 UTC is already 14 Nov in Colombo (UTC+5:30).
	start := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	ev := &domain.Event{StartsAt: &start, TimeZone: "Asia/Colombo"}
	if got := AgeOn(ev).Format("2006-01-02"); got != "2026-11-14" {
		t.Errorf("default cut-off %s, want the start date in the event's zone, 2026-11-14", got)
	}
	set := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ev.AgeOn = &set
	if got := AgeOn(ev).Format("2006-01-02"); got != "2026-11-01" {
		t.Errorf("set cut-off %s", got)
	}
	if !AgeOn(&domain.Event{}).IsZero() {
		t.Error("no dates: want zero")
	}
}

func TestSheetHasUnder18Column(t *testing.T) {
	cut := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	age15, age20 := 15, 20
	d := &postgres.SheetData{
		Event:  domain.Event{AgeOn: &cut, TimeZone: "Asia/Colombo"},
		Fields: []domain.FormField{{Key: "dob", Label: "Date of birth", Type: domain.FieldBirthDate}},
		Participants: []domain.EventParticipant{
			{FullName: "Young", Age: &age15, Answers: map[string]any{"dob": "2011-03-02"}},
			{FullName: "Grown", Age: &age20, Answers: map[string]any{"dob": "2006-01-01"}},
			{FullName: "Unknown", Answers: map[string]any{}},
		},
	}
	rows := SheetRows(d)["Participants"]
	head := rows[0]
	if head[len(head)-1] != "Under 18 on 14 Nov 2026" {
		t.Fatalf("last column %v", head[len(head)-1])
	}
	for i, want := range []any{"Yes", "No", ""} {
		if got := rows[i+1][len(head)-1]; got != want {
			t.Errorf("%s: %v, want %v", rows[i+1][1], got, want)
		}
	}
}
