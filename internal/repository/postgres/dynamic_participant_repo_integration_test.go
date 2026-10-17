package postgres

// Integration tests for custom fields (migration 005) against a real
// PostgreSQL. Skipped unless NSAC_TEST_DATABASE_URL is set. Field keys and
// emails carry a random tag and are deleted afterwards, so the tests can share
// a database with others.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type fieldFixture struct {
	fields *FieldRepo
	ingest *IngestRepo
	dash   *DashboardRepo
	parts  *ParticipantRepo
	tag    string
	keys   map[string]string // short name -> tagged field key
}

func newFieldFixture(t *testing.T) *fieldFixture {
	t.Helper()
	db := openTestDB(t)
	tag := randTag()
	f := &fieldFixture{
		fields: NewFieldRepo(db), ingest: NewIngestRepo(db), dash: NewDashboardRepo(db),
		parts: NewParticipantRepo(db), tag: tag,
		keys: map[string]string{
			"food":    "it_" + tag + "_food",
			"contact": "it_" + tag + "_contact",
			"laptops": "it_" + tag + "_laptops",
			"shirt":   "it_" + tag + "_shirt",
		},
	}
	t.Cleanup(func() {
		ctx := context.Background()
		db.ExecContext(ctx, `DELETE FROM participants WHERE email LIKE $1`, "it-"+tag+"-%")
		db.ExecContext(ctx, `DELETE FROM participant_field_definitions WHERE field_key LIKE $1`, "it\\_"+tag+"\\_%")
	})
	return f
}

// seed creates three fields and five delegates:
//
//	food (text, popup + filter), contact (text, neither), laptops (number, filter);
//	shirt is registered by the upload itself.
func (f *fieldFixture) seed(t *testing.T) []domain.CreatedParticipant {
	t.Helper()
	ctx := context.Background()
	for _, nf := range []domain.NewField{
		{Key: f.keys["food"], Label: "Food " + f.tag, DataType: "text", ShowInScannerPopup: true, ShowInDashboardFilter: true, Source: "manual"},
		{Key: f.keys["contact"], Label: "Contact " + f.tag, DataType: "text", Source: "manual"},
		{Key: f.keys["laptops"], Label: "Laptops " + f.tag, DataType: "number", ShowInDashboardFilter: true, Source: "manual"},
	} {
		if _, err := f.fields.CreateField(ctx, nf); err != nil {
			t.Fatal(err)
		}
	}

	attrs := []map[string]any{
		{f.keys["food"]: "Vegetarian", f.keys["contact"]: "+94 77 000 0001", f.keys["laptops"]: 2.0, f.keys["shirt"]: "M"},
		{f.keys["food"]: "Vegetarian", f.keys["laptops"]: 1.0},
		{f.keys["food"]: "Halal (no beef)"},
		{f.keys["contact"]: "+94 77 000 0004"},
		nil,
	}
	rows := make([]domain.NewParticipant, len(attrs))
	for i, a := range attrs {
		rows[i] = domain.NewParticipant{
			FullName: fmt.Sprintf("Delegate %d %s", i, f.tag), Email: fmt.Sprintf("it-%s-%d@test.local", f.tag, i),
			CustomAttributes: a,
		}
	}
	newFields := []domain.NewField{{Key: f.keys["shirt"], Label: "Shirt", DataType: "text", Source: "csv", SortOrder: 100}}
	created, _, err := f.ingest.BulkInsertWithAutoID(ctx, rows, newFields, testSign)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (f *fieldFixture) field(t *testing.T, list []domain.FieldDefinition, short string) domain.FieldDefinition {
	t.Helper()
	for _, d := range list {
		if d.Key == f.keys[short] {
			return d
		}
	}
	t.Fatalf("field %s not listed", short)
	return domain.FieldDefinition{}
}

func TestFieldsRegistrationUsageAndOptions(t *testing.T) {
	f := newFieldFixture(t)
	f.seed(t)
	ctx := context.Background()

	list, err := f.fields.ListFieldsWithUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	shirt := f.field(t, list, "shirt")
	if shirt.Source != "csv" || shirt.ShowInScannerPopup || shirt.ShowInDashboardFilter || shirt.UsageCount != 1 {
		t.Errorf("CSV-registered field = %+v; want source csv, toggles off, used once", shirt)
	}
	food := f.field(t, list, "food")
	if food.UsageCount != 3 || len(food.Options) != 2 || food.Options[0] != "Vegetarian" {
		t.Errorf("food usage %d options %v; want 3 and [Vegetarian, Halal...] most common first", food.UsageCount, food.Options)
	}
	if contact := f.field(t, list, "contact"); contact.Options != nil {
		t.Errorf("options listed for a non-filter field: %v", contact.Options)
	}
	laptops := f.field(t, list, "laptops")
	if len(laptops.Options) != 2 {
		t.Errorf("laptop options %v", laptops.Options)
	}

	// Type change is refused while values exist, allowed once they're gone.
	num := "number"
	if _, err := f.fields.UpdateField(ctx, f.keys["food"], domain.FieldUpdateRequest{DataType: &num}); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("type change on a used field: %v, want conflict", err)
	}
	if _, err := f.fields.CreateField(ctx, domain.NewField{Key: f.keys["food"], Label: "Again", DataType: "text", Source: "manual"}); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("duplicate key: %v, want conflict", err)
	}
	if _, err := f.fields.UpdateField(ctx, "it_"+f.tag+"_none", domain.FieldUpdateRequest{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing field: %v, want not found", err)
	}
}

