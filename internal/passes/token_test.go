package passes

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/nasa-spaceapps/nsac-qr/internal/auth"
)

const (
	org  = "01a15000-0000-7000-8000-0000000000a1"
	ev   = "01a15000-0000-7000-8000-0000000000e1"
	part = "01a15000-0000-7000-8000-0000000000f1"
)

func seed(b byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(rune('a'+b)), 32)))
}

func TestIssueAndVerify(t *testing.T) {
	kr, err := ParseKeyRing("1:" + seed(1))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := kr.Issue(org, ev, part, "CSA-7KQ4-M2XD")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "EP1.") || len(tok) > 200 {
		t.Fatalf("token %q (%d chars)", tok, len(tok))
	}
	c, err := kr.Verify(tok)
	if err != nil || *c != (Claims{OrgID: org, EventID: ev, ParticipantID: part, PassID: "CSA-7KQ4-M2XD", KeyID: 1}) {
		t.Fatalf("verify: %+v, %v", c, err)
	}
}

func TestTamperingFails(t *testing.T) {
	kr, _ := ParseKeyRing("1:" + seed(1))
	tok, _ := kr.Issue(org, ev, part, "CSA-7KQ4-M2XD")
	body, sig, _ := strings.Cut(strings.TrimPrefix(tok, "EP1."), ".")
	payload, _ := base64.RawURLEncoding.DecodeString(body)

	// Same signature, another event: the classic "use my pass at their event".
	other := append([]byte{}, payload...)
	other[20] ^= 0xFF
	forged := "EP1." + base64.RawURLEncoding.EncodeToString(other) + "." + sig
	if _, err := kr.Verify(forged); !errors.Is(err, ErrBadSig) {
		t.Errorf("changed event: %v, want ErrBadSig", err)
	}
	// Signed by someone else's key.
	foreign, _ := ParseKeyRing("1:" + seed(9))
	ftok, _ := foreign.Issue(org, ev, part, "CSA-7KQ4-M2XD")
	if _, err := kr.Verify(ftok); !errors.Is(err, ErrBadSig) {
		t.Errorf("foreign key: %v, want ErrBadSig", err)
	}
	for _, bad := range []string{"", "hello", "EP1.", "EP1.abc", "EP1.!!!.###", `{"id":"x","sig":"y"}`} {
		if _, err := kr.Verify(bad); !errors.Is(err, ErrMalformed) {
			t.Errorf("Verify(%q): %v, want ErrMalformed", bad, err)
		}
	}
}

func TestKeyRotation(t *testing.T) {
	old, _ := ParseKeyRing("1:" + seed(1))
	oldTok, _ := old.Issue(org, ev, part, "P-1")
	ring, err := ParseKeyRing("1:" + seed(1) + ", 2:" + seed(2))
	if err != nil {
		t.Fatal(err)
	}
	if c, err := ring.Verify(oldTok); err != nil || c.KeyID != 1 {
		t.Fatalf("pass from before rotation: %+v, %v", c, err)
	}
	newTok, _ := ring.Issue(org, ev, part, "P-1")
	if c, _ := ring.Verify(newTok); c == nil || c.KeyID != 2 {
		t.Fatal("new passes aren't signed with the newest key")
	}
	if _, err := old.Verify(newTok); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("new pass on an old ring: %v, want ErrUnknownKey", err)
	}
	for _, bad := range []string{"", "x", "1:short", "0:" + seed(1), "abc:" + seed(1)} {
		if _, err := ParseKeyRing(bad); err == nil {
			t.Errorf("ParseKeyRing(%q) accepted", bad)
		}
	}
}

// QR codes already printed by the current build keep verifying.
func TestLegacyPasses(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	sig := auth.QRSignature("NSAC-1092", "participant@spaceapps.org", key)
	payload := `{"id":"` + part + `","sig":"` + sig + `"}`
	l, ok := ParseLegacy(payload)
	if !ok || l.ID != part {
		t.Fatalf("ParseLegacy: %+v %v", l, ok)
	}
	if !VerifyLegacy(l, "NSAC-1092", "participant@spaceapps.org", key) {
		t.Error("valid legacy signature rejected")
	}
	if VerifyLegacy(l, "NSAC-1093", "participant@spaceapps.org", key) {
		t.Error("legacy signature accepted for another delegate")
	}
	if _, ok := ParseLegacy("EP1.abc.def"); ok {
		t.Error("a new token parsed as legacy")
	}
}

func TestDerivedKeyRingIsStable(t *testing.T) {
	a, b := DerivedKeyRing([]byte("secret")), DerivedKeyRing([]byte("secret"))
	tok, _ := a.Issue(org, ev, part, "P-1")
	if _, err := b.Verify(tok); err != nil {
		t.Fatal("derived ring changes between restarts")
	}
}
