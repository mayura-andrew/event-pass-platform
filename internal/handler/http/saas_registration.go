package httphandler

// Publishing, the registration form, the public event page and the
// organizer's participant list (milestone M3).

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/internal/service"
)

// Registration routes join eventRoutes, so the wrapper enforces their action
// and the authorization matrix test covers them.
func init() {
	eventRoutes = append(eventRoutes, []eventRoute{
		{"GET", ev + "/fields", true, authz.EventEdit, "", (*Handler).apiListFields},
		{"POST", ev + "/fields", true, authz.EventEdit, "form.field.add", (*Handler).apiAddField},
		{"PATCH", ev + "/fields/{field}", true, authz.EventEdit, "form.field.update", (*Handler).apiUpdateField},
		{"DELETE", ev + "/fields/{field}", true, authz.EventEdit, "form.field.remove", (*Handler).apiDeleteField},
		{"PATCH", ev + "/registration", true, authz.EventEdit, "event.registration", (*Handler).apiRegistrationSettings},
		{"POST", ev + "/publish", true, authz.EventEdit, "event.publish", (*Handler).apiPublish},
		{"POST", ev + "/unpublish", true, authz.EventEdit, "event.unpublish", (*Handler).apiUnpublish},
		{"GET", ev + "/participants", true, authz.EventDashboard, "", (*Handler).apiListParticipants},
		{"PATCH", ev + "/participants/{participant}", true, authz.EventEdit, "participant.status", (*Handler).apiSetParticipantStatus},

		{"GET", evPage + "/registration", false, authz.EventEdit, "", (*Handler).registrationPage},
		{"POST", evPage + "/registration", false, authz.EventEdit, "event.registration", (*Handler).registrationPost},
		{"POST", evPage + "/fields", false, authz.EventEdit, "form.field.add", (*Handler).addFieldPost},
		{"POST", evPage + "/fields/{field}", false, authz.EventEdit, "form.field.update", (*Handler).updateFieldPost},
		{"POST", evPage + "/fields/{field}/remove", false, authz.EventEdit, "form.field.remove", (*Handler).removeFieldPost},
		{"POST", evPage + "/fields/{field}/up", false, authz.EventEdit, "", (*Handler).fieldUpPost},
		{"POST", evPage + "/fields/{field}/down", false, authz.EventEdit, "", (*Handler).fieldDownPost},
		{"POST", evPage + "/publish", false, authz.EventEdit, "event.publish", (*Handler).publishPost},
		{"POST", evPage + "/unpublish", false, authz.EventEdit, "event.unpublish", (*Handler).unpublishPost},
		{"GET", evPage + "/participants", false, authz.EventDashboard, "", (*Handler).participantsPage},
		{"POST", evPage + "/participants/{participant}/status", false, authz.EventEdit, "participant.status", (*Handler).participantStatusPost},
		{"POST", ev + "/passes/check", true, authz.EventLookup, "", (*Handler).apiCheckPass},
		{"GET", evPage + "/participants/{participant}/pass.png", false, authz.EventDashboard, "", (*Handler).participantPassPNG},
		{"GET", evPage + "/participants/{participant}/pass.pdf", false, authz.EventDashboard, "", (*Handler).participantPassPDF},
	}...)
}

// =============================================================================
// JSON API
// =============================================================================

func (h *Handler) apiListFields(w http.ResponseWriter, r *http.Request, e *eventReq) {
	fields, err := h.Registrations.Fields(r.Context(), e.es)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, fields)
}

func (h *Handler) apiAddField(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var f domain.FormField
	if !decodeJSON(w, r, 16<<10, &f) {
		return
	}
	f.ID = ""
	out, err := h.Registrations.SaveField(r.Context(), e.es, f)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) apiUpdateField(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var f domain.FormField
	if !decodeJSON(w, r, 16<<10, &f) {
		return
	}
	f.ID = r.PathValue("field")
	out, err := h.Registrations.SaveField(r.Context(), e.es, f)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) apiDeleteField(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Registrations.DeleteField(r.Context(), e.es, r.PathValue("field")))
}

