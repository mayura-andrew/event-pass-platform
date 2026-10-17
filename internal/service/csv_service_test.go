package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/auth"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

const testVenue = "6f1c2a9e-0b7d-4c1e-9a55-1d2e3f4a5b6c"

// fakeIngestStore hands out NSAC-001000, NSAC-001001, ... like the sequence.
type fakeIngestStore struct {
	registered map[string]string // email -> membership ID
	raceEmails map[string]bool   // registered by "someone else" during the insert
	next       int
	inserted   []domain.NewParticipant
	insertCall int
	fields     []domain.FieldDefinition
	updateRows []domain.NewParticipant
	newFields  []domain.NewField
}

func newFakeIngestStore() *fakeIngestStore {
	return &fakeIngestStore{registered: map[string]string{}, raceEmails: map[string]bool{}, next: 1000}
}

func (f *fakeIngestStore) ExistingEmails(_ context.Context, emails []string) (map[string]string, error) {
	out := map[string]string{}
	for _, e := range emails {
		if id, ok := f.registered[e]; ok {
			out[e] = id
		}
	}
	return out, nil
}

func (f *fakeIngestStore) VenueIDs(context.Context) (map[string]bool, error) {
	return map[string]bool{testVenue: true}, nil
}

// BulkUpdateByEmail reports every non-blank value as a change; the real merge
// rules are tested on mergeUpload and against PostgreSQL.
func (f *fakeIngestStore) BulkUpdateByEmail(_ context.Context, rows []domain.NewParticipant, newFields []domain.NewField) ([]domain.UpdatedParticipant, []domain.NewParticipant, []domain.NewParticipant, error) {
	f.newFields = append(f.newFields, newFields...)
	f.updateRows = append(f.updateRows, rows...)
	var updated []domain.UpdatedParticipant
	var unchanged []domain.NewParticipant
	for _, p := range rows {
		var changed []string
		if p.FullName != "" {
			changed = append(changed, "full_name")
		}
		for k := range p.CustomAttributes {
			changed = append(changed, k)
		}
		if len(changed) == 0 {
			unchanged = append(unchanged, p)
			continue
		}
		updated = append(updated, domain.UpdatedParticipant{Line: p.Line, Email: p.Email, MembershipID: f.registered[p.Email], Changed: changed})
	}
	return updated, unchanged, nil, nil
}

func (f *fakeIngestStore) FieldDefinitions(context.Context) ([]domain.FieldDefinition, error) {
	return f.fields, nil
}

func (f *fakeIngestStore) BulkInsertWithAutoID(_ context.Context, rows []domain.NewParticipant, newFields []domain.NewField, sign func(string, string) string) ([]domain.CreatedParticipant, []domain.NewParticipant, error) {
	f.insertCall++
	f.newFields = append(f.newFields, newFields...)
	var created []domain.CreatedParticipant
	var skipped []domain.NewParticipant
	for _, p := range rows {
		id := fmt.Sprintf("NSAC-%06d", f.next)
		f.next++
		if f.raceEmails[p.Email] {
			skipped = append(skipped, p)
			continue
		}
		f.registered[p.Email] = id
		f.inserted = append(f.inserted, p)
		created = append(created, domain.CreatedParticipant{
			Line: p.Line, ID: "uuid-" + id, MembershipID: id, FullName: p.FullName,
			Email: p.Email, TeamName: p.TeamName, DateOfBirth: p.DateOfBirth,
			VenueID: p.VenueID, QRSignature: sign(id, p.Email), CustomAttributes: p.CustomAttributes,
		})
	}
	return created, skipped, nil
}

var testKey = []byte("0123456789abcdef0123456789abcdef")

func newTestIngest(store IngestStore) *IngestService {
	s := NewIngestService(store, testKey)
	s.today = func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }
	return s
}

