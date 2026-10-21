package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/internal/passes"
)

// PassTemplateStore persists templates (postgres.PassTemplateRepo).
type PassTemplateStore interface {
	List(ctx context.Context, sc authz.OrgScope) ([]domain.PassTemplate, error)
	Get(ctx context.Context, sc authz.OrgScope, id string) (*domain.PassTemplate, error)
	Create(ctx context.Context, sc authz.OrgScope, name string, design json.RawMessage) (*domain.PassTemplate, error)
	SaveVersion(ctx context.Context, sc authz.OrgScope, id, name string, design json.RawMessage) (int, error)
	SetArchived(ctx context.Context, sc authz.OrgScope, id string, archived bool) error
	VersionDesign(ctx context.Context, sc authz.OrgScope, id string, version int) (json.RawMessage, error)
	SetEventTemplate(ctx context.Context, es authz.EventScope, templateID string, version int) error
	QueuePasses(ctx context.Context, es authz.EventScope) (int, error)
}

// PassAssetStore keeps uploaded images (postgres.PassAssetRepo).
type PassAssetStore interface {
	Save(ctx context.Context, sc authz.OrgScope, a *passes.Asset) (*domain.PassAsset, error)
	Get(ctx context.Context, sc authz.OrgScope, id string) (*domain.PassAsset, []byte, error)
	LoadAssets(ctx context.Context, orgID string, ids []string) (map[string][]byte, error)
}

type PassTemplateService struct {
	store  PassTemplateStore
	assets PassAssetStore
	passes *PassService
}

func NewPassTemplateService(store PassTemplateStore, assets PassAssetStore, passes *PassService) *PassTemplateService {
	return &PassTemplateService{store: store, assets: assets, passes: passes}
}

// DesignProblems are failed scan checks; saving is refused (a validation
// error the API reports with the list).
type DesignProblems []passes.Problem

func (p DesignProblems) Error() string {
	msgs := make([]string, len(p))
	for i, x := range p {
		msgs[i] = x.Message
	}
	return domain.ErrValidation.Error() + ": " + strings.Join(msgs, "; ")
}

func (p DesignProblems) Unwrap() error { return domain.ErrValidation }

// parseDesign reads a design and runs the scan checks.
func parseDesign(raw json.RawMessage) (passes.Design, json.RawMessage, error) {
	var d passes.Design
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, nil, invalid("the design isn't valid: %v", err)
	}
	if ps := passes.Check(d); len(ps) > 0 {
		return d, nil, DesignProblems(ps)
	}
	clean, _ := json.Marshal(d) // stored normalized
	return d, clean, nil
}

func templateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 2 || n > 100 || !ValidDisplayName(name) {
		return "", invalid("the template name must be 2 to 100 characters")
	}
	return name, nil
}

func (s *PassTemplateService) List(ctx context.Context, sc authz.OrgScope) ([]domain.PassTemplate, error) {
	if err := sc.Require(authz.ViewOrg); err != nil {
		return nil, err
	}
	if sc.Role() == domain.OrgRoleGuest {
		return nil, domain.ErrForbidden
	}
	return s.store.List(ctx, sc)
}

func (s *PassTemplateService) Get(ctx context.Context, sc authz.OrgScope, id string) (*domain.PassTemplate, error) {
	if _, err := s.List(ctx, sc); err != nil { // same access rule
		return nil, err
	}
	return s.store.Get(ctx, sc, id)
}

// Create starts a template from a gallery design (from = its key) or a copy
// of one of the org's templates (from = its ID).
func (s *PassTemplateService) Create(ctx context.Context, sc authz.OrgScope, name, from string) (*domain.PassTemplate, error) {
	if err := sc.Require(authz.ManageTemplates); err != nil {
		return nil, err
	}
	var design json.RawMessage
	for _, g := range passes.Gallery() {
		if g.Key == from || (from == "" && g.Key == "classic") {
			design, _ = json.Marshal(g.Design)
			if name == "" {
				name = g.Name
			}
		}
	}
	if design == nil {
		src, err := s.store.Get(ctx, sc, from)
		if err != nil {
			return nil, err
		}
		design = src.Design
		if name == "" {
			name = "Copy of " + src.Name
		}
	}
	name, err := templateName(name)
	if err != nil {
		return nil, err
	}
	return s.store.Create(ctx, sc, name, design)
}

// Save checks a design and stores it as the template's next version.
func (s *PassTemplateService) Save(ctx context.Context, sc authz.OrgScope, id, name string, design json.RawMessage) (int, error) {
	if err := sc.Require(authz.ManageTemplates); err != nil {
		return 0, err
	}
	name, err := templateName(name)
	if err != nil {
		return 0, err
	}
	d, clean, err := parseDesign(design)
	if err != nil {
		return 0, err
	}
	if err := s.checkAssets(ctx, sc, d); err != nil {
		return 0, err
	}
	return s.store.SaveVersion(ctx, sc, id, name, clean)
}

func (s *PassTemplateService) SetArchived(ctx context.Context, sc authz.OrgScope, id string, archived bool) error {
	if err := sc.Require(authz.ManageTemplates); err != nil {
		return err
	}
	return s.store.SetArchived(ctx, sc, id, archived)
}

// Check runs the scan checks without saving (the designer shows them live).
func (s *PassTemplateService) Check(design json.RawMessage) []passes.Problem {
	_, _, err := parseDesign(design)
	if ps, ok := err.(DesignProblems); ok {
		return ps
	}
	if err != nil {
		return []passes.Problem{{Message: strings.TrimPrefix(err.Error(), domain.ErrValidation.Error()+": ")}}
	}
	return nil
}

