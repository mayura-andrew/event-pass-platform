package service

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"sort"
	"strings"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/passes"
)

// PassService turns a participant into their pass: QR token and card.
type PassService struct {
	keys *passes.KeyRing
	// Assets loads uploaded images for designs that show them (nil: they
	// draw as placeholders).
	Assets AssetLoader
	images *passes.ImageCache
}

// AssetLoader reads an org's pass images (postgres.PassAssetRepo).
type AssetLoader interface {
	LoadAssets(ctx context.Context, orgID string, ids []string) (map[string][]byte, error)
}

func NewPassService(keys *passes.KeyRing) *PassService {
	return &PassService{keys: keys, images: passes.NewImageCache(64)}
}

// LoadImages fills in the images a design shows. Decoded images are cached:
// assets never change.
func (s *PassService) LoadImages(ctx context.Context, orgID string, d *passes.Design) error {
	ids := passes.AssetIDs(*d)
	if len(ids) == 0 || s.Assets == nil {
		return nil
	}
	d.Images = map[string]image.Image{}
	var missing []string
	for _, id := range ids {
		if img, ok := s.images.Get(orgID + "/" + id); ok {
			d.Images[id] = img
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	data, err := s.Assets.LoadAssets(ctx, orgID, missing)
	if err != nil {
		return fmt.Errorf("pass images: %w", err)
	}
	for id, b := range data {
		img, err := s.images.Add(orgID+"/"+id, b)
		if err != nil {
			return fmt.Errorf("pass image %s: %w", id, err)
		}
		d.Images[id] = img
	}
	return nil
}

// Token is what the pass's QR code holds.
func (s *PassService) Token(pc *domain.PassContext) (string, error) {
	p := pc.Participant
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
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.LoadImages(ctx, pc.Event.OrgID, &d); err != nil {
			return passes.Card{}, err
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

// Scanned is a recognised platform QR payload.
type Scanned struct {
	Claims *passes.Claims
}

// Parse recognises and verifies a scanned QR payload.
func (s *PassService) Parse(payload string) (*Scanned, error) {
	c, err := s.keys.Verify(payload)
	if err != nil {
		return nil, err
	}
	return &Scanned{Claims: c}, nil
}