func TestQRSignatureVector(t *testing.T) {
	// hex(HMAC-SHA256("NSAC-001000:ada@example.org", testKey)), computed
	// independently with Python's hmac module. If this changes, every pass
	// already sent stops verifying.
	const want = "28e2cc185afb13e1c703a709c21a0068e8e5962b7cac9d4591fd72549b90195d"
	if got := auth.QRSignature("NSAC-001000", "ada@example.org", testKey); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestImportCSV(t *testing.T) {
	store := newFakeIngestStore()
	store.registered["taken@example.org"] = "NSAC-1092"
	svc := newTestIngest(store)

	csv := "\ufeffFull Name,Email,Team Name,Date of Birth,Venue ID\r\n" +
		"Ada Reyes,ADA@Example.org ,Orion,1998-04-12," + strings.ToUpper(testVenue) + "\r\n" +
		"  Lin   Tan ,lin@example.org,,,\r\n" +
		",,,,\r\n" + // blank spreadsheet row: ignored, not counted
		"Kwame,taken@example.org,,,\r\n" +
		"Ada Again,ada@example.org,,,\r\n" +
		"No Email,,,,\r\n" +
		",nobody@example.org,,,\r\n" +
		"Bad Mail,not-an-email,,,\r\n" +
		"Bad Date,date@example.org,,12/04/1998,\r\n" +
		"Future,future@example.org,,2030-01-01,\r\n" +
		"Bad Venue,venue@example.org,,,not-a-uuid\r\n" +
		"Gone Venue,gone@example.org,,,00000000-0000-0000-0000-000000000000\r\n" +
		"\"Multi\nLine\",multi@example.org,\"Team, with comma\",,\r\n" +
		"Short Row,short@example.org\r\n"

	res, err := svc.ImportCSV(context.Background(), "actor", strings.NewReader(csv), domain.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if res.TotalRows != 13 {
		t.Errorf("TotalRows = %d, want 13", res.TotalRows)
	}
	if got := len(res.Created) + len(res.Duplicates) + len(res.Errors); got != res.TotalRows {
		t.Errorf("created+duplicates+errors = %d, want every row (%d) accounted for", got, res.TotalRows)
	}

	wantCreated := map[string]int{ // email -> line
		"ada@example.org":   2,
		"lin@example.org":   3,
		"multi@example.org": 14, // the record starts here and runs onto line 15
		"short@example.org": 16,
	}
	if len(res.Created) != len(wantCreated) {
		t.Fatalf("created %d rows, want %d: %+v", len(res.Created), len(wantCreated), res.Created)
	}
	for _, c := range res.Created {
		line, ok := wantCreated[c.Email]
		if !ok || c.Line != line {
			t.Errorf("created %s on line %d; want line %d", c.Email, c.Line, line)
		}
		if c.QRSignature != auth.QRSignature(c.MembershipID, c.Email, testKey) {
			t.Errorf("%s: signature doesn't match its membership ID and email", c.Email)
		}
	}
	ada := res.Created[0]
	if ada.MembershipID != "NSAC-001000" || ada.FullName != "Ada Reyes" || ada.TeamName != "Orion" ||
		ada.DateOfBirth != "1998-04-12" || ada.VenueID != testVenue {
		t.Errorf("first row stored as %+v", ada)
	}
	if res.Created[1].FullName != "Lin Tan" {
		t.Errorf("name not tidied: %q", res.Created[1].FullName)
	}
	if res.Created[2].FullName != "Multi Line" || res.Created[2].TeamName != "Team, with comma" {
		t.Errorf("quoted fields: %+v", res.Created[2])
	}

	wantDup := map[int]string{5: "already registered as NSAC-1092", 6: "repeats line 2 of this file"}
	if len(res.Duplicates) != len(wantDup) {
		t.Fatalf("duplicates = %+v", res.Duplicates)
	}
	for _, d := range res.Duplicates {
		if wantDup[d.Line] != d.Message {
			t.Errorf("duplicate on line %d: %q, want %q", d.Line, d.Message, wantDup[d.Line])
		}
	}

	wantErr := map[int]string{
		7:  "email is required",
		8:  "full name is required",
		9:  "not a valid email",
		10: "YYYY-MM-DD",
		11: "not a plausible birth date",
		12: "not a venue ID",
		13: "doesn't match any venue",
	}
	if len(res.Errors) != len(wantErr) {
		t.Fatalf("errors = %+v", res.Errors)
	}
	for _, e := range res.Errors {
		if want := wantErr[e.Line]; want == "" || !strings.Contains(e.Message, want) {
			t.Errorf("error on line %d: %q, want it to mention %q", e.Line, e.Message, want)
		}
	}
	if store.insertCall != 1 {
		t.Errorf("BulkInsertWithAutoID called %d times, want one batch", store.insertCall)
	}
}

func TestImportCSVRaceDuplicate(t *testing.T) {
	store := newFakeIngestStore()
	store.raceEmails["late@example.org"] = true
	res, err := newTestIngest(store).ImportCSV(context.Background(), "actor",
		strings.NewReader("full_name,email\nA,a@example.org\nLate,late@example.org\n"), domain.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 || len(res.Duplicates) != 1 || res.Duplicates[0].Line != 3 {
		t.Fatalf("got %+v", res)
	}
}

func TestImportCSVRejectsWholeFile(t *testing.T) {
	tooMany := "full_name,email\n" + strings.Repeat("A,a@example.org\n", domain.MaxImportRows+1)
	cases := map[string]string{
		"empty":           "",
		"missing column":  "full_name,team_name\nAda,Orion\n",
		"membership col":  "membership_id,full_name,email\nNSAC-1,Ada,a@example.org\n",
		"repeated column": "full_name,email,email\nAda,a@example.org,b@example.org\n",
		"broken quote":    "full_name,email\n\"Ada,a@example.org\n",
		"too many rows":   tooMany,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeIngestStore()
			_, err := newTestIngest(store).ImportCSV(context.Background(), "actor", strings.NewReader(body), domain.ImportOptions{})
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("err = %v, want a validation error", err)
			}
			if store.insertCall != 0 {
				t.Fatal("rows were inserted from a rejected file")
			}
		})
	}
}

func TestCreateParticipant(t *testing.T) {
	store := newFakeIngestStore()
	svc := newTestIngest(store)
	ctx := context.Background()

	p, err := svc.Create(ctx, "actor", domain.ParticipantCreateRequest{
		FullName: " Ada  Reyes ", Email: "Ada@Example.org", TeamName: "Orion",
		DateOfBirth: "1998-04-12", VenueID: testVenue,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.MembershipID != "NSAC-001000" || p.Email != "ada@example.org" || p.FullName != "Ada Reyes" {
		t.Fatalf("created %+v", p)
	}

	_, err = svc.Create(ctx, "actor", domain.ParticipantCreateRequest{FullName: "Ada", Email: "ada@example.org"})
	if !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), "NSAC-001000") {
		t.Fatalf("second create: %v, want a conflict naming the existing ID", err)
	}

	_, err = svc.Create(ctx, "actor", domain.ParticipantCreateRequest{FullName: "", Email: "x@example.org"})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("blank name: %v, want validation error", err)
	}

	store.raceEmails["late@example.org"] = true
	_, err = svc.Create(ctx, "actor", domain.ParticipantCreateRequest{FullName: "Late", Email: "late@example.org"})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("race: %v, want conflict", err)
	}
}
