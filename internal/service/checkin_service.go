package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/internal/passes"
)

// CheckinStore persists checkpoints and check-ins (postgres.CheckinRepo).
type CheckinStore interface {
	Checkpoints(ctx context.Context, es authz.EventScope, archived bool) ([]domain.Checkpoint, error)
	Checkpoint(ctx context.Context, es authz.EventScope, id string) (*domain.Checkpoint, error)
	SaveCheckpoint(ctx context.Context, es authz.EventScope, c domain.Checkpoint) (*domain.Checkpoint, error)
	SetCheckpointArchived(ctx context.Context, es authz.EventScope, id string, archived bool) error
	ParticipantCheckins(ctx context.Context, es authz.EventScope, participantID string) ([]domain.Checkin, error)
	Record(ctx context.Context, es authz.EventScope, in domain.RecordInput) (*domain.Checkin, *domain.Checkin, bool, error)
	GetCheckin(ctx context.Context, es authz.EventScope, id string) (*domain.Checkin, error)
	Undo(ctx context.Context, es authz.EventScope, id string) error
	Recent(ctx context.Context, es authz.EventScope, limit int) ([]domain.CheckinRow, error)
	Stats(ctx context.Context, es authz.EventScope, now time.Time) (*domain.EventStats, error)
}

// WorkshopStore persists workshops and sign-ups (postgres.WorkshopRepo).
type WorkshopStore interface {
	List(ctx context.Context, es authz.EventScope) ([]domain.Workshop, error)
	Get(ctx context.Context, es authz.EventScope, id string) (*domain.Workshop, error)
	Save(ctx context.Context, es authz.EventScope, w domain.Workshop) (*domain.Workshop, error)
	Delete(ctx context.Context, es authz.EventScope, id string) error
	Signups(ctx context.Context, es authz.EventScope, workshopID string) ([]domain.WorkshopSignup, error)
	StaffSignUp(ctx context.Context, es authz.EventScope, workshopID, participantID string) (string, error)
	StaffCancel(ctx context.Context, es authz.EventScope, workshopID, participantID string) error
	SignupStatus(ctx context.Context, es authz.EventScope, workshopID, participantID string) (string, error)
	MyWorkshops(ctx context.Context, orgID, eventID, accountID, participantID string) ([]domain.Workshop, error)
	MySignUp(ctx context.Context, orgID, eventID, accountID, workshopID, participantID string) (string, error)
	MyCancel(ctx context.Context, orgID, eventID, accountID, workshopID, participantID string) error
}

// UndoWindow is how long check-in staff may undo their own scans (spec).
const UndoWindow = 30 * time.Second

type CheckinService struct {
	checkins  CheckinStore
	workshops WorkshopStore
	regs      *RegistrationService
	regStore  RegistrationStore
	passes    *PassService
	checker   ParticipantChecker
	now       func() time.Time
}

func NewCheckinService(checkins CheckinStore, workshops WorkshopStore, regs *RegistrationService, regStore RegistrationStore,
	passes *PassService, checker ParticipantChecker) *CheckinService {
	return &CheckinService{checkins: checkins, workshops: workshops, regs: regs, regStore: regStore, passes: passes,
		checker: checker, now: time.Now}
}

// ---- Checkpoints ----------------------------------------------------------------

func (s *CheckinService) Checkpoints(ctx context.Context, es authz.EventScope, archived bool) ([]domain.Checkpoint, error) {
	if !es.Can(authz.EventScan) && !es.Can(authz.EventDashboard) {
		return nil, domain.ErrForbidden
	}
	return s.checkins.Checkpoints(ctx, es, archived)
}

var scanRoles = []string{domain.EventRoleOwner, domain.EventRoleEditor, domain.EventRoleCheckin}

// SaveCheckpoint adds a custom checkpoint (ID empty) or changes a
// checkpoint's name and rules.
func (s *CheckinService) SaveCheckpoint(ctx context.Context, es authz.EventScope, c domain.Checkpoint) (*domain.Checkpoint, error) {
	if err := es.Require(authz.EventEdit); err != nil {
		return nil, err
	}
	c.Name = strings.TrimSpace(c.Name)
	if n := utf8.RuneCountInString(c.Name); n < 2 || n > 100 || !ValidDisplayName(c.Name) {
		return nil, invalid("the checkpoint name must be 2 to 100 characters")
	}
	r := &c.Rules
	if r.OpensAt != nil && r.ClosesAt != nil && !r.ClosesAt.After(*r.OpensAt) {
		return nil, invalid("the checkpoint closes before it opens")
	}
	for _, role := range r.Roles {
		if !slices.Contains(scanRoles, role) {
			return nil, invalid("roles that can record are owner, editor and checkin")
		}
	}
	all, err := s.checkins.Checkpoints(ctx, es, true)
	if err != nil {
		return nil, err
	}
	reqs := []string{}
	for _, id := range r.Requires {
		if id == "" || id == c.ID || slices.Contains(reqs, id) {
			continue
		}
		if !slices.ContainsFunc(all, func(x domain.Checkpoint) bool { return x.ID == id }) {
			return nil, invalid("a required checkpoint isn't part of this event")
		}
		reqs = append(reqs, id)
	}
	r.Requires = reqs
	return s.checkins.SaveCheckpoint(ctx, es, c)
}

