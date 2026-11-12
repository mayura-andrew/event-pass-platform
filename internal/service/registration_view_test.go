package service

import (
	"testing"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

func TestParticipantViewsRespectAnswerVisibility(t *testing.T) {
	age := 16
	registeredAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	participant := domain.EventParticipant{
		ID: "participant-1", Email: "person@example.com", FullName: "Person One",
		Answers:   map[string]any{"food": "Vegan", "medical_note": "Restricted", "unknown": "Unlisted"},
		BirthDate: "2010-01-02", Age: &age, RegisteredAt: registeredAt,
	}
	fields := []domain.FormField{
		{Key: "food", Visibility: "staff"},
		{Key: "medical_note", Visibility: "admin"},
	}

	restricted := participantViews([]domain.EventParticipant{participant}, false, visibleParticipantAnswers(fields))
	if len(restricted) != 1 {
		t.Fatalf("restricted views: got %d, want 1", len(restricted))
	}
	view := restricted[0]
	if view.Answers["food"] != "Vegan" {
		t.Errorf("non-restricted answer missing: %v", view.Answers)
	}
	for _, key := range []string{"medical_note", "unknown"} {
		if _, ok := view.Answers[key]; ok {
			t.Errorf("restricted view includes %q", key)
		}
	}
	if view.BirthDate != "" || view.Age != nil {
		t.Errorf("restricted view includes birth-date data: birth_date=%q age=%v", view.BirthDate, view.Age)
	}

	organizer := participantViews([]domain.EventParticipant{participant}, true, nil)[0]
	if organizer.Answers["medical_note"] != "Restricted" || organizer.Answers["unknown"] != "Unlisted" ||
		organizer.BirthDate != participant.BirthDate || organizer.Age != participant.Age {
		t.Errorf("organizer view lost participant data: %+v", organizer)
	}
}