func TestManifestCarriesOnlyPopupFields(t *testing.T) {
	f := newFieldFixture(t)
	created := f.seed(t)
	ctx := context.Background()

	manifest, err := f.parts.ListConfirmedManifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var first map[string]any
	for _, m := range manifest {
		if m.ID == created[0].ID {
			if err := json.Unmarshal(m.CustomAttributes, &first); err != nil {
				t.Fatal(err)
			}
		}
	}
	if first[f.keys["food"]] != "Vegetarian" {
		t.Errorf("popup field missing from manifest: %v", first)
	}
	for _, k := range []string{"contact", "laptops", "shirt"} {
		if _, leaked := first[f.keys[k]]; leaked {
			t.Errorf("%s reached the scanner manifest but isn't a popup field: %v", k, first)
		}
	}

	// Turning the contact field on puts it in the next manifest.
	on := true
	if _, err := f.fields.UpdateField(ctx, f.keys["contact"], domain.FieldUpdateRequest{ShowInScannerPopup: &on}); err != nil {
		t.Fatal(err)
	}
	scanner, err := f.fields.ScannerFields(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range scanner {
		found = found || s.Key == f.keys["contact"]
	}
	if !found {
		t.Error("contact not in scanner field list after enabling")
	}
	manifest, _ = f.parts.ListConfirmedManifest(ctx)
	for _, m := range manifest {
		if m.ID == created[0].ID {
			json.Unmarshal(m.CustomAttributes, &first)
		}
	}
	if first[f.keys["contact"]] != "+94 77 000 0001" {
		t.Errorf("enabled field not in manifest: %v", first)
	}
}

func TestDashboardCustomFilters(t *testing.T) {
	f := newFieldFixture(t)
	f.seed(t)
	ctx := context.Background()

	count := func(filters ...domain.CustomFilter) int {
		t.Helper()
		page, err := f.dash.GetFilteredParticipants(ctx, domain.ParticipantFilter{
			Search: f.tag, Custom: filters, Page: 1, PageSize: 50,
		})
		if err != nil {
			t.Fatal(err)
		}
		return page.Total
	}
	food, laptops := f.keys["food"], f.keys["laptops"]
	cases := []struct {
		name    string
		filters []domain.CustomFilter
		want    int
	}{
		{"no custom filter", nil, 5},
		{"eq text", []domain.CustomFilter{{Key: food, Op: "eq", Typed: "Vegetarian"}}, 2},
		{"eq is exact", []domain.CustomFilter{{Key: food, Op: "eq", Typed: "vegetarian"}}, 0},
		{"eq number", []domain.CustomFilter{{Key: laptops, Op: "eq", Typed: 2.0}}, 1},
		{"contains, case-insensitive", []domain.CustomFilter{{Key: food, Op: "contains", Value: "HALAL"}}, 1},
		{"contains with LIKE characters", []domain.CustomFilter{{Key: food, Op: "contains", Value: "%"}}, 0},
		{"missing", []domain.CustomFilter{{Key: food, Op: "missing"}}, 2},
		{"two filters AND", []domain.CustomFilter{
			{Key: food, Op: "eq", Typed: "Vegetarian"}, {Key: laptops, Op: "eq", Typed: 1.0}}, 1},
		{"hostile key matches nothing", []domain.CustomFilter{{Key: `x' OR '1'='1`, Op: "missing"}}, 5},
	}
	for _, c := range cases {
		if got := count(c.filters...); got != c.want {
			t.Errorf("%s: %d delegates, want %d", c.name, got, c.want)
		}
	}

	page, err := f.dash.GetFilteredParticipants(ctx, domain.ParticipantFilter{
		Search: f.tag, Custom: []domain.CustomFilter{{Key: laptops, Op: "eq", Typed: 2.0}}, Page: 1, PageSize: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	var attrs map[string]any
	json.Unmarshal(page.Rows[0].CustomAttributes, &attrs)
	if attrs[f.keys["contact"]] == nil {
		t.Error("dashboard rows must carry every custom attribute, including non-popup ones")
	}
}

func TestDeleteFieldRemovesValues(t *testing.T) {
	f := newFieldFixture(t)
	created := f.seed(t)
	ctx := context.Background()

	n, err := f.fields.DeleteField(ctx, f.keys["contact"])
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("removed %d values, want 2", n)
	}
	var left int
	f.fields.db.QueryRowContext(ctx, `SELECT count(*) FROM participants WHERE custom_attributes ? $1::text`, f.keys["contact"]).Scan(&left)
	if left != 0 {
		t.Errorf("%d delegates still hold the deleted field", left)
	}
	var food string
	f.fields.db.QueryRowContext(ctx, `SELECT custom_attributes->>$2::text FROM participants WHERE id = $1`, created[0].ID, f.keys["food"]).Scan(&food)
	if food != "Vegetarian" {
		t.Errorf("other fields must survive; food = %q", food)
	}
	if _, err := f.fields.DeleteField(ctx, f.keys["contact"]); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second delete: %v, want not found", err)
	}
}

func TestCustomAttributesSizeLimit(t *testing.T) {
	f := newFieldFixture(t)
	big := map[string]any{}
	for i := 0; i < 60; i++ {
		big[fmt.Sprintf("k%d", i)] = fmt.Sprintf("%0500d", i) // ~30 KB, well over 16 KB
	}
	_, _, err := f.ingest.BulkInsertWithAutoID(context.Background(), []domain.NewParticipant{{
		FullName: "Big", Email: "it-" + f.tag + "-big@test.local", CustomAttributes: big,
	}}, nil, testSign)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("oversized attributes: %v, want validation error", err)
	}
}