func (s *CheckinService) SetCheckpointArchived(ctx context.Context, es authz.EventScope, id string, archived bool) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	return s.checkins.SetCheckpointArchived(ctx, es, id, archived)
}

// ---- Scanning --------------------------------------------------------------------

// ScanRequest is one QR code scanned at a checkpoint. With Record unset the
// scanner only checks and waits for staff to press Record (no fast lane).
type ScanRequest struct {
	Payload        string     `json:"payload"`
	CheckpointID   string     `json:"checkpoint_id"`
	Record         bool       `json:"record"`
	IdempotencyKey string     `json:"idempotency_key"`
	DeviceID       string     `json:"device_id"`
	ClientTime     *time.Time `json:"client_time"`
}

// RecordRequest records a checkpoint for someone found by lookup, or after
// a check-only scan.
type RecordRequest struct {
	ParticipantID  string     `json:"participant_id"`
	IdempotencyKey string     `json:"idempotency_key"`
	DeviceID       string     `json:"device_id"`
	ClientTime     *time.Time `json:"client_time"`
}

// ScanParticipant is what staff see about the person.
type ScanParticipant struct {
	ID       string        `json:"id"`
	FullName string        `json:"full_name"`
	PassID   string        `json:"pass_id"`
	Status   string        `json:"status"`
	Answers  []passes.Line `json:"answers"` // questions visible to staff
}

// ScanResult is the scanner's result card.
type ScanResult struct {
	Result      string           `json:"result"`
	Tone        string           `json:"tone"` // green | amber | red
	Message     string           `json:"message"`
	Detail      string           `json:"detail,omitempty"`
	Checkpoint  *CheckpointRef   `json:"checkpoint,omitempty"`
	Participant *ScanParticipant `json:"participant,omitempty"`
	// The check-in just recorded, or for a duplicate the earlier one.
	Checkin *domain.Checkin `json:"checkin,omitempty"`
	// Undoable: the caller may undo Checkin now.
	Undoable bool `json:"undoable"`
	// Workshop door: the person isn't signed up but staff may add them.
	CanAdd     bool `json:"can_add"`
	PlacesLeft int  `json:"places_left"`
}

type CheckpointRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var toneFor = map[string]string{
	domain.ScanRecorded: "green", domain.ScanReady: "green",
	domain.ScanDuplicate: "amber", domain.ScanNotSignedUp: "amber", CheckNotConfirmed: "amber",
	domain.ScanNotAllowed: "red", domain.ScanOutsideHours: "red", CheckInvalid: "red", CheckWrongEvent: "red", CheckCancelled: "red",
}

func result(code, msg string) *ScanResult {
	return &ScanResult{Result: code, Tone: toneFor[code], Message: msg}
}

// checkpointFor loads a checkpoint the caller may record.
func (s *CheckinService) checkpointFor(ctx context.Context, es authz.EventScope, id string) (*domain.Checkpoint, *ScanResult, error) {
	if err := es.Require(authz.EventScan); err != nil {
		return nil, nil, err
	}
	cp, err := s.checkins.Checkpoint(ctx, es, id)
	if err != nil {
		return nil, nil, err
	}
	if cp.ArchivedAt != nil {
		return nil, nil, fmt.Errorf("%w: this checkpoint is closed", domain.ErrConflict)
	}
	if len(cp.Rules.Roles) > 0 && !slices.Contains(cp.Rules.Roles, es.Role()) {
		r := result(domain.ScanNotAllowed, "Your role can't record "+cp.Name)
		r.Checkpoint = &CheckpointRef{cp.ID, cp.Name}
		return cp, r, nil
	}
	return cp, nil, nil
}

