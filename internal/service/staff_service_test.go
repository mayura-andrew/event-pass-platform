package service

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

func TestGenerateTempPassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		p, err := GenerateTempPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != TempPasswordLength {
			t.Fatalf("length %d: %q", len(p), p)
		}
		for _, set := range []string{tempUpper, tempLower, tempDigits, tempSymbols} {
			if !strings.ContainsAny(p, set) {
				t.Fatalf("%q lacks a character from %q", p, set)
			}
		}
		if strings.ContainsAny(p, "0O1lIo") {
			t.Fatalf("%q contains an ambiguous character", p)
		}
		if seen[p] {
			t.Fatalf("duplicate password %q", p)
		}
		seen[p] = true
	}
}

func TestBuildMIMEBlocksHeaderInjection(t *testing.T) {
	raw, err := buildMIME(mail.Address{Name: "Space Apps", Address: "no-reply@example.org"}, Message{
		To:      mail.Address{Name: "Eve\r\nBcc: victim@example.org", Address: "eve@example.org"},
		Subject: "Hi\r\nBcc: victim@example.org",
		Text:    "body",
		HTML:    "<p>body</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	headers := strings.SplitN(string(raw), "\r\n\r\n", 2)[0]
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") {
			t.Fatalf("injected header line: %q", line)
		}
	}
}

func TestRenderCredentialsEmailEscapesHTML(t *testing.T) {
	_, text, html, err := RenderCredentialsEmail(CredentialsEmail{
		FullName: `<script>alert(1)</script>`, Email: "a@b.org", TempPassword: "Ab3!Ab3!Ab3!",
		RoleLabel: "Scanner", LoginURL: "https://pass.example.org/scanner.html", EventName: "Test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "<script>") {
		t.Fatal("name was not escaped in HTML")
	}
	if !strings.Contains(text, "Ab3!Ab3!Ab3!") || !strings.Contains(html, "Ab3!Ab3!Ab3!") {
		t.Fatal("password missing from email")
	}
}

func TestNormalizeStaffEmail(t *testing.T) {
	ok := map[string]string{" Gate1@Example.ORG ": "gate1@example.org", "a.b+c@sub.example.lk": "a.b+c@sub.example.lk"}
	for in, want := range ok {
		got, err := normalizeStaffEmail(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "nope", "a@b", "Name <a@b.org>", "a@b.org\r\nBcc: x@y.org", "a@@b.org"} {
		if _, err := normalizeStaffEmail(bad); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNormalizeGrants(t *testing.T) {
	id := "4bda9fd4-2c0c-4205-ab9c-aabe951edb43"
	got, err := normalizeGrants(domain.RoleScanner, []string{id, strings.ToUpper(id)})
	if err != nil || len(got) != 1 || got[0] != id {
		t.Fatalf("dedupe/lowercase: %v %v", got, err)
	}
	if got, _ := normalizeGrants(domain.RoleAdmin, []string{id}); got != nil {
		t.Fatal("admins should not store grants")
	}
	if _, err := normalizeGrants(domain.RoleScanner, []string{"not-a-uuid"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("bad UUID accepted")
	}
}

// ---- Self-protection ---------------------------------------------------------

type nopStaffStore struct{ StaffStore }

func TestUpdateRejectsSelfLockout(t *testing.T) {
	s := NewStaffService(nopStaffStore{}, DisabledMailer(), "https://x.org", "Test", nil)
	me := "6321491f-9e31-45b6-80b9-970508829a6d"
	off, admin := false, domain.RoleAdmin
	for name, req := range map[string]domain.StaffUpdateRequest{
		"deactivate self": {IsActive: &off},
		"demote self":     {Role: &admin},
	} {
		if _, err := s.Update(context.Background(), me, me, req); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: got %v, want validation error", name, err)
		}
	}
}

// ---- Grant enforcement on sync -----------------------------------------------

func TestValidateScanEnforcesGrants(t *testing.T) {
	admin := "9ae14910-346e-46bd-9993-2cf364442860"
	part := "faa4a893-7a56-4fb6-8050-8b649ec65ffe"
	granted := "4bda9fd4-2c0c-4205-ab9c-aabe951edb43"
	other := "1c4d1f1e-8f3a-4c55-9a1e-0f6f6f2a9b10"
	known := map[string]struct{}{granted: {}, other: {}}
	scan := func(sc string) domain.SyncScanPayload {
		return domain.SyncScanPayload{IdempotencyKey: admin + "_" + part + "_" + sc, ParticipantID: part, ScenarioID: sc, ScannedAtDevice: time.Now()}
	}
	grants := map[string]struct{}{granted: {}}

	if msg := validateScan(admin, scan(granted), known, grants, map[string]struct{}{}); msg != "" {
		t.Fatalf("granted scan rejected: %s", msg)
	}
	if msg := validateScan(admin, scan(other), known, grants, map[string]struct{}{}); !strings.Contains(msg, "not assigned") {
		t.Fatalf("ungranted scan accepted: %q", msg)
	}
	// nil grants = manager: everything allowed
	if msg := validateScan(admin, scan(other), known, nil, map[string]struct{}{}); msg != "" {
		t.Fatalf("manager scan rejected: %s", msg)
	}
}
