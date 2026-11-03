package httphandler

// Pass templates and the designer (milestone M5).

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/passes"
	"github.com/mayura-andrew/event-pass-platform/internal/service"
)

const tpl = "/api/v1/orgs/{org}/pass-templates"

func init() {
	orgRoutes = append(orgRoutes, []orgRoute{
		{"GET", "/api/v1/orgs/{org}/pass-gallery", true, (*Handler).apiGallery},
		{"GET", tpl, true, (*Handler).apiTemplates},
		{"POST", tpl, true, (*Handler).apiCreateTemplate},
		{"GET", tpl + "/{template}", true, (*Handler).apiTemplate},
		{"PUT", tpl + "/{template}", true, (*Handler).apiSaveTemplate},
		{"POST", tpl + "/{template}/archive", true, (*Handler).apiArchiveTemplate},
		{"POST", tpl + "/{template}/restore", true, (*Handler).apiRestoreTemplate},
		{"POST", tpl + "/{template}/preview", true, (*Handler).apiPreviewTemplate},
		{"POST", tpl + "/{template}/check", true, (*Handler).apiCheckTemplate},
		{"POST", "/api/v1/orgs/{org}/pass-assets", true, (*Handler).apiUploadAsset},
		{"GET", "/api/v1/orgs/{org}/pass-assets/{asset}", true, (*Handler).apiAsset},

		{"GET", "/o/{org}/pass-templates", false, (*Handler).templatesPage},
		{"POST", "/o/{org}/pass-templates", false, (*Handler).createTemplatePost},
		{"POST", "/o/{org}/pass-templates/upload", false, (*Handler).uploadTemplatePost},
		{"GET", "/o/{org}/pass-templates/{template}", false, (*Handler).designerPage},
		{"POST", "/o/{org}/pass-templates/{template}/archive", false, (*Handler).archiveTemplatePost},
		{"POST", "/o/{org}/pass-templates/{template}/restore", false, (*Handler).restoreTemplatePost},
	}...)
	eventRoutes = append(eventRoutes, []eventRoute{
		{"GET", ev + "/pass", true, authz.EventView, "", (*Handler).apiEventPass},
		{"PUT", ev + "/pass", true, authz.EventDesignPass, "event.pass_template", (*Handler).apiSetEventPass},
		{"POST", ev + "/pass/resend", true, authz.EventEmail, "event.passes.resend", (*Handler).apiResendPasses},
		{"POST", ev + "/pass/preview", true, authz.EventDesignPass, "", (*Handler).apiEventPassPreview},
		{"GET", evPage + "/pass", false, authz.EventDesignPass, "", (*Handler).eventPassPage},
		{"GET", evPage + "/pass/preview.png", false, authz.EventDesignPass, "", (*Handler).eventPassPreviewPNG},
		{"POST", evPage + "/pass", false, authz.EventDesignPass, "event.pass_template", (*Handler).setEventPassPost},
		{"POST", evPage + "/pass/resend", false, authz.EventEmail, "event.passes.resend", (*Handler).resendPassesPost},
	}...)
}

// writeDesignError reports failed scan checks with their list (422), other
// errors as usual.
func writeDesignError(w http.ResponseWriter, r *http.Request, err error) {
	var ps service.DesignProblems
	if errors.As(err, &ps) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the design fails its scan checks", "problems": ps})
		return
	}
	writeDomainError(w, r, err)
}

