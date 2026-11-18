// Package passes issues and checks the QR credential on a participant's pass
// (SaaS milestone M3).
//
// A token is "EP1.<payload>.<signature>", both parts base64url:
//
//	payload   = version(1) | keyID(1) | org(16) | event(16) | participant(16) | passID          (version 1)
//	payload   = version(2) | keyID(1) | org(16) | event(16) | participant(16) | generation(2) | passID
//	signature = Ed25519 over "ep1|" + org + "|" + event + "|" + participant + "|" + passID + "|" + keyID
//	            (version 2 appends "|g" + generation)
//
// The generation is the participant row's pass_generation. Reissuing a pass
// bumps it, and the scanner refuses a token whose generation is not the
// current one, so a photographed or lost badge stops working. Version 1
// tokens (issued before generations) count as generation 1.
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
	prefix    = "EP1."
	versionV1 = 1
	version   = 2
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
	Generation    int // 1 for version 1 tokens
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

// SameAs reports whether any key in kr is also in other, so production can
// refuse a configured key that is really the development-derived one.
func (kr *KeyRing) SameAs(other *KeyRing) bool {
	for _, a := range kr.keys {
		for _, b := range other.keys {
			if a.Equal(b) {
				return true
			}
		}
	}
	return false
}

func message(v byte, c Claims) []byte {
	m := "ep1|" + c.OrgID + "|" + c.EventID + "|" + c.ParticipantID + "|" + c.PassID + "|" + strconv.Itoa(c.KeyID)
	if v >= 2 {
		m += "|g" + strconv.Itoa(c.Generation)
	}
	return []byte(m)
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

// Issue signs a token for one participant's pass at its current generation.
func (kr *KeyRing) Issue(orgID, eventID, participantID, passID string, generation int) (string, error) {
	if passID == "" || len(passID) > 40 {
		return "", errors.New("pass ID must be 1 to 40 characters")
	}
	if generation < 1 || generation > 0xffff {
		return "", errors.New("pass generation must be 1 to 65535")
	}
	payload := []byte{version, byte(kr.current)}
	for _, id := range []string{orgID, eventID, participantID} {
		b, err := uuidBytes(id)
		if err != nil {
			return "", err
		}
		payload = append(payload, b...)
	}
	payload = append(payload, byte(generation>>8), byte(generation))
	payload = append(payload, passID...)
	c := Claims{OrgID: orgID, EventID: eventID, ParticipantID: participantID, PassID: passID, KeyID: kr.current, Generation: generation}
	sig := ed25519.Sign(kr.keys[kr.current], message(version, c))
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
	if err1 != nil || err2 != nil || len(payload) < 2+48+1 || len(sig) != ed25519.SignatureSize {
		return nil, ErrMalformed
	}
	v := payload[0]
	c := Claims{
		KeyID:         int(payload[1]),
		OrgID:         uuidString(payload[2:18]),
		EventID:       uuidString(payload[18:34]),
		ParticipantID: uuidString(payload[34:50]),
		Generation:    1,
	}
	switch v {
	case versionV1:
		c.PassID = string(payload[50:])
	case version:
		if len(payload) < 2+48+2+1 {
			return nil, ErrMalformed
		}
		c.Generation = int(payload[50])<<8 | int(payload[51])
		c.PassID = string(payload[52:])
	default:
		return nil, ErrMalformed
	}
	key, ok := kr.keys[c.KeyID]
	if !ok {
		return nil, ErrUnknownKey
	}
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), message(v, c), sig) {
		return nil, ErrBadSig
	}
	return &c, nil
}
