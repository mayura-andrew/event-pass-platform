package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// MinQRKeyBytes is the shortest HMAC_SECRET_KEY accepted.
const MinQRKeyBytes = 32

// QRSignature returns hex(HMAC-SHA256(membership_id + ":" + email, secret)),
// the qr_signature stored for every delegate and encoded in their pass.
// Inputs must already be normalized (trimmed ID, trimmed lower-case email).
// The API and scripts/seed both sign with this, using the same key.
func QRSignature(membershipID, email string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(membershipID + ":" + email))
	return hex.EncodeToString(mac.Sum(nil))
}