func sendPNG(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

// =============================================================================
// JSON API
// =============================================================================

type galleryItem struct {
	Key         string        `json:"key"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Design      passes.Design `json:"design"`
}

func (h *Handler) apiGallery(w http.ResponseWriter, r *http.Request, o *orgReq) {
	var out []galleryItem
	for _, g := range passes.Gallery() {
		out = append(out, galleryItem{g.Key, g.Name, g.Description, g.Design})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) apiTemplates(w http.ResponseWriter, r *http.Request, o *orgReq) {
	list, err := h.Templates.List(r.Context(), o.sc)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) apiCreateTemplate(w http.ResponseWriter, r *http.Request, o *orgReq) {
	var req struct{ Name, From string }
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	t, err := h.Templates.Create(r.Context(), o.sc, req.Name, req.From)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (h *Handler) apiTemplate(w http.ResponseWriter, r *http.Request, o *orgReq) {
	t, err := h.Templates.Get(r.Context(), o.sc, r.PathValue("template"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// PUT {name, design}: saves a new version, or 422 with the failed checks.
func (h *Handler) apiSaveTemplate(w http.ResponseWriter, r *http.Request, o *orgReq) {
	var req struct {
		Name   string          `json:"name"`
		Design json.RawMessage `json:"design"`
	}
	if !decodeJSON(w, r, 128<<10, &req) {
		return
	}
	v, err := h.Templates.Save(r.Context(), o.sc, r.PathValue("template"), req.Name, req.Design)
	if err != nil {
		writeDesignError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"version": v})
}

func (h *Handler) apiArchiveTemplate(w http.ResponseWriter, r *http.Request, o *orgReq) {
	noContent(w, r, h.Templates.SetArchived(r.Context(), o.sc, r.PathValue("template"), true))
}

func (h *Handler) apiRestoreTemplate(w http.ResponseWriter, r *http.Request, o *orgReq) {
	noContent(w, r, h.Templates.SetArchived(r.Context(), o.sc, r.PathValue("template"), false))
}

// POST …/preview?sample=short|long|nonlatin|cancelled {design}: the PNG the
// design produces, through the same renderer as real passes.
func (h *Handler) apiPreviewTemplate(w http.ResponseWriter, r *http.Request, o *orgReq) {
	if _, err := h.Templates.Get(r.Context(), o.sc, r.PathValue("template")); err != nil {
		writeDomainError(w, r, err)
		return
	}
	var req struct {
		Design json.RawMessage `json:"design"`
	}
	if !decodeJSON(w, r, 128<<10, &req) {
		return
	}
	b, err := h.Templates.Preview(r.Context(), o.org.ID, req.Design, r.URL.Query().Get("sample"), nil)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	sendPNG(w, b)
}

func (h *Handler) apiCheckTemplate(w http.ResponseWriter, r *http.Request, o *orgReq) {
	if _, err := h.Templates.Get(r.Context(), o.sc, r.PathValue("template")); err != nil {
		writeDomainError(w, r, err)
		return
	}
	var req struct {
		Design json.RawMessage `json:"design"`
	}
	if !decodeJSON(w, r, 128<<10, &req) {
		return
	}
	ps := h.Templates.Check(req.Design)
	if ps == nil {
		ps = []passes.Problem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"problems": ps})
}

type eventPassInfo struct {
	TemplateID string          `json:"template_id"`
	Version    int             `json:"version"`
	Design     json.RawMessage `json:"design"`
}

func (h *Handler) apiEventPass(w http.ResponseWriter, r *http.Request, e *eventReq) {
	d, err := h.Templates.EventDesign(r.Context(), e.es, e.event)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	info := eventPassInfo{Design: d}
	if e.event.PassTemplateID != nil {
		info.TemplateID, info.Version = *e.event.PassTemplateID, *e.event.PassTemplateVersion
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *Handler) apiSetEventPass(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var req struct {
		TemplateID string `json:"template_id"`
		Version    int    `json:"version"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	noContent(w, r, h.Templates.SetEventTemplate(r.Context(), e.es, req.TemplateID, req.Version))
}

func (h *Handler) apiResendPasses(w http.ResponseWriter, r *http.Request, e *eventReq) {
	n, err := h.Templates.ResendPasses(r.Context(), e.es)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"queued": n})
}

