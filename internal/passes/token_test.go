package passes

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

const (
	tOrg  = "01a15000-0000-7000-8000-000000000001"
	tEv   = "01a15000-0000-7000-8000-000000000002"
	tPart = "01a15000-0000-7000-8000-000000000003"
)

func TestTokenGeneration(t *testing.T) {
	kr := DerivedKeyRing([]byte("test"))
	tok, err := kr.Issue(tOrg, tEv, tPart, "CSA-7KQ4-M2XD", 3)
	if err != nil {
		t.Fatal(err)
	}
	c, err := kr.Verify(tok)
	if err != nil || c.Generation != 3 || c.PassID != "CSA-7KQ4-M2XD" || c.ParticipantID != tPart {
		t.Fatalf("verify: %+v %v", c, err)
	}
	// The generation is signed: changing its bytes breaks the signature.
	body, sig, _ := cut(tok[len(prefix):])
	payload, _ := base64.RawURLEncoding.DecodeString(body)
	payload[51] = 9
	forged := prefix + base64.RawURLEncoding.EncodeToString(payload) + "." + sig
	if _, err := kr.Verify(forged); err != ErrBadSig {
		t.Fatalf("forged generation: %v, want ErrBadSig", err)
	}
	if _, err := kr.Issue(tOrg, tEv, tPart, "X", 0); err == nil {
		t.Fatal("generation 0 accepted")
	}
}

// Passes issued before generations (version 1) still verify, as generation 1.
func TestTokenVersion1StillVerifies(t *testing.T) {
	kr := DerivedKeyRing([]byte("test"))
	c := Claims{OrgID: tOrg, EventID: tEv, ParticipantID: tPart, PassID: "OLD-1", KeyID: 1}
	payload := []byte{versionV1, 1}
	for _, id := range []string{tOrg, tEv, tPart} {
		b, _ := uuidBytes(id)
		payload = append(payload, b...)
	}
	payload = append(payload, "OLD-1"...)
	sig := ed25519.Sign(kr.keys[1], message(versionV1, c))
	tok := prefix + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
	got, err := kr.Verify(tok)
	if err != nil || got.Generation != 1 || got.PassID != "OLD-1" {
		t.Fatalf("v1 token: %+v %v", got, err)
	}
}

func TestSameAs(t *testing.T) {
	a := DerivedKeyRing([]byte("secret-a"))
	if !a.SameAs(DerivedKeyRing([]byte("secret-a"))) || a.SameAs(DerivedKeyRing([]byte("secret-b"))) {
		t.Fatal("SameAs")
	}
}

func cut(s string) (string, string, bool) {
	for i := range s {
		if s[i] == '.' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}