func (h *Handler) apiRegistrationSettings(w http.ResponseWriter, r *http.Request, e *eventReq) {
	st := registrationSettings(e.event)
	if !decodeJSON(w, r, 4<<10, &st) {
		return
	}
	noContent(w, r, h.Registrations.UpdateSettings(r.Context(), e.es, st))
}

func registrationSettings(ev *domain.Event) domain.RegistrationSettings {
	return domain.RegistrationSettings{Mode: ev.RegMode, OpensAt: ev.RegOpensAt, ClosesAt: ev.RegClosesAt,
		Waitlist: ev.Waitlist, PassPrefix: ev.PassPrefix, AgeOn: ev.AgeOn}
}

func (h *Handler) apiPublish(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Registrations.SetPublished(r.Context(), e.es, e.event, true))
}

func (h *Handler) apiUnpublish(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Registrations.SetPublished(r.Context(), e.es, e.event, false))
}

// participantQuery reads ?status= (a status, or "minors" for people under
// 18 on the event's cut-off date) and ?q=.
func participantQuery(r *http.Request) domain.ParticipantQuery {
	q := domain.ParticipantQuery{Status: r.URL.Query().Get("status"), Q: r.URL.Query().Get("q")}
	if q.Status == domain.CountMinors {
		q.Status, q.Minors = "", true
	}
	return q
}

func (h *Handler) apiListParticipants(w http.ResponseWriter, r *http.Request, e *eventReq) {
	list, counts, err := h.Registrations.Participants(r.Context(), e.es, participantQuery(r))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"participants": list, "counts": counts})
}

