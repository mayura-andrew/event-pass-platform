package postgres

import (
	"reflect"
	"testing"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

func TestMergeUpload(t *testing.T) {
	old := storedParticipant{
		ID: "id", MembershipID: "NSAC-001000", Email: "ada@example.org",
		FullName: "Ada Reyes", TeamName: "Orion", DateOfBirth: "1998-04-12",
		Custom: map[string]any{"food": "Vegetarian", "laptops": 2.0, "vip": true},
	}

	// Blank cells keep everything; identical values are not changes.
	m, changed := mergeUpload(old, domain.NewParticipant{
		Email: "ada@example.org", TeamName: "Orion",
		CustomAttributes: map[string]any{"food": "Vegetarian", "laptops": 2.0},
	})
	if len(changed) != 0 || !reflect.DeepEqual(m, old) {
		t.Fatalf("no-op row changed %v -> %+v", changed, m)
	}

	m, changed = mergeUpload(old, domain.NewParticipant{
		Email: "ada@example.org", FullName: "Ada R. Reyes", VenueID: "v1",
		CustomAttributes: map[string]any{"laptops": 3.0, "shirt": "M"},
	})
	if want := []string{"full_name", "venue_id", "laptops", "shirt"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	want := map[string]any{"food": "Vegetarian", "laptops": 3.0, "vip": true, "shirt": "M"}
	if !reflect.DeepEqual(m.Custom, want) || m.TeamName != "Orion" || m.DateOfBirth != "1998-04-12" {
		t.Errorf("merged = %+v", m)
	}
	if m.Email != old.Email || m.MembershipID != old.MembershipID {
		t.Error("email and membership ID must never change")
	}
	if len(old.Custom) != 3 || old.Custom["laptops"] != 2.0 {
		t.Errorf("old map was modified: %v", old.Custom)
	}

	// A delegate with no custom attributes yet.
	m, changed = mergeUpload(storedParticipant{FullName: "Lin"}, domain.NewParticipant{CustomAttributes: map[string]any{"shirt": "S"}})
	if !reflect.DeepEqual(changed, []string{"shirt"}) || m.Custom["shirt"] != "S" {
		t.Errorf("from empty: %v %v", changed, m.Custom)
	}
}