// Scan checks a scanned QR at a checkpoint and, with Record, records it.
func (s *CheckinService) Scan(ctx context.Context, es authz.EventScope, req ScanRequest) (*ScanResult, error) {
	cp, stop, err := s.checkpointFor(ctx, es, req.CheckpointID)
	if err != nil || stop != nil {
		return stop, err
	}
	check, err := s.regs.CheckPass(ctx, es, s.passes, s.checker, req.Payload)
	if err != nil {
		return nil, err
	}
	if check.Result != CheckValid {
		r := result(check.Result, check.Message)
		r.Checkpoint = &CheckpointRef{cp.ID, cp.Name}
		if check.Participant != nil {
			r.Participant, err = s.staffView(ctx, es, check.Participant)
		}
		return r, err
	}
	return s.decide(ctx, es, cp, check.Participant, req.Record, domain.RecordInput{
		IdempotencyKey: req.IdempotencyKey, DeviceID: req.DeviceID, ClientTime: req.ClientTime,
	})
}

// Record records a checkpoint for a participant by ID.
func (s *CheckinService) Record(ctx context.Context, es authz.EventScope, checkpointID string, req RecordRequest) (*ScanResult, error) {
	cp, stop, err := s.checkpointFor(ctx, es, checkpointID)
	if err != nil || stop != nil {
		return stop, err
	}
	p, err := s.regStore.GetParticipant(ctx, es, req.ParticipantID)
	if err != nil {
		return nil, err
	}
	if p.Status != domain.PartRegistered {
		code, msg := CheckNotConfirmed, statusWords[p.Status]
		if p.Status == domain.PartCancelled {
			code = CheckCancelled
		}
		r := result(code, msg)
		r.Checkpoint = &CheckpointRef{cp.ID, cp.Name}
		r.Participant, err = s.staffView(ctx, es, p)
		return r, err
	}
	return s.decide(ctx, es, cp, p, true, domain.RecordInput{
		IdempotencyKey: req.IdempotencyKey, DeviceID: req.DeviceID, ClientTime: req.ClientTime,
	})
}

var statusWords = map[string]string{
	domain.PartPending: "Waiting for approval", domain.PartWaitlisted: "On the waitlist",
	domain.PartCancelled: "Registration cancelled", domain.PartRegistered: "Registered",
}

// decide applies the checkpoint's rules to a registered participant and
// records when asked.
func (s *CheckinService) decide(ctx context.Context, es authz.EventScope, cp *domain.Checkpoint, p *domain.EventParticipant,
	record bool, in domain.RecordInput) (*ScanResult, error) {
	view, err := s.staffView(ctx, es, p)
	if err != nil {
		return nil, err
	}
	out := func(r *ScanResult) (*ScanResult, error) {
		r.Checkpoint, r.Participant = &CheckpointRef{cp.ID, cp.Name}, view
		return r, nil
	}
	now := s.now()
	if (cp.Rules.OpensAt != nil && now.Before(*cp.Rules.OpensAt)) || (cp.Rules.ClosesAt != nil && !now.Before(*cp.Rules.ClosesAt)) {
		return out(result(domain.ScanOutsideHours, cp.Name+" isn't open now"))
	}
	if cp.WorkshopID != "" {
		w, err := s.workshops.Get(ctx, es, cp.WorkshopID)
		if err != nil {
			return nil, err
		}
		if w.SignupMode != domain.SignupEveryone {
			st, err := s.workshops.SignupStatus(ctx, es, w.ID, p.ID)
			if err != nil {
				return nil, err
			}
			if st != domain.SignupSignedUp {
				r := result(domain.ScanNotSignedUp, "Not signed up")
				if st == domain.SignupWaitlisted {
					r.Detail = "On the waitlist for " + w.Title
				}
				r.PlacesLeft = w.PlacesLeft()
				r.CanAdd = r.PlacesLeft != 0
				return out(r)
			}
		}
	}
	done, err := s.checkins.ParticipantCheckins(ctx, es, p.ID)
	if err != nil {
		return nil, err
	}
	has := func(id string) *domain.Checkin {
		for i := range done {
			if done[i].CheckpointID == id {
				return &done[i]
			}
		}
		return nil
	}
	for _, req := range cp.Rules.Requires {
		if has(req) == nil {
			name := "another checkpoint"
			if c, err := s.checkins.Checkpoint(ctx, es, req); err == nil {
				name = c.Name
			}
			return out(result(domain.ScanNotAllowed, "Needs "+name+" first"))
		}
	}
	if !record {
		if prev := has(cp.ID); prev != nil && !cp.Rules.Repeatable {
			return out(s.duplicate(es, prev))
		}
		return out(result(domain.ScanReady, statusWords[p.Status]))
	}
	// Recording: Record decides duplicates, after recognising a retry of the
	// same scan by its idempotency key.
	if in.IdempotencyKey == "" || len(in.IdempotencyKey) > 80 {
		return nil, invalid("each scan needs an idempotency_key of up to 80 characters")
	}
	in.ParticipantID, in.CheckpointID, in.Once = p.ID, cp.ID, !cp.Rules.Repeatable
	rec, dup, _, err := s.checkins.Record(ctx, es, in)
	if err != nil {
		return nil, err
	}
	if dup != nil {
		return out(s.duplicate(es, dup))
	}
	r := result(domain.ScanRecorded, cp.Name+" recorded")
	r.Checkin, r.Undoable = rec, s.canUndo(es, rec)
	return out(r)
}

