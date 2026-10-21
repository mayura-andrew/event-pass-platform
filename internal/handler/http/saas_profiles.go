package httphandler

// Participant profiles and the participant map (beta, M6 demo): the pass
// page's profile card, the map page and its API, and the organizer's Map
// tab with the switch and feedback.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/internal/service"
)

func (h *Handler) registerProfiles(mux *http.ServeMux) {
	mux.Handle("POST /me/passes/{participant}/profile", h.pageAuthed((*Handler).saveProfilePost))
	mux.Handle("GET /me/passes/{participant}/photo/{other}", h.pageAuthed((*Handler).profilePhoto))
	mux.Handle("GET /me/passes/{participant}/map", h.pageAuthed((*Handler).participantMapPage))
	mux.Handle("GET /api/v1/me/passes/{participant}/map", h.apiAuthed((*Handler).apiParticipantMap))
	mux.Handle("PUT /api/v1/me/passes/{participant}/location", h.apiAuthed((*Handler).apiShareLocation))
	mux.Handle("DELETE /api/v1/me/passes/{participant}/location", h.apiAuthed((*Handler).apiStopSharing))
	mux.Handle("POST /api/v1/me/passes/{participant}/feedback", h.apiAuthed((*Handler).apiMapFeedback))
}

func init() {
	eventRoutes = append(eventRoutes, []eventRoute{
		{"GET", evPage + "/map", false, authz.EventEdit, "", (*Handler).eventMapPage},
		{"POST", evPage + "/map", false, authz.EventEdit, "event.participant_map", (*Handler).eventMapPost},
	}...)
}

// ---- Pass page: profile -----------------------------------------------------------

func (h *Handler) saveProfilePost(w http.ResponseWriter, r *http.Request, p *principal) {
	id := r.PathValue("participant")
	in := service.ProfileInput{Headline: r.PostFormValue("headline"), LinkedIn: r.PostFormValue("linkedin"),
		Website: r.PostFormValue("website"), GitHub: r.PostFormValue("github"), Visibility: r.PostFormValue("visibility"),
		RemovePhoto: r.PostFormValue("remove_photo") == "on"}
	if f, hdr, err := r.FormFile("photo"); err == nil {
		defer f.Close()
		if hdr.Size > 0 {
			in.Photo = f
		}
	}
	err := h.Profiles.SaveProfile(r.Context(), p.scope(), id, in)
	var fe service.FieldErrors
	if errors.As(err, &fe) {
		pc, perr := h.Portal.MyPass(r.Context(), p.scope(), id)
		if perr != nil {
			h.renderError(w, r, p, perr)
			return
		}
		h.renderMyPassWith(w, r, p, pc, nil, fe, &in)
		return
	}
	if err != nil {
		h.renderError(w, r, p, err)
		return
	}
	http.Redirect(w, r, "/me/passes/"+id+"?notice=profile#profile", http.StatusSeeOther)
}

func (h *Handler) profilePhoto(w http.ResponseWriter, r *http.Request, p *principal) {
	b, err := h.Profiles.Photo(r.Context(), p.scope(), r.PathValue("participant"), r.PathValue("other"))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrForbidden) {
			http.NotFound(w, r)
			return
		}
		h.renderError(w, r, p, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}

// ---- Map page and API -------------------------------------------------------------

type participantMapData struct {
	Pass            *domain.PassContext
	Tiles           string
	TileAttribution string
	Venue           struct {
		Lat, Lng float64
		Has      bool
	}
	Public bool
}

func (h *Handler) participantMapPage(w http.ResponseWriter, r *http.Request, p *principal) {
	id := r.PathValue("participant")
	v, err := h.Profiles.Map(r.Context(), p.scope(), id)
	if err != nil {
		h.renderError(w, r, p, err)
		return
	}
	pc, err := h.Portal.MyPass(r.Context(), p.scope(), id)
	if err != nil {
		h.renderError(w, r, p, err)
		return
	}
	d := participantMapData{Pass: pc, Public: v.Public, Tiles: h.tileURL(), TileAttribution: h.MapTileAttribution}
	if d.TileAttribution == "" || h.MapTileURL == "" {
		d.TileAttribution = defaultTileAttribution
	}
	if ven := pc.Event.Venue; ven.Lat != nil && ven.Lng != nil {
		d.Venue.Lat, d.Venue.Lng, d.Venue.Has = *ven.Lat, *ven.Lng, true
	}
	h.render(w, http.StatusOK, "participant_map", p, view{Title: "Participant map · " + pc.Event.Name, Data: d})
}

// tileURL is the configured map tile server, or OpenStreetMap's (only
// https URLs: the page is https in production and its CSP allows the host).
func (h *Handler) tileURL() string {
	if strings.HasPrefix(h.MapTileURL, "https://") {
		return h.MapTileURL
	}
	return defaultTileURL
}

func (h *Handler) apiParticipantMap(w http.ResponseWriter, r *http.Request, p *principal) {
	v, err := h.Profiles.Map(r.Context(), p.scope(), r.PathValue("participant"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"me": v.Me, "public": v.Public, "people": v.People,
		"fresh_seconds": int(service.LocationFresh.Seconds())})
}

func (h *Handler) apiShareLocation(w http.ResponseWriter, r *http.Request, p *principal) {
	var req struct {
		Lat      float64 `json:"lat"`
		Lng      float64 `json:"lng"`
		Accuracy float64 `json:"accuracy"`
	}
	if !decodeJSON(w, r, 1<<10, &req) {
		return
	}
	noContent(w, r, h.Profiles.ShareLocation(r.Context(), p.scope(), r.PathValue("participant"), req.Lat, req.Lng, req.Accuracy))
}

func (h *Handler) apiStopSharing(w http.ResponseWriter, r *http.Request, p *principal) {
	noContent(w, r, h.Profiles.StopSharing(r.Context(), p.scope(), r.PathValue("participant")))
}

func (h *Handler) apiMapFeedback(w http.ResponseWriter, r *http.Request, p *principal) {
	var req struct {
		Rating  int    `json:"rating"`
		Comment string `json:"comment"`
	}
	if !decodeJSON(w, r, 8<<10, &req) {
		return
	}
	if err := h.Profiles.Feedback(r.Context(), p.scope(), r.PathValue("participant"), req.Rating, req.Comment); err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"thanks": true})
}

// ---- Organizer: Map tab -----------------------------------------------------------

type eventMapData struct {
	eventPageData
	Stats *domain.MapStats
}

func (h *Handler) eventMapPage(w http.ResponseWriter, r *http.Request, e *eventReq) {
	st, err := h.Profiles.MapStats(r.Context(), e.es)
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	h.render(w, http.StatusOK, "event_map", e.principal, view{Title: "Participant map · " + e.event.Name,
		Notice: map[string]string{"on": "The participant map is on. Registered participants find it on their pass page.",
			"off": "The participant map is off, and every shared location was deleted."}[r.URL.Query().Get("notice")],
		Data: eventMapData{eventPageData: h.eventData(e, "map"), Stats: st}})
}

func (h *Handler) eventMapPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	on := r.PostFormValue("enabled") == "on"
	notice := "off"
	if on {
		notice = "on"
	}
	h.eventAction(w, r, e, "map", notice, h.Profiles.SetMapEnabled(r.Context(), e.es, on))
}
