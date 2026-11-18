package domain

import "time"

// PassClaim is a registration made with an account's email that is not yet
// linked to it. The account holder confirms it ("This is mine") before it
// becomes one of their passes, so someone who typed their address into a
// public form can't put a pass, answers or a birth date in their account.
type PassClaim struct {
	ParticipantID string
	EventName     string
	OrgName       string
	RegisteredAt  time.Time
}