// duplicate says who recorded it and when (spec: amber with time and staff).
func (s *CheckinService) duplicate(es authz.EventScope, prev *domain.Checkin) *ScanResult {
	r := result(domain.ScanDuplicate, "Already recorded")
	r.Detail = fmt.Sprintf("%s by %s", prev.CreatedAt.UTC().Format("15:04 UTC, 2 Jan"), prev.StaffName)
	r.Checkin, r.Undoable = prev, s.canUndo(es, prev)
	return r
}

func (s *CheckinService) canUndo(es authz.EventScope, c *domain.Checkin) bool {
	if c == nil || c.UndoneAt != nil {
		return false
	}
	return es.Can(authz.EventUndoAny) ||
		(es.Can(authz.EventUndoOwn) && c.StaffID == es.AccountID() && s.now().Sub(c.CreatedAt) <= UndoWindow)
}

// staffView is the person as the scanner shows them: no admin-only answers.
func (s *CheckinService) staffView(ctx context.Context, es authz.EventScope, p *domain.EventParticipant) (*ScanParticipant, error) {
	fields, err := s.regStore.ListFields(ctx, es)
	if err != nil {
		return nil, err
	}
	v := &ScanParticipant{ID: p.ID, FullName: p.FullName, PassID: p.PassID, Status: p.Status, Answers: []passes.Line{}}
	for _, f := range fields {
		if f.Visibility == "admin" {
			continue
		}
		if a := answerText(p.Answers[f.Key]); a != "" {
			v.Answers = append(v.Answers, passes.Line{Label: f.Label, Value: a})
		}
	}
	return v, nil
}

// Undo removes a check-in: editors any, check-in staff their own within 30
// seconds.
func (s *CheckinService) Undo(ctx context.Context, es authz.EventScope, checkinID string) error {
	if !es.Can(authz.EventUndoAny) && !es.Can(authz.EventUndoOwn) {
		return domain.ErrForbidden
	}
	c, err := s.checkins.GetCheckin(ctx, es, checkinID)
	if err != nil {
		return err
	}
	if c.UndoneAt != nil {
		return fmt.Errorf("%w: this check-in was already undone", domain.ErrConflict)
	}
	if !s.canUndo(es, c) {
		return fmt.Errorf("%w: check-in staff can undo their own scans within 30 seconds", domain.ErrForbidden)
	}
	return s.checkins.Undo(ctx, es, checkinID)
}

// AddToWorkshop signs someone up at the workshop door, if there is room.
func (s *CheckinService) AddToWorkshop(ctx context.Context, es authz.EventScope, workshopID, participantID string) (string, error) {
	if err := es.Require(authz.EventScan); err != nil {
		return "", err
	}
	return s.workshops.StaffSignUp(ctx, es, workshopID, participantID)
}