// POST …/pass/preview {design?, participant_id?, sample?}: the event's (or
// an unsaved) design for one of its real participants or a sample.
func (h *Handler) apiEventPassPreview(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var req struct {
		Design        json.RawMessage `json:"design"`
		ParticipantID string          `json:"participant_id"`
		Sample        string          `json:"sample"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, 128<<10, &req) {
		return
	}
	b, err := h.eventPreview(r, e, req.Design, req.ParticipantID, req.Sample)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	sendPNG(w, b)
}

func (h *Handler) eventPreview(r *http.Request, e *eventReq, design json.RawMessage, participantID, sample string) ([]byte, error) {
	if len(design) == 0 {
		var err error
		if design, err = h.Templates.EventDesign(r.Context(), e.es, e.event); err != nil {
			return nil, err
		}
	}
	var pc *domain.PassContext
	if participantID != "" {
		var err error
		if pc, err = h.Registrations.PassFor(r.Context(), e.es, participantID); err != nil {
			return nil, err
		}
	}
	return h.Templates.Preview(r.Context(), e.event.OrgID, design, sample, pc)
}

// ---- Uploaded images -------------------------------------------------------------

// uploadedFile opens the multipart "file" field, within the upload limit.
func uploadedFile(w http.ResponseWriter, r *http.Request) (multipart.File, error) {
	r.Body = http.MaxBytesReader(w, r.Body, passes.MaxUploadBytes+1<<20)
	f, _, err := r.FormFile("file")
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		return nil, fmt.Errorf("%w: the image is over %d MB", domain.ErrValidation, passes.MaxUploadBytes>>20)
	case err != nil:
		return nil, fmt.Errorf("%w: choose an image to upload", domain.ErrValidation)
	}
	return f, nil
}

// POST /api/v1/orgs/{org}/pass-assets (multipart "file"): stores an image
// and returns its "asset:<id>" for an image element.
func (h *Handler) apiUploadAsset(w http.ResponseWriter, r *http.Request, o *orgReq) {
	f, err := uploadedFile(w, r)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	defer f.Close()
	a, err := h.Templates.UploadAsset(r.Context(), o.sc, f)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// GET /api/v1/orgs/{org}/pass-assets/{asset}: the stored image. Assets
// never change, so it may be cached (privately: it's the org's).
func (h *Handler) apiAsset(w http.ResponseWriter, r *http.Request, o *orgReq) {
	a, data, err := h.Templates.Asset(r.Context(), o.sc, r.PathValue("asset"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", a.ContentType)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

// =============================================================================
// Pages
// =============================================================================

type templatesData struct {
	orgPageData
	Templates []domain.PassTemplate
	Gallery   []passes.GalleryTemplate
	CanManage bool
}

func (h *Handler) templatesPage(w http.ResponseWriter, r *http.Request, o *orgReq) {
	h.renderTemplates(w, r, o, http.StatusOK, "")
}

func (h *Handler) renderTemplates(w http.ResponseWriter, r *http.Request, o *orgReq, status int, errMsg string) {
	list, err := h.Templates.List(r.Context(), o.sc)
	if err != nil {
		h.renderError(w, r, o.principal, err)
		return
	}
	d := templatesData{orgPageData: orgPageData{Org: o.org, Tab: "templates", sc: o.sc}, Templates: list,
		Gallery: passes.Gallery(), CanManage: o.sc.Can(authz.ManageTemplates)}
	h.render(w, status, "pass_templates", o.principal, view{Title: "Pass templates · " + o.org.Name, Error: errMsg,
		Notice: map[string]string{"archived": "Template archived. Events using it keep their version."}[r.URL.Query().Get("notice")], Data: d})
}

func (h *Handler) createTemplatePost(w http.ResponseWriter, r *http.Request, o *orgReq) {
	t, err := h.Templates.Create(r.Context(), o.sc, r.PostFormValue("name"), r.PostFormValue("from"))
	if err != nil {
		status, msg := pageErrorStatus(err)
		if status == http.StatusInternalServerError || status == http.StatusNotFound {
			h.renderError(w, r, o.principal, err)
			return
		}
		h.renderTemplates(w, r, o, status, msg)
		return
	}
	http.Redirect(w, r, "/o/"+o.org.Slug+"/pass-templates/"+t.ID, http.StatusSeeOther)
}

// POST /o/{org}/pass-templates/upload (multipart: name, file): a template
// from the organizer's own artwork, opened in the designer to place the
// name and QR code.
func (h *Handler) uploadTemplatePost(w http.ResponseWriter, r *http.Request, o *orgReq) {
	f, err := uploadedFile(w, r)
	if err == nil {
		defer f.Close()
		var t *domain.PassTemplate
		if t, err = h.Templates.CreateFromArtwork(r.Context(), o.sc, r.PostFormValue("name"), f); err == nil {
			http.Redirect(w, r, "/o/"+o.org.Slug+"/pass-templates/"+t.ID+"?notice=artwork", http.StatusSeeOther)
			return
		}
	}
	status, msg := pageErrorStatus(err)
	if status == http.StatusInternalServerError || status == http.StatusNotFound {
		h.renderError(w, r, o.principal, err)
		return
	}
	h.renderTemplates(w, r, o, status, msg)
}

type designerData struct {
	orgPageData
	Template   *domain.PassTemplate
	DesignJSON string
	APIBase    string
	Samples    []string
	Variables  []string
	CanManage  bool
}

func (h *Handler) designerPage(w http.ResponseWriter, r *http.Request, o *orgReq) {
	t, err := h.Templates.Get(r.Context(), o.sc, r.PathValue("template"))
	if err != nil {
		h.renderError(w, r, o.principal, err)
		return
	}
	d := designerData{orgPageData: orgPageData{Org: o.org, Tab: "templates", sc: o.sc}, Template: t,
		DesignJSON: string(t.Design), APIBase: "/api/v1/orgs/" + o.org.Slug + "/pass-templates/" + t.ID,
		Samples: passes.SampleOrder, Variables: passes.Variables, CanManage: o.sc.Can(authz.ManageTemplates)}
	h.render(w, http.StatusOK, "designer", o.principal, view{Title: t.Name + " · Designer", Data: d,
		Notice: map[string]string{"artwork": "Your artwork is in. Move the name, QR code and pass ID to where they belong, " +
			"remove the white panels if your artwork leaves room, then save."}[r.URL.Query().Get("notice")]})
}

func (h *Handler) archiveTemplatePost(w http.ResponseWriter, r *http.Request, o *orgReq) {
	if err := h.Templates.SetArchived(r.Context(), o.sc, r.PathValue("template"), true); err != nil {
		h.renderError(w, r, o.principal, err)
		return
	}
	http.Redirect(w, r, "/o/"+o.org.Slug+"/pass-templates?notice=archived", http.StatusSeeOther)
}

func (h *Handler) restoreTemplatePost(w http.ResponseWriter, r *http.Request, o *orgReq) {
	if err := h.Templates.SetArchived(r.Context(), o.sc, r.PathValue("template"), false); err != nil {
		h.renderError(w, r, o.principal, err)
		return
	}
	http.Redirect(w, r, "/o/"+o.org.Slug+"/pass-templates", http.StatusSeeOther)
}

type eventPassData struct {
	eventPageData
	Templates    []domain.PassTemplate
	Current      *domain.PassTemplate // nil: Classic
	Participants []domain.EventParticipant
	Registered   int
	PreviewWith  string
	CanEmail     bool
	CanDesign    bool
}

var eventPassNotices = map[string]string{"set": "Pass design updated. New passes use it; re-send to update ones already sent.",
	"resent": "Passes queued: everyone registered gets theirs again in the next minutes."}

func (h *Handler) eventPassPage(w http.ResponseWriter, r *http.Request, e *eventReq) {
	list, err := h.Templates.List(r.Context(), e.sc)
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	d := eventPassData{eventPageData: h.eventData(e, "pass"), Templates: list, PreviewWith: r.URL.Query().Get("participant"),
		CanEmail: e.es.Can(authz.EventEmail), CanDesign: e.sc.Can(authz.ManageTemplates)}
	for i := range list {
		if e.event.PassTemplateID != nil && list[i].ID == *e.event.PassTemplateID {
			d.Current = &list[i]
		}
	}
	if ps, counts, err := h.Registrations.Participants(r.Context(), e.es, domain.ParticipantQuery{Status: domain.PartRegistered}); err == nil {
		d.Participants, d.Registered = ps, counts[domain.PartRegistered]
		if len(d.Participants) > 50 {
			d.Participants = d.Participants[:50]
		}
	}
	h.render(w, http.StatusOK, "event_pass", e.principal, view{Title: "Pass · " + e.event.Name,
		Notice: eventPassNotices[r.URL.Query().Get("notice")], Data: d})
}

func (h *Handler) eventPassPreviewPNG(w http.ResponseWriter, r *http.Request, e *eventReq) {
	b, err := h.eventPreview(r, e, nil, r.URL.Query().Get("participant"), r.URL.Query().Get("sample"))
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	sendPNG(w, b)
}

func (h *Handler) setEventPassPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	version, _ := strconv.Atoi(r.PostFormValue("version"))
	h.eventAction(w, r, e, "pass", "set", h.Templates.SetEventTemplate(r.Context(), e.es, r.PostFormValue("template_id"), version))
}

func (h *Handler) resendPassesPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	_, err := h.Templates.ResendPasses(r.Context(), e.es)
	h.eventAction(w, r, e, "pass", "resent", err)
}
