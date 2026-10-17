package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

func TestNormalizeFieldKey(t *testing.T) {
	cases := map[string]string{
		"food_preference":          "food_preference",
		"Food Preference":          "food_preference",
		"  T-Shirt Size ":          "tshirt_size",
		"Parent's Contact #":       "parent_contact",
		"Parents' contact":         "parents_contact",
		"Don't know":               "dont_know",
		"Kids's":                   "kids",
		"Préférence alimentaire":   "preference_alimentaire",
		"\ufeffFull Name":          "full_name",
		"E-mail":                   "email",
		"Allergies (if any)":       "allergies_if_any",
		"2nd choice":               "", // must start with a letter
		"!!!":                      "",
		"":                         "",
		strings.Repeat("abc ", 20): "abc_abc_abc_abc_abc_abc_abc_abc_abc_abc_abc_abc_ab", // 50 max
		"Ünïcödé Nämé":             "unicode_name",
	}
	for in, want := range cases {
		if got := NormalizeFieldKey(in); got != want {
			t.Errorf("NormalizeFieldKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFieldLabelFromHeader(t *testing.T) {
	cases := [][3]string{
		{"T-Shirt Size", "tshirt_size", "T-Shirt Size"},
		{"food_preference", "food_preference", "Food Preference"},
		{"allergies", "allergies", "Allergies"},
		{"  Emergency   Contact ", "emergency_contact", "Emergency Contact"},
	}
	for _, c := range cases {
		if got := FieldLabelFromHeader(c[0], c[1]); got != c[2] {
			t.Errorf("FieldLabelFromHeader(%q) = %q, want %q", c[0], got, c[2])
		}
	}
}

func TestCoerceCustomValue(t *testing.T) {
	ok := []struct {
		typ, in string
		want    any
	}{
		{domain.FieldText, "  Vegetarian\n(no nuts) ", "Vegetarian (no nuts)"},
		{domain.FieldNumber, "1,250.5", 1250.5},
		{domain.FieldNumber, "-3", -3.0},
		{domain.FieldBoolean, "Yes", true},
		{domain.FieldBoolean, "n", false},
		{domain.FieldDate, "2026-10-18", "2026-10-18"},
		{domain.FieldText, "   ", nil},
		{domain.FieldNumber, "", nil},
	}
	for _, c := range ok {
		got, err := CoerceCustomValue(c.typ, c.in)
		if err != nil || got != c.want {
			t.Errorf("CoerceCustomValue(%s, %q) = %v, %v; want %v", c.typ, c.in, got, err, c.want)
		}
	}
	bad := [][2]string{
		{domain.FieldNumber, "twelve"},
		{domain.FieldNumber, "NaN"},
		{domain.FieldNumber, "1e999"},
		{domain.FieldBoolean, "maybe"},
		{domain.FieldDate, "18/10/2026"},
		{domain.FieldText, strings.Repeat("x", domain.MaxCustomValueLen+1)},
		{domain.FieldText, "bell\a"},
	}
	for _, c := range bad {
		if _, err := CoerceCustomValue(c[0], c[1]); err == nil {
			t.Errorf("CoerceCustomValue(%s, %q) accepted", c[0], c[1])
		}
	}
}

func TestImportCSVCustomColumns(t *testing.T) {
	store := newFakeIngestStore()
	store.fields = []domain.FieldDefinition{
		{Key: "food_preference", Label: "Food Preference", DataType: domain.FieldText},
		{Key: "laptop_count", Label: "Laptops", DataType: domain.FieldNumber},
	}
	svc := newTestIngest(store)

	csv := "Name,E-mail,Food Preference,Laptops,T-Shirt Size,Guardian's Phone,Never Filled,venue_name,\n" +
		"Ada,ada@example.org,Vegetarian,2,M,+94 77 123 4567,,Hall A,junk\n" +
		"Lin,lin@example.org,,1,L,,,,\n" +
		"Kwame,k@example.org,Halal,two,,,,,\n" + // laptops isn't a number
		",,,,,,,,\n"

	res, err := svc.ImportCSV(context.Background(), "actor", strings.NewReader(csv), domain.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 2 || len(res.Errors) != 1 {
		t.Fatalf("created %d, errors %+v", len(res.Created), res.Errors)
	}
	if e := res.Errors[0]; e.Line != 4 || !strings.Contains(e.Message, "Laptops") || !strings.Contains(e.Message, "not a number") {
		t.Errorf("type error reported as %+v", e)
	}

	ada := res.Created[0].CustomAttributes
	want := map[string]any{"food_preference": "Vegetarian", "laptop_count": 2.0, "tshirt_size": "M", "guardian_phone": "+94 77 123 4567"}
	if len(ada) != len(want) {
		t.Fatalf("ada's attributes = %v, want %v", ada, want)
	}
	for k, v := range want {
		if ada[k] != v {
			t.Errorf("ada[%s] = %#v, want %#v", k, ada[k], v)
		}
	}
	if _, ok := res.Created[1].CustomAttributes["food_preference"]; ok {
		t.Error("a blank cell must leave the key out, not store an empty string")
	}

	wantFields := "food_preference,laptop_count,tshirt_size,guardian_phone,never_filled"
	if got := strings.Join(res.CustomFields, ","); got != wantFields {
		t.Errorf("custom fields = %s, want %s", got, wantFields)
	}
	// never_filled has no value anywhere, so it isn't registered.
	if got := strings.Join(res.NewFields, ","); got != "tshirt_size,guardian_phone" {
		t.Errorf("new fields = %s", got)
	}
	for _, f := range store.newFields {
		if f.DataType != domain.FieldText || f.Source != domain.FieldSourceCSV {
			t.Errorf("registered %+v; new CSV fields must be text from source csv", f)
		}
	}
	if store.newFields[0].Label != "T-Shirt Size" || store.newFields[1].Label != "Guardian's Phone" {
		t.Errorf("labels = %q, %q", store.newFields[0].Label, store.newFields[1].Label)
	}
	if got := strings.Join(res.IgnoredColumns, ","); got != "venue_name" {
		t.Errorf("ignored = %q, want venue_name (the blank header is dropped silently)", got)
	}
}

func TestImportCSVHeaderClashes(t *testing.T) {
	cases := map[string]string{
		"two spellings of one key": "full_name,email,Food Preference,food_preference\n",
		"alias and core together":  "full_name,email,E-mail\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newTestIngest(newFakeIngestStore()).ImportCSV(context.Background(), "a", strings.NewReader(body), domain.ImportOptions{})
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("err = %v, want validation error", err)
			}
		})
	}

	t.Run("too many new fields", func(t *testing.T) {
		store := newFakeIngestStore()
		for i := 0; i < domain.MaxFieldDefinitions-1; i++ {
			store.fields = append(store.fields, domain.FieldDefinition{Key: "f" + strings.Repeat("x", i+1), DataType: "text"})
		}
		_, err := newTestIngest(store).ImportCSV(context.Background(), "a",
			strings.NewReader("full_name,email,new_one,new_two\nA,a@example.org,1,2\n"), domain.ImportOptions{})
		if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "at most") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCreateParticipantCustomAttributes(t *testing.T) {
	store := newFakeIngestStore()
	store.fields = []domain.FieldDefinition{{Key: "vip", Label: "VIP", DataType: domain.FieldBoolean}}
	svc := newTestIngest(store)
	ctx := context.Background()

	p, err := svc.Create(ctx, "a", domain.ParticipantCreateRequest{
		FullName: "Ada", Email: "ada@example.org", CustomAttributes: map[string]string{"vip": "yes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.CustomAttributes["vip"] != true {
		t.Fatalf("vip = %#v", p.CustomAttributes["vip"])
	}

	_, err = svc.Create(ctx, "a", domain.ParticipantCreateRequest{
		FullName: "Lin", Email: "lin@example.org", CustomAttributes: map[string]string{"made_up": "x"},
	})
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field: %v", err)
	}
	if store.newFields != nil {
		t.Fatal("single entry must never register fields")
	}
}

func TestValidateCustomFilters(t *testing.T) {
	defs := []domain.FieldDefinition{
		{Key: "food_preference", Label: "Food", DataType: domain.FieldText, ShowInDashboardFilter: true},
		{Key: "laptops", Label: "Laptops", DataType: domain.FieldNumber, ShowInDashboardFilter: true},
		{Key: "parent_contact", Label: "Parent", DataType: domain.FieldText}, // not a filter
	}
	got, err := validateCustomFilters([]domain.CustomFilter{
		{Key: "food_preference", Value: "Vegetarian"},
		{Key: "laptops", Op: "EQ", Value: "2"},
	}, defs)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Op != domain.CustomEquals || got[0].Typed != "Vegetarian" || got[1].Typed != 2.0 {
		t.Fatalf("got %+v", got)
	}

	got, err = validateCustomFilters([]domain.CustomFilter{
		{Key: "food_preference", Op: "contains", Value: "  "}, // empty search: dropped
		{Key: "laptops", Op: "missing", Value: "ignored"},
	}, defs)
	if err != nil || len(got) != 1 || got[0].Op != domain.CustomMissing || got[0].Value != "" {
		t.Fatalf("got %+v, %v", got, err)
	}

	bad := map[string][]domain.CustomFilter{
		"not a filter field": {{Key: "parent_contact", Value: "x"}},
		"unknown key":        {{Key: "x' OR 1=1 --", Value: "x"}},
		"contains on number": {{Key: "laptops", Op: "contains", Value: "2"}},
		"wrong type":         {{Key: "laptops", Value: "many"}},
		"unknown op":         {{Key: "laptops", Op: "gt", Value: "1"}},
		"same field twice":   {{Key: "laptops", Value: "1"}, {Key: "laptops", Value: "2"}},
	}
	for name, in := range bad {
		if _, err := validateCustomFilters(in, defs); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: err = %v, want validation error", name, err)
		}
	}
	tooMany := make([]domain.CustomFilter, domain.MaxCustomFilters+1)
	if _, err := validateCustomFilters(tooMany, defs); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("too many filters accepted")
	}
}

func TestFieldServiceCreateValidation(t *testing.T) {
	svc := NewFieldService(nil) // validation fails before the store is used
	bad := []domain.FieldCreateRequest{
		{Key: "Food Pref", Label: "Food"},   // not normalized
		{Key: "email", Label: "Email"},      // core column
		{Key: "e_mail", Label: "Email"},     // alias of a core column
		{Key: "venue_name", Label: "Venue"}, // seed-format column
		{Key: "ok_key", Label: ""},
		{Key: "ok_key", Label: "OK", DataType: "json"},
	}
	for _, req := range bad {
		if _, err := svc.Create(context.Background(), "a", req); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("Create(%+v) err = %v, want validation error", req, err)
		}
	}
}

func TestImportCSVUpdateExisting(t *testing.T) {
	newStore := func() *fakeIngestStore {
		s := newFakeIngestStore()
		s.registered["ada@example.org"] = "NSAC-001000"
		s.registered["lin@example.org"] = "NSAC-001001"
		return s
	}
	ctx := context.Background()
	body := "email,full_name,T-Shirt Size\n" +
		"ada@example.org,,M\n" + // registered, name blank: update the shirt only
		"lin@example.org,,\n" + // registered, nothing to change
		"new@example.org,,L\n" + // new delegate without a name: error
		"kwame@example.org,Kwame,XL\n" + // new delegate: created
		"ADA@example.org,,S\n" // repeats line 2

	t.Run("update mode", func(t *testing.T) {
		store := newStore()
		res, err := newTestIngest(store).ImportCSV(ctx, "a", strings.NewReader(body), domain.ImportOptions{UpdateExisting: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Updated) != 1 || res.Updated[0].Line != 2 || res.Updated[0].MembershipID != "NSAC-001000" {
			t.Errorf("updated = %+v", res.Updated)
		}
		if len(res.Unchanged) != 1 || res.Unchanged[0].Line != 3 {
			t.Errorf("unchanged = %+v", res.Unchanged)
		}
		if len(res.Errors) != 1 || res.Errors[0].Line != 4 || !strings.Contains(res.Errors[0].Message, "full name is required for a new delegate") {
			t.Errorf("errors = %+v", res.Errors)
		}
		if len(res.Created) != 1 || res.Created[0].Email != "kwame@example.org" {
			t.Errorf("created = %+v", res.Created)
		}
		if len(res.Duplicates) != 1 || res.Duplicates[0].Line != 6 {
			t.Errorf("duplicates = %+v", res.Duplicates)
		}
		if n := len(res.Created) + len(res.Updated) + len(res.Unchanged) + len(res.Duplicates) + len(res.Errors); n != res.TotalRows {
			t.Errorf("%d rows accounted for, want %d", n, res.TotalRows)
		}
		// tshirt_size has values in both an update and a new row: registered once.
		if strings.Join(res.NewFields, ",") != "tshirt_size" {
			t.Errorf("new fields = %v", res.NewFields)
		}
	})

	t.Run("default mode refuses a file without names", func(t *testing.T) {
		_, err := newTestIngest(newStore()).ImportCSV(ctx, "a",
			strings.NewReader("email,tshirt_size\nada@example.org,M\n"), domain.ImportOptions{})
		if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "full_name") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("default mode still skips registered emails", func(t *testing.T) {
		store := newStore()
		res, err := newTestIngest(store).ImportCSV(ctx, "a",
			strings.NewReader("full_name,email,tshirt_size\nAda,ada@example.org,M\n"), domain.ImportOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Duplicates) != 1 || len(store.updateRows) != 0 {
			t.Fatalf("duplicates %+v, update rows %d", res.Duplicates, len(store.updateRows))
		}
	})
}
