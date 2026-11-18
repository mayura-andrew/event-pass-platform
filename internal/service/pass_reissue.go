package service

import (
	"context"
	"errors"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

// PassReissuer bumps a pass's generation (postgres.RegistrationRepo).
type PassReissuer interface {
	ReissuePass(ctx context.Context, es authz.EventScope, participantID string) (int, error)
}

// MyPassReissuer is the participant's own reissue (postgres.PortalRepo).
type MyPassReissuer interface {
	ReissueMyPass(ctx context.Context, as authz.AccountScope, orgID, participantID string) error
}

var errNoReissue = errors.New("pass reissue is not available")

// ReissuePass replaces a participant's QR code: the old one (a lost badge, a
// photo of it) scans as "This pass was replaced". Organizers who can edit
// the event may do it; send the pass again afterwards.
func (s *RegistrationService) ReissuePass(ctx context.Context, es authz.EventScope, participantID string) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	r, ok := s.store.(PassReissuer)
	if !ok {
		return errNoReissue
	}
	_, err := r.ReissuePass(ctx, es, participantID)
	return err
}

// ReissueMyPass lets a participant replace their own QR code.
func (s *PortalService) ReissueMyPass(ctx context.Context, as authz.AccountScope, participantID string) error {
	pc, err := s.store.MyPass(ctx, as, participantID)
	if err != nil {
		return err
	}
	r, ok := s.store.(MyPassReissuer)
	if !ok {
		return errNoReissue
	}
	if pc.Participant.Status == domain.PartCancelled {
		return invalid("this registration is cancelled")
	}
	return r.ReissueMyPass(ctx, as, pc.Event.OrgID, participantID)
}