func (h *Handler) apiSetParticipantStatus(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var req struct {
		Status string `json:"status"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	noContent(w, r, h.Registrations.SetStatus(r.Context(), e.es, r.PathValue("participant"), req.Status))
}

// =============================================================================
// Organizer pages
// =============================================================================

type registrationData struct {
	eventPageData
	Fields     []domain.FormField
	FieldTypes []viewLink
	TypeLabels map[string]string
	PublicURL  string
	OpensAt    string // datetime-local in the event's zone
	ClosesAt   string
	AgeOn      string // YYYY-MM-DD, "" for the start date
	// AgeDefault is the start date used when AgeOn is empty ("" without dates).
	AgeDefault  string
	HasBirthDay bool // the form asks for a birth date
}

func localValue(t *time.Time, tz string) string {
	if t == nil {
		return ""
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02T15:04")
}

var registrationNotices = map[string]string{
	"saved":       "Registration settings saved.",
	"added":       "Question added.",
	"updated":     "Question saved.",
	"removed":     "Question removed.",
	"published":   "Published. The public page and registration form are live.",
	"unpublished": "Back to draft. The public page is offline.",
}

func (h *Handler) registrationPage(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.renderRegistration(w, r, e, http.StatusOK, "", registrationNotices[r.URL.Query().Get("notice")])
}

func (h *Handler) renderRegistration(w http.ResponseWriter, r *http.Request, e *eventReq, status int, errMsg, notice string) {
	fields, err := h.Registrations.Fields(r.Context(), e.es)
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	d := registrationData{eventPageData: h.eventData(e, "registration"), Fields: fields, TypeLabels: domain.FieldTypeLabels,
		OpensAt: localValue(e.event.RegOpensAt, e.event.TimeZone), ClosesAt: localValue(e.event.RegClosesAt, e.event.TimeZone)}
	if e.event.AgeOn != nil {
		d.AgeOn = e.event.AgeOn.Format("2006-01-02")
	}
	if e.event.StartsAt != nil {
		d.AgeDefault = service.AgeOn(&domain.Event{StartsAt: e.event.StartsAt, TimeZone: e.event.TimeZone}).Format("2 Jan 2006")
	}
	for _, f := range fields {
		d.HasBirthDay = d.HasBirthDay || f.Type == domain.FieldBirthDate
	}
	for _, t := range domain.FieldTypes {
		d.FieldTypes = append(d.FieldTypes, viewLink{t, domain.FieldTypeLabels[t]})
	}
	if e.event.Visibility != "private" {
		d.PublicURL = h.BaseURL + "/e/" + e.org.Slug + "/" + e.event.Slug
	}
	h.render(w, status, "event_registration", e.principal, view{Title: "Registration · " + e.event.Name,
		Error: errMsg, Notice: notice, Data: d})
}

// registrationResult goes back to the Registration tab with a notice, or
// shows it again with the error.
func (h *Handler) registrationResult(w http.ResponseWriter, r *http.Request, e *eventReq, notice string, err error) {
	if err != nil {
		status, msg := pageErrorStatus(err)
		if status == http.StatusInternalServerError || status == http.StatusNotFound {
			h.renderError(w, r, e.principal, err)
			return
		}
		h.renderRegistration(w, r, e, status, msg, "")
		return
	}
	h.backToEvent(w, r, e, "registration", notice)
}

func (h *Handler) registrationPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	opens, err := service.ParseLocalTime(r.PostFormValue("reg_opens"), e.event.TimeZone)
	var closes *time.Time
	if err == nil {
		closes, err = service.ParseLocalTime(r.PostFormValue("reg_closes"), e.event.TimeZone)
	}
	var ageOn *time.Time
	if v := strings.TrimSpace(r.PostFormValue("age_on")); v != "" && err == nil {
		d, perr := time.Parse("2006-01-02", v)
		if perr != nil {
			err = fmt.Errorf("%w: enter the date ages are counted on, such as 14 November", domain.ErrValidation)
		}
		ageOn = &d
	}
	if err == nil {
		err = h.Registrations.UpdateSettings(r.Context(), e.es, domain.RegistrationSettings{
			Mode: r.PostFormValue("reg_mode"), OpensAt: opens, ClosesAt: closes,
			Waitlist: r.PostFormValue("waitlist") == "on", PassPrefix: r.PostFormValue("pass_prefix"), AgeOn: ageOn,
		})
	}
	h.registrationResult(w, r, e, "saved", err)
}

func fieldFromForm(r *http.Request) domain.FormField {
	return domain.FormField{
		Label: r.PostFormValue("label"), Type: r.PostFormValue("type"), Help: r.PostFormValue("help"),
		Required: r.PostFormValue("required") == "on", Visibility: r.PostFormValue("visibility"),
		Options: strings.Split(r.PostFormValue("options"), "\n"),
	}
}

func (h *Handler) addFieldPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	_, err := h.Registrations.SaveField(r.Context(), e.es, fieldFromForm(r))
	h.registrationResult(w, r, e, "added", err)
}

func (h *Handler) updateFieldPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	f := fieldFromForm(r)
	f.ID = r.PathValue("field")
	_, err := h.Registrations.SaveField(r.Context(), e.es, f)
	h.registrationResult(w, r, e, "updated", err)
}

func (h *Handler) removeFieldPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.registrationResult(w, r, e, "removed", h.Registrations.DeleteField(r.Context(), e.es, r.PathValue("field")))
}

func (h *Handler) fieldUpPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.registrationResult(w, r, e, "", h.Registrations.MoveField(r.Context(), e.es, r.PathValue("field"), true))
}

func (h *Handler) fieldDownPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.registrationResult(w, r, e, "", h.Registrations.MoveField(r.Context(), e.es, r.PathValue("field"), false))
}

func (h *Handler) publishPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.registrationResult(w, r, e, "published", h.Registrations.SetPublished(r.Context(), e.es, e.event, true))
}

func (h *Handler) unpublishPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.registrationResult(w, r, e, "unpublished", h.Registrations.SetPublished(r.Context(), e.es, e.event, false))
}

type participantsData struct {
	eventPageData
	Participants []domain.EventParticipant
	Counts       domain.ParticipantCounts
	Total        int
	Filter, Q    string // Filter: the status shown ("" = all, "minors"); Status is the event's
	Statuses     []viewLink
	AgeOn        string // the under-18 cut-off, as shown
}

var statusFilterLabels = []viewLink{{"", "All"}, {domain.PartRegistered, "Registered"}, {domain.PartPending, "Waiting for approval"},
	{domain.PartWaitlisted, "Waitlist"}, {domain.PartCancelled, "Cancelled"}}

func (d participantsData) Href(status string) string {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if d.Q != "" {
		q.Set("q", d.Q)
	}
	u := d.Base() + "/participants"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

func (d participantsData) Count(status string) int {
	if status == "" {
		return d.Total
	}
	return d.Counts[status]
}

var participantNotices = map[string]string{"status": "Status updated."}

func (h *Handler) participantsPage(w http.ResponseWriter, r *http.Request, e *eventReq) {
	q := participantQuery(r)
	list, counts, err := h.Registrations.Participants(r.Context(), e.es, q)
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	d := participantsData{eventPageData: h.eventData(e, "participants"), Participants: list, Counts: counts,
		Filter: q.Status, Q: strings.TrimSpace(q.Q), Statuses: statusFilterLabels}
	if q.Minors {
		d.Filter = domain.CountMinors
	}
	for status, n := range counts {
		if status != domain.CountMinors { // minors are also counted under their status
			d.Total += n
		}
	}
	if counts[domain.CountMinors] > 0 || q.Minors {
		d.Statuses = append(append([]viewLink{}, statusFilterLabels...), viewLink{domain.CountMinors, "Under 18"})
	}
	if e.event.StartsAt != nil || e.event.AgeOn != nil {
		d.AgeOn = service.AgeOn(e.event).Format("2 Jan 2006")
	}
	h.render(w, http.StatusOK, "event_participants", e.principal, view{Title: "Participants · " + e.event.Name,
		Notice: participantNotices[r.URL.Query().Get("notice")], Data: d})
}

func (h *Handler) participantStatusPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.eventAction(w, r, e, "participants", "status",
		h.Registrations.SetStatus(r.Context(), e.es, r.PathValue("participant"), r.PostFormValue("status")))
}

// =============================================================================
// Public event page: /e/{org}/{event}
// =============================================================================

type publicData struct {
	Event    *domain.PublicEvent
	Open     bool
	Closed   string
	Values   map[string][]string
	Errors   service.FieldErrors
	Action   string
	Result   *service.RegisterResult
	Capacity string
}

// Value is the first posted value for a key, to refill the form.
func (d publicData) Value(key string) string {
	if v := d.Values[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// Has reports whether value was posted for key (checkboxes, choices).
func (d publicData) Has(key, value string) bool {
	for _, v := range d.Values[key] {
		if v == value {
			return true
		}
	}
	return false
}

func (h *Handler) publicData(pe *domain.PublicEvent) publicData {
	open, why := pe.RegistrationOpen(time.Now())
	d := publicData{Event: pe, Open: open, Closed: why, Values: map[string][]string{},
		Action: "/e/" + pe.OrgSlug + "/" + pe.Event.Slug + "/register"}
	if c := pe.Event.Capacity; c != nil && open {
		if left := *c - pe.Taken; left > 0 && left <= 20 {
			d.Capacity = plural(left, "place", "places") + " left"
		} else if left <= 0 {
			d.Capacity = "Full: new registrations join the waitlist"
		}
	}
	return d
}

func (h *Handler) publicEventPage(w http.ResponseWriter, r *http.Request) {
	pe, err := h.Registrations.PublicEvent(r.Context(), r.PathValue("org"), r.PathValue("event"))
	if err != nil {
		h.publicError(w, r, err)
		return
	}
	h.render(w, http.StatusOK, "public_event", nil, view{Title: pe.Event.Name, Data: h.publicData(pe)})
}

func (h *Handler) publicError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, domain.ErrNotFound) {
		h.renderStatus(w, r, nil, http.StatusNotFound, "Event not found",
			"This event doesn't exist or isn't open to the public. Check the link with the organizer.")
		return
	}
	h.renderError(w, r, nil, err)
}

func (h *Handler) registerPost(w http.ResponseWriter, r *http.Request) {
	if !h.sameOrigin(r) {
		h.renderStatus(w, r, nil, http.StatusForbidden, "Registration refused", "That form came from another site.")
		return
	}
	if !h.registerLimit.allow(h.clientIP(r)) {
		h.renderStatus(w, r, nil, http.StatusTooManyRequests, "Too many attempts", "Wait a minute, then try again.")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderStatus(w, r, nil, http.StatusBadRequest, "Form not readable", "Go back and try again.")
		return
	}
	req := service.RegisterRequest{FullName: r.PostForm.Get("full_name"), Email: r.PostForm.Get("email"),
		Answers: map[string][]string{}}
	for k, v := range r.PostForm {
		if strings.HasPrefix(k, "q.") {
			req.Answers[strings.TrimPrefix(k, "q.")] = v
		}
	}
	res, err := h.Registrations.Register(r.Context(), r.PathValue("org"), r.PathValue("event"), req)
	if err != nil {
		var fe service.FieldErrors
		pe, perr := h.Registrations.PublicEvent(r.Context(), r.PathValue("org"), r.PathValue("event"))
		if perr != nil {
			h.publicError(w, r, perr)
			return
		}
		d := h.publicData(pe)
		d.Values = r.PostForm
		status, msg := pageErrorStatus(err)
		if errors.As(err, &fe) {
			d.Errors, msg = fe, "Some answers need another look."
		}
		if status == http.StatusInternalServerError {
			h.renderError(w, r, nil, err)
			return
		}
		h.render(w, status, "public_event", nil, view{Title: pe.Event.Name, Error: msg, Data: d})
		return
	}
	d := h.publicData(res.Event)
	d.Result = res
	h.render(w, http.StatusOK, "public_registered", nil, view{Title: "Registered · " + res.Event.Event.Name, Data: d})
}

// participantPassPNG and participantPassPDF let organizers preview and
// download someone's pass.
func (h *Handler) participantPassPNG(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.participantPass(w, r, e, "png")
}

func (h *Handler) participantPassPDF(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.participantPass(w, r, e, "pdf")
}

func (h *Handler) participantPass(w http.ResponseWriter, r *http.Request, e *eventReq, kind string) {
	pc, err := h.Registrations.PassFor(r.Context(), e.es, r.PathValue("participant"))
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	h.sendPass(w, r, e.principal, pc, kind)
}

// sendPass writes a rendered pass as a download.
func (h *Handler) sendPass(w http.ResponseWriter, r *http.Request, p *principal, pc *domain.PassContext, kind string) {
	var b []byte
	var err error
	ctype := "image/png"
	if kind == "pdf" {
		b, err = h.Passes.PDF(pc)
		ctype = "application/pdf"
	} else {
		b, err = h.Passes.PNG(pc)
	}
	if err != nil {
		h.renderError(w, r, p, err)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "private, no-store")
	disposition := "inline"
	if r.URL.Query().Get("download") != "" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", disposition+`; filename="`+service.PassFilename(pc.Participant.PassID, kind)+`"`)
	_, _ = w.Write(b)
}

// POST /…/passes/check {payload}: is this scanned QR a valid pass here?
func (h *Handler) apiCheckPass(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var req struct {
		Payload string `json:"payload"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	res, err := h.Registrations.CheckPass(r.Context(), e.es, h.Passes, h.Checker, req.Payload)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