// checkAssets makes sure every image a design shows was uploaded to this
// org: a design can't borrow another org's images by ID.
func (s *PassTemplateService) checkAssets(ctx context.Context, sc authz.OrgScope, d passes.Design) error {
	ids := passes.AssetIDs(d)
	if len(ids) == 0 {
		return nil
	}
	found, err := s.assets.LoadAssets(ctx, sc.OrgID(), ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			return DesignProblems{{Message: "an image in this design wasn't uploaded to this organization; upload it again"}}
		}
	}
	return nil
}

// Preview renders an (unsaved) design as PNG for a sample participant, or
// for a real participant's pass context. It uses the same renderer as the
// passes people receive, so what the designer shows is what they get.
func (s *PassTemplateService) Preview(ctx context.Context, orgID string, design json.RawMessage, sample string, pc *domain.PassContext) ([]byte, error) {
	var d passes.Design
	if err := json.Unmarshal(design, &d); err != nil {
		return nil, invalid("the design isn't valid: %v", err)
	}
	if err := s.passes.LoadImages(ctx, orgID, &d); err != nil {
		return nil, err
	}
	var v passes.Vars
	if pc != nil {
		card, err := s.passes.Card(pc)
		if err != nil {
			return nil, err
		}
		v = passes.CardVars(card)
	} else {
		samples := passes.Samples(sampleToken())
		var ok bool
		if v, ok = samples[sample]; !ok {
			v = samples["short"]
		}
	}
	img, _, err := passes.RenderDesign(d, v)
	if err != nil {
		return nil, invalid("%v", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sampleToken is a real-format token for previews (it verifies nowhere).
func sampleToken() string {
	tok, _ := passes.DerivedKeyRing([]byte("preview")).Issue("00000000-0000-7000-8000-000000000001",
		"00000000-0000-7000-8000-000000000002", "00000000-0000-7000-8000-000000000003", "CSA-7KQ4-M2XD")
	return tok
}

// ---- Uploaded images --------------------------------------------------------------

// UploadAsset stores an image for designs (artwork, a logo).
func (s *PassTemplateService) UploadAsset(ctx context.Context, sc authz.OrgScope, r io.Reader) (*domain.PassAsset, error) {
	if err := sc.Require(authz.ManageTemplates); err != nil {
		return nil, err
	}
	a, err := passes.PrepareUpload(r)
	if errors.Is(err, passes.ErrNotImage) {
		return nil, invalid("%s; export PDF or SVG artwork as PNG first", err)
	}
	if err != nil {
		return nil, invalid("%v", err)
	}
	return s.assets.Save(ctx, sc, a)
}

// Asset returns an uploaded image, for anyone who can see the templates.
func (s *PassTemplateService) Asset(ctx context.Context, sc authz.OrgScope, id string) (*domain.PassAsset, []byte, error) {
	if _, err := s.List(ctx, sc); err != nil { // same access rule
		return nil, nil, err
	}
	return s.assets.Get(ctx, sc, id)
}

// CreateFromArtwork makes a template from an uploaded design: the artwork
// fills the pass and the name, QR code and pass ID go on top, ready to be
// arranged in the designer.
func (s *PassTemplateService) CreateFromArtwork(ctx context.Context, sc authz.OrgScope, name string, r io.Reader) (*domain.PassTemplate, error) {
	name, err := templateName(name)
	if err != nil {
		return nil, err
	}
	a, err := s.UploadAsset(ctx, sc, r)
	if err != nil {
		return nil, err
	}
	design, _ := json.Marshal(passes.ArtworkDesign(a.ID, a.Width, a.Height))
	if _, _, err := parseDesign(design); err != nil {
		return nil, err
	}
	return s.store.Create(ctx, sc, name, design)
}

// ---- Events ----------------------------------------------------------------------

// SetEventTemplate pins the event to a template version (0 = the current
// one), or back to Classic with templateID "".
func (s *PassTemplateService) SetEventTemplate(ctx context.Context, es authz.EventScope, templateID string, version int) error {
	if err := es.Require(authz.EventDesignPass); err != nil {
		return err
	}
	if templateID != "" {
		t, err := s.store.Get(ctx, es.Org(), templateID)
		if err != nil {
			return err
		}
		if version == 0 {
			version = t.CurrentVersion
		}
		if version < 1 || version > t.CurrentVersion {
			return invalid("%s has versions 1 to %d", t.Name, t.CurrentVersion)
		}
	}
	return s.store.SetEventTemplate(ctx, es, templateID, version)
}

// ResendPasses emails every registered participant their pass again (after a
// design change).
func (s *PassTemplateService) ResendPasses(ctx context.Context, es authz.EventScope) (int, error) {
	if err := es.Require(authz.EventEmail); err != nil {
		return 0, err
	}
	return s.store.QueuePasses(ctx, es)
}

// EventDesign is the event's design, as JSON, for previews.
func (s *PassTemplateService) EventDesign(ctx context.Context, es authz.EventScope, ev *domain.Event) (json.RawMessage, error) {
	if ev.PassTemplateID == nil {
		return json.Marshal(passes.Classic())
	}
	return s.store.VersionDesign(ctx, es.Org(), *ev.PassTemplateID, *ev.PassTemplateVersion)
}