// Lookup finds participants by name, email or pass ID for manual check-in.
func (s *CheckinService) Lookup(ctx context.Context, es authz.EventScope, q string) ([]ScanParticipant, error) {
	if err := es.Require(authz.EventLookup); err != nil {
		return nil, err
	}
	q = strings.TrimSpace(q)
	if utf8.RuneCountInString(q) < 2 {
		return []ScanParticipant{}, nil
	}
	list, _, err := s.regStore.ListParticipants(ctx, es, domain.ParticipantQuery{Q: q})
	if err != nil {
		return nil, err
	}
	out := []ScanParticipant{}
	for i := range list {
		if len(out) == 20 {
			break
		}
		v, err := s.staffView(ctx, es, &list[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

func (s *CheckinService) Recent(ctx context.Context, es authz.EventScope) ([]domain.CheckinRow, error) {
	if err := es.Require(authz.EventLookup); err != nil {
		return nil, err
	}
	return s.checkins.Recent(ctx, es, 30)
}

func (s *CheckinService) Stats(ctx context.Context, es authz.EventScope) (*domain.EventStats, error) {
	if err := es.Require(authz.EventDashboard); err != nil {
		return nil, err
	}
	return s.checkins.Stats(ctx, es, s.now())
}

// ---- Workshops (organizer) -----------------------------------------------------

func (s *CheckinService) Workshops(ctx context.Context, es authz.EventScope) ([]domain.Workshop, error) {
	if err := es.Require(authz.EventView); err != nil {
		return nil, err
	}
	return s.workshops.List(ctx, es)
}

var signupModes = []string{domain.SignupEveryone, domain.SignupOptIn, domain.SignupInvite}

func (s *CheckinService) SaveWorkshop(ctx context.Context, es authz.EventScope, w domain.Workshop) (*domain.Workshop, error) {
	if err := es.Require(authz.EventEdit); err != nil {
		return nil, err
	}
	w.Title, w.Room, w.Speakers = strings.TrimSpace(w.Title), strings.TrimSpace(w.Room), strings.TrimSpace(w.Speakers)
	w.Description = strings.TrimSpace(w.Description)
	switch {
	case utf8.RuneCountInString(w.Title) < 2 || utf8.RuneCountInString(w.Title) > 150 || !ValidDisplayName(w.Title):
		return nil, invalid("the workshop title must be 2 to 150 characters")
	case utf8.RuneCountInString(w.Room) > 100 || utf8.RuneCountInString(w.Speakers) > 300 || utf8.RuneCountInString(w.Description) > 5000:
		return nil, invalid("the room, speakers or description is too long")
	case w.EndsAt != nil && w.StartsAt == nil:
		return nil, invalid("set a start before an end")
	case w.StartsAt != nil && w.EndsAt != nil && w.EndsAt.Before(*w.StartsAt):
		return nil, invalid("the workshop ends before it starts")
	case w.Capacity != nil && (*w.Capacity < 1 || *w.Capacity > maxCapacity):
		return nil, invalid("capacity must be between 1 and %d", maxCapacity)
	}
	if w.SignupMode == "" {
		w.SignupMode = domain.SignupOptIn
	}
	if !slices.Contains(signupModes, w.SignupMode) {
		return nil, invalid("sign-up must be everyone, opt_in or invite")
	}
	return s.workshops.Save(ctx, es, w)
}

func (s *CheckinService) DeleteWorkshop(ctx context.Context, es authz.EventScope, id string) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	return s.workshops.Delete(ctx, es, id)
}

func (s *CheckinService) Signups(ctx context.Context, es authz.EventScope, workshopID string) ([]domain.WorkshopSignup, error) {
	if err := es.Require(authz.EventDashboard); err != nil {
		return nil, err
	}
	return s.workshops.Signups(ctx, es, workshopID)
}

// CancelSignup removes someone from a workshop (organizers); the waitlist
// moves up.
func (s *CheckinService) CancelSignup(ctx context.Context, es authz.EventScope, workshopID, participantID string) error {
	if err := es.Require(authz.EventEdit); err != nil {
		return err
	}
	return s.workshops.StaffCancel(ctx, es, workshopID, participantID)
}

// ---- Workshops (participant portal) -----------------------------------------------

// MyWorkshops are the workshops of the event behind one of the caller's
// passes, with their own status.
func (s *CheckinService) MyWorkshops(ctx context.Context, as authz.AccountScope, pc *domain.PassContext) ([]domain.Workshop, error) {
	return s.workshops.MyWorkshops(ctx, pc.Event.OrgID, pc.Event.ID, as.AccountID(), pc.Participant.ID)
}

func (s *CheckinService) MySignUp(ctx context.Context, as authz.AccountScope, pc *domain.PassContext, workshopID string) (string, error) {
	if pc.Participant.Status != domain.PartRegistered {
		return "", fmt.Errorf("%w: your registration isn't confirmed yet", domain.ErrConflict)
	}
	return s.workshops.MySignUp(ctx, pc.Event.OrgID, pc.Event.ID, as.AccountID(), workshopID, pc.Participant.ID)
}

func (s *CheckinService) MyCancel(ctx context.Context, as authz.AccountScope, pc *domain.PassContext, workshopID string) error {
	err := s.workshops.MyCancel(ctx, pc.Event.OrgID, pc.Event.ID, as.AccountID(), workshopID, pc.Participant.ID)
	if errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("%w: you aren't signed up for this workshop", domain.ErrConflict)
	}
	return err
}
