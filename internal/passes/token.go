// Package passes issues and checks the QR credential on a participant's pass
// (SaaS milestone M3).
//
// A token is "EP1.<payload>.<signature>", both parts base64url:
//
//	payload   = version(1) | keyID(1) | org(16) | event(16) | participant(16) | passID
//	signature = Ed25519 over "ep1|" + org + "|" + event + "|" + participant + "|" + passID + "|" + keyID
//
// It carries no personal data and never signs anyone in. Keys are numbered:
// new passes are signed with the current key, and old keys stay in the ring
// to verify passes already sent, so keys rotate without reissuing passes.
package passes

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	prefix  = "EP1."
	version = 1
)

var (
	ErrMalformed  = errors.New("not a pass QR code")
	ErrBadSig     = errors.New("pass signature is not valid")
	ErrUnknownKey = errors.New("pass was signed with an unknown key")
)

// Claims are what a valid token proves.
type Claims struct {
	OrgID         string
	EventID       string
	ParticipantID string
	PassID        string
	KeyID         int
}

// KeyRing holds the signing keys.
type KeyRing struct {
	current int
	keys    map[int]ed25519.PrivateKey
}

// ParseKeyRing reads "1:<base64 seed>,2:<base64 seed>"; the highest ID signs.
// Seeds are 32 bytes (openssl rand -base64 32).
func ParseKeyRing(spec string) (*KeyRing, error) {
	kr := &KeyRing{keys: map[int]ed25519.PrivateKey{}}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idStr, seedStr, ok := strings.Cut(part, ":")
		id, err := strconv.Atoi(idStr)
		if !ok || err != nil || id < 1 || id > 255 {
			return nil, fmt.Errorf("pass key %q: want <id 1-255>:<base64 seed>", idStr)
		}
		seed, err := base64.StdEncoding.DecodeString(seedStr)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("pass key %d: the seed must be %d bytes, base64 (openssl rand -base64 32)", id, ed25519.SeedSize)
		}
		kr.keys[id] = ed25519.NewKeyFromSeed(seed)
		if id > kr.current {
			kr.current = id
		}
	}
	if kr.current == 0 {
		return nil, errors.New("no pass signing keys")
	}
	return kr, nil
}

// DerivedKeyRing makes a one-key ring from another secret, for local
// development when PASS_SIGNING_KEYS is unset. Production sets real keys.
func DerivedKeyRing(secret []byte) *KeyRing {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("event pass signing key v1"))
	return &KeyRing{current: 1, keys: map[int]ed25519.PrivateKey{1: ed25519.NewKeyFromSeed(mac.Sum(nil))}}
}

func message(c Claims) []byte {
	return []byte("ep1|" + c.OrgID + "|" + c.EventID + "|" + c.ParticipantID + "|" + c.PassID + "|" + strconv.Itoa(c.KeyID))
}

func uuidBytes(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(b) != 16 {
		return nil, fmt.Errorf("not a UUID: %q", s)
	}
	return b, nil
}

func uuidString(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Issue signs a token for one participant's pass.
func (kr *KeyRing) Issue(orgID, eventID, participantID, passID string) (string, error) {
	if passID == "" || len(passID) > 40 {
		return "", errors.New("pass ID must be 1 to 40 characters")
	}
	payload := []byte{version, byte(kr.current)}
	for _, id := range []string{orgID, eventID, participantID} {
		b, err := uuidBytes(id)
		if err != nil {
			return "", err
		}
		payload = append(payload, b...)
	}
	payload = append(payload, passID...)
	c := Claims{OrgID: orgID, EventID: eventID, ParticipantID: participantID, PassID: passID, KeyID: kr.current}
	sig := ed25519.Sign(kr.keys[kr.current], message(c))
	enc := base64.RawURLEncoding
	return prefix + enc.EncodeToString(payload) + "." + enc.EncodeToString(sig), nil
}

// Verify checks a token and returns what it proves. Callers then check the
// event matches where it is scanned ("Pass is for another event").
func (kr *KeyRing) Verify(token string) (*Claims, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, prefix) {
		return nil, ErrMalformed
	}
	body, sigStr, ok := strings.Cut(strings.TrimPrefix(token, prefix), ".")
	if !ok {
		return nil, ErrMalformed
	}
	enc := base64.RawURLEncoding
	payload, err1 := enc.DecodeString(body)
	sig, err2 := enc.DecodeString(sigStr)
	if err1 != nil || err2 != nil || len(payload) < 2+48+1 || payload[0] != version || len(sig) != ed25519.SignatureSize {
		return nil, ErrMalformed
	}
	c := Claims{
		KeyID:         int(payload[1]),
		OrgID:         uuidString(payload[2:18]),
		EventID:       uuidString(payload[18:34]),
		ParticipantID: uuidString(payload[34:50]),
		PassID:        string(payload[50:]),
	}
	key, ok := kr.keys[c.KeyID]
	if !ok {
		return nil, ErrUnknownKey
	}
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), message(c), sig) {
		return nil, ErrBadSig
	}
	return &c, nil
}
