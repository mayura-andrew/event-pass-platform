package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/internal/passes"
)

// PassService turns a participant into their pass: QR token and card.
type PassService struct{ keys *passes.KeyRing }

func NewPassService(keys *passes.KeyRing) *PassService { return &PassService{keys: keys} }

// Token is what the pass's QR code holds. A delegate imported from the
// current build keeps their old payload, so the pass already in their
// inbox still scans.
func (s *PassService) Token(pc *domain.PassContext) (string, error) {
	p := pc.Participant
	if pc.LegacySig != "" {
		b, err := json.Marshal(passes.Legacy{ID: p.ID, Sig: pc.LegacySig})
		return string(b), err
	}
	return s.keys.Issue(pc.Event.OrgID, pc.Event.ID, p.ID, p.PassID)
}

// Card is the printed content of the pass.
func (s *PassService) Card(pc *domain.PassContext) (passes.Card, error) {
	tok, err := s.Token(pc)
	if err != nil {
		return passes.Card{}, err
	}
	c := passes.Card{OrgName: pc.OrgName, EventName: pc.Event.Name, When: pc.Event.DateRange(), Where: pc.Event.Where(),
		FullName: pc.Participant.FullName, PassID: pc.Participant.PassID, Status: pc.Participant.Status, Token: tok,
		Answers: map[string]string{}}
	// Only answers to questions marked "printed on the pass" reach the card,
	// whether as the answer list or as {{answers.key}} in a design.
	for _, f := range pc.Fields {
		if f.Visibility != "pass" {
			continue
		}
		if v := answerText(pc.Participant.Answers[f.Key]); v != "" {
			c.Lines = append(c.Lines, passes.Line{Label: f.Label, Value: v})
			c.Answers[f.Key] = v
		}
	}
	if len(pc.Design) > 0 {
		var d passes.Design
		if err := json.Unmarshal(pc.Design, &d); err != nil {
			return passes.Card{}, fmt.Errorf("event pass design: %w", err)
		}
		c.Design = &d
	}
	return c, nil
}

// answerText shows a stored answer as text.
func answerText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case bool:
		if x {
			return "Yes"
		}
		return "No"
	case float64:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", x), "0"), ".")
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, answerText(e))
		}
		sort.Strings(parts)
		return strings.Join(parts, ", ")
	case string:
		return x
	}
	return fmt.Sprint(v)
}

func (s *PassService) PNG(pc *domain.PassContext) ([]byte, error) {
	c, err := s.Card(pc)
	if err != nil {
		return nil, err
	}
	return passes.PNG(c)
}

func (s *PassService) PDF(pc *domain.PassContext) ([]byte, error) {
	c, err := s.Card(pc)
	if err != nil {
		return nil, err
	}
	return passes.PDF(c)
}

// PassFilename is a safe download name for a pass.
func PassFilename(passID, ext string) string {
	safe := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return -1
	}, passID)
	return "pass-" + safe + "." + ext
}

// Scanned is a recognised QR payload: a platform token, or a pass from the
// current build.
type Scanned struct {
	Claims *passes.Claims
	Legacy *passes.Legacy
}

// Parse recognises and (for tokens) verifies a scanned QR payload.
func (s *PassService) Parse(payload string) (*Scanned, error) {
	if l, ok := passes.ParseLegacy(payload); ok {
		return &Scanned{Legacy: &l}, nil
	}
	c, err := s.keys.Verify(payload)
	if err != nil {
		return nil, err
	}
	return &Scanned{Claims: c}, nil
}
