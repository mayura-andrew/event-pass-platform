package httphandler

// Events home, event editor, sharing and activity (milestone M2).

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/internal/service"
)

// eventRoute is one endpoint on a single event. action is enforced before fn
// runs (see resolveEvent), and the authorization matrix test is generated
// from this table: every route x every role. audit is the audit action a
// successful call must write ("" for reads and personal state like stars).
type eventRoute struct {
	method, pattern string
	api             bool
	action          authz.EventAction
	audit           string
	fn              eventFunc
}

const ev = "/api/v1/orgs/{org}/events/{event}"
const evPage = "/o/{org}/events/{event}"

var eventRoutes = []eventRoute{
	{"GET", ev, true, authz.EventView, "", (*Handler).apiGetEvent},
	{"PATCH", ev, true, authz.EventEdit, "event.update", (*Handler).apiUpdateEvent},
	{"DELETE", ev, true, authz.EventDelete, "event.delete", (*Handler).apiDeleteEvent},
	{"POST", ev + "/restore", true, authz.EventDelete, "event.restore", (*Handler).apiRestoreEvent},
	{"POST", ev + "/archive", true, authz.EventEdit, "event.archive", (*Handler).apiArchiveEvent},
	{"POST", ev + "/unarchive", true, authz.EventEdit, "event.unarchive", (*Handler).apiUnarchiveEvent},
	{"POST", ev + "/duplicate", true, authz.EventEdit, "event.duplicate", (*Handler).apiDuplicateEvent},
	{"POST", ev + "/transfer", true, authz.EventTransfer, "event.transfer", (*Handler).apiTransferEvent},
	{"PATCH", ev + "/settings", true, authz.EventTransfer, "event.settings", (*Handler).apiEventSettings},
	{"PUT", ev + "/star", true, authz.EventView, "", (*Handler).apiStar},
	{"DELETE", ev + "/star", true, authz.EventView, "", (*Handler).apiUnstar},
	{"GET", ev + "/shares", true, authz.EventEdit, "", (*Handler).apiGetSharing},
	{"PUT", ev + "/shares", true, authz.EventShare, "event.invite.create", (*Handler).apiShare},
	{"PATCH", ev + "/shares/{share}", true, authz.EventShare, "event.share.role", (*Handler).apiSetShareRole},
	{"DELETE", ev + "/shares/{share}", true, authz.EventShare, "event.share.remove", (*Handler).apiRemoveShare},
	{"DELETE", ev + "/invites/{id}", true, authz.EventShare, "invite.revoke", (*Handler).apiRevokeEventInvite},
	{"POST", ev + "/link-share", true, authz.EventShare, "event.link.on", (*Handler).apiEnableLink},
	{"DELETE", ev + "/link-share", true, authz.EventShare, "event.link.off", (*Handler).apiDisableLink},
	{"GET", ev + "/activity", true, authz.EventViewAudit, "", (*Handler).apiEventActivity},

	{"GET", evPage, false, authz.EventView, "", (*Handler).eventPage},
	{"POST", evPage, false, authz.EventEdit, "event.update", (*Handler).updateEventPost},
	{"GET", evPage + "/share", false, authz.EventEdit, "", (*Handler).sharePage},
	{"POST", evPage + "/share", false, authz.EventShare, "event.invite.create", (*Handler).sharePost},
	{"POST", evPage + "/shares/{share}/role", false, authz.EventShare, "event.share.role", (*Handler).shareRolePost},
	{"POST", evPage + "/shares/{share}/remove", false, authz.EventShare, "event.share.remove", (*Handler).shareRemovePost},
	{"POST", evPage + "/invites/{id}/revoke", false, authz.EventShare, "invite.revoke", (*Handler).eventInviteRevokePost},
	{"POST", evPage + "/link", false, authz.EventShare, "event.link.on", (*Handler).linkPost},
	{"POST", evPage + "/link/remove", false, authz.EventShare, "event.link.off", (*Handler).linkRemovePost},
	{"GET", evPage + "/activity", false, authz.EventViewAudit, "", (*Handler).eventActivityPage},
	{"GET", evPage + "/settings", false, authz.EventEdit, "", (*Handler).eventSettingsPage},
	{"POST", evPage + "/settings", false, authz.EventTransfer, "event.settings", (*Handler).eventSettingsPost},
	{"POST", evPage + "/archive", false, authz.EventEdit, "event.archive", (*Handler).archivePost},
	{"POST", evPage + "/unarchive", false, authz.EventEdit, "event.unarchive", (*Handler).unarchivePost},
	{"POST", evPage + "/delete", false, authz.EventDelete, "event.delete", (*Handler).deletePost},
	{"POST", evPage + "/restore", false, authz.EventDelete, "event.restore", (*Handler).restorePost},
	{"POST", evPage + "/duplicate", false, authz.EventEdit, "event.duplicate", (*Handler).duplicatePost},
	{"POST", evPage + "/transfer", false, authz.EventTransfer, "event.transfer", (*Handler).transferPost},
	{"POST", evPage + "/star", false, authz.EventView, "", (*Handler).starPost},
	{"POST", evPage + "/unstar", false, authz.EventView, "", (*Handler).unstarPost},
}

// =============================================================================
// JSON API
// =============================================================================

func (h *Handler) apiOrgSettings(w http.ResponseWriter, r *http.Request, o *orgReq) {
	var req struct {
		ExternalSharing *bool `json:"external_sharing"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	if req.ExternalSharing == nil {
		writeError(w, http.StatusBadRequest, "external_sharing is required")
		return
	}
	if err := h.Orgs.UpdateSettings(r.Context(), o.sc, o.org, *req.ExternalSharing); err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) apiOrgActivity(w http.ResponseWriter, r *http.Request, o *orgReq) {
	entries, err := h.Orgs.Activity(r.Context(), o.sc)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func listQuery(r *http.Request) domain.EventListQuery {
	q := r.URL.Query()
	return domain.EventListQuery{View: q.Get("view"), Q: q.Get("q"), Sort: q.Get("sort")}
}

// GET /api/v1/orgs/{org}/events?view=mine|shared|recent|starred|archived|trash|all&q=&sort=
func (h *Handler) apiListEvents(w http.ResponseWriter, r *http.Request, o *orgReq) {
	cards, err := h.Events.List(r.Context(), o.sc, listQuery(r))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cards)
}

func (h *Handler) apiCreateEvent(w http.ResponseWriter, r *http.Request, o *orgReq) {
	var req service.CreateEventRequest
	if !decodeJSON(w, r, 32<<10, &req) {
		return
	}
	e, err := h.Events.Create(r.Context(), o.sc, req)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (h *Handler) apiGetEvent(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.Events.Opened(r.Context(), e.es)
	writeJSON(w, http.StatusOK, map[string]any{"event": e.event, "role": e.es.Role(),
		"display_status": e.event.DisplayStatus(time.Now())})
}

func (h *Handler) apiUpdateEvent(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var in domain.EventInput
	if !decodeJSON(w, r, 32<<10, &in) {
		return
	}
	out, err := h.Events.Update(r.Context(), e.es, e.event, in)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func noContent(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) apiDeleteEvent(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Events.SetDeleted(r.Context(), e.es, e.event, true))
}

func (h *Handler) apiRestoreEvent(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Events.SetDeleted(r.Context(), e.es, e.event, false))
}

func (h *Handler) apiArchiveEvent(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Events.SetArchived(r.Context(), e.es, e.event, true))
}

func (h *Handler) apiUnarchiveEvent(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Events.SetArchived(r.Context(), e.es, e.event, false))
}

func (h *Handler) apiDuplicateEvent(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var in domain.EventInput
	if r.ContentLength != 0 && !decodeJSON(w, r, 32<<10, &in) {
		return
	}
	out, err := h.Events.Duplicate(r.Context(), e.es, e.event, in)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) apiTransferEvent(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var req struct {
		AccountID string `json:"account_id"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	noContent(w, r, h.Events.Transfer(r.Context(), e.es, e.event, req.AccountID))
}

func (h *Handler) apiEventSettings(w http.ResponseWriter, r *http.Request, e *eventReq) {
	st := e.event.Settings
	if !decodeJSON(w, r, 4<<10, &st) {
		return
	}
	noContent(w, r, h.Events.UpdateSettings(r.Context(), e.es, st))
}

func (h *Handler) apiStar(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Events.SetStarred(r.Context(), e.es, true))
}

func (h *Handler) apiUnstar(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Events.SetStarred(r.Context(), e.es, false))
}

func (h *Handler) apiGetSharing(w http.ResponseWriter, r *http.Request, e *eventReq) {
	st, err := h.Events.Sharing(r.Context(), e.es)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// PUT /…/shares {email, role}: share with a person (or invite them).
func (h *Handler) apiShare(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	res, err := h.Events.Share(r.Context(), e.es, e.org, e.event, e.account, req.Email, req.Role)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) apiSetShareRole(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var req struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	noContent(w, r, h.Events.SetShareRole(r.Context(), e.es, r.PathValue("share"), req.Role))
}

func (h *Handler) apiRemoveShare(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Events.RemoveShare(r.Context(), e.es, r.PathValue("share")))
}

func (h *Handler) apiRevokeEventInvite(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Events.RevokeInvite(r.Context(), e.es, r.PathValue("id")))
}

// POST /…/link-share {expires_in_days}: returns the link's URL, once.
func (h *Handler) apiEnableLink(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var req struct {
		ExpiresInDays int `json:"expires_in_days"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	u, err := h.Events.EnableLink(r.Context(), e.es, e.org, e.event, req.ExpiresInDays)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]string{"url": u})
}

func (h *Handler) apiDisableLink(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Events.DisableLink(r.Context(), e.es))
}

func (h *Handler) apiEventActivity(w http.ResponseWriter, r *http.Request, e *eventReq) {
	entries, err := h.Events.Activity(r.Context(), e.es)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// =============================================================================
// Pages
// =============================================================================

// orgPageData is shared by pages under /o/{org}: the org and the tabs.
type orgPageData struct {
	Org *domain.OrgMembership
	Tab string
	sc  authz.OrgScope
}

// OrgTab is the highlighted org tab; event pages keep "events" here while
// their own Tab names the event tab.
func (d orgPageData) OrgTab() string       { return d.Tab }
func (d orgPageData) CanSeeMembers() bool  { return d.sc.Can(authz.ViewMembers) }
func (d orgPageData) CanSeeActivity() bool { return d.sc.Can(authz.ViewAudit) }
func (d orgPageData) CanCreate() bool      { return d.sc.Can(authz.CreateEvent) }

type viewLink struct {
	Key, Label string
}

type eventsHomeData struct {
	orgPageData
	View, Q, Sort, Layout string
	Views                 []viewLink
	Sorts                 []viewLink
	Events                []domain.EventCard
}

var viewLabels = map[string]string{
	domain.ViewMine: "My events", "all": "All events", domain.ViewShared: "Shared with me",
	domain.ViewRecent: "Recent", domain.ViewStarred: "Starred", domain.ViewArchived: "Archived", domain.ViewTrash: "Trash",
}

// Href is the events home URL with one parameter changed.
func (d eventsHomeData) Href(key, value string) string {
	q := url.Values{}
	for k, v := range map[string]string{"view": d.View, "q": d.Q, "sort": d.Sort, "layout": d.Layout} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if value == "" {
		q.Del(key)
	} else {
		q.Set(key, value)
	}
	if key == "view" {
		q.Del("q")
	}
	if len(q) == 0 {
		return "/o/" + d.Org.Slug
	}
	return "/o/" + d.Org.Slug + "?" + q.Encode()
}

func (d eventsHomeData) ViewLabel() string { return viewLabels[d.View] }

func (h *Handler) eventsHomePage(w http.ResponseWriter, r *http.Request, o *orgReq) {
	q := listQuery(r)
	if q.View == "" {
		q.View = domain.ViewMine
		if o.sc.Role() == domain.OrgRoleGuest {
			q.View = domain.ViewShared
		}
	}
	if q.View == "all" && !o.sc.Can(authz.AllEvents) {
		q.View = domain.ViewMine
	}
	cards, err := h.Events.List(r.Context(), o.sc, q)
	if err != nil {
		h.renderError(w, r, o.principal, err)
		return
	}
	layout := r.URL.Query().Get("layout")
	if layout != "list" {
		layout = "grid"
	}
	d := eventsHomeData{orgPageData: orgPageData{Org: o.org, Tab: "events", sc: o.sc},
		View: q.View, Q: strings.TrimSpace(q.Q), Sort: q.Sort, Layout: layout, Events: cards}
	views := []string{domain.ViewMine}
	if o.sc.Can(authz.AllEvents) {
		views = append(views, "all")
	}
	views = append(views, domain.ViewShared, domain.ViewRecent, domain.ViewStarred, domain.ViewArchived, domain.ViewTrash)
	if o.sc.Role() == domain.OrgRoleGuest {
		views = []string{domain.ViewShared, domain.ViewRecent, domain.ViewStarred}
	}
	for _, v := range views {
		d.Views = append(d.Views, viewLink{v, viewLabels[v]})
	}
	d.Sorts = []viewLink{{"", "Last modified"}, {domain.SortOpened, "Last opened"}, {domain.SortDate, "Event date"}, {domain.SortName, "Name"}}
	h.render(w, http.StatusOK, "events", o.principal, view{Title: o.org.Name, Data: d,
		Notice: orgNotices[r.URL.Query().Get("notice")]})
}

// ---- New event ---------------------------------------------------------------

// eventForm is the create/edit form's values, kept as typed so a failed
// submit shows them again.
type eventForm struct {
	Name, Type, Description, Starts, Ends, TimeZone string
	VenueName, VenueAddress, Lat, Lng, Online, Cap  string
	Visibility, Tags, DuplicateFrom                 string
}

func formFromEvent(e *domain.Event) eventForm {
	f := eventForm{Name: e.Name, Type: e.Type, Description: e.Description, TimeZone: e.TimeZone,
		VenueName: e.Venue.Name, VenueAddress: e.Venue.Address, Online: e.OnlineURL, Visibility: e.Visibility,
		Tags: strings.Join(e.Tags, ", ")}
	loc, err := time.LoadLocation(e.TimeZone)
	if err != nil {
		loc = time.UTC
	}
	if e.StartsAt != nil {
		f.Starts = e.StartsAt.In(loc).Format("2006-01-02T15:04")
	}
	if e.EndsAt != nil {
		f.Ends = e.EndsAt.In(loc).Format("2006-01-02T15:04")
	}
	if e.Venue.Lat != nil {
		f.Lat = strconv.FormatFloat(*e.Venue.Lat, 'f', -1, 64)
		f.Lng = strconv.FormatFloat(*e.Venue.Lng, 'f', -1, 64)
	}
	if e.Capacity != nil {
		f.Cap = strconv.Itoa(*e.Capacity)
	}
	return f
}

func readEventForm(r *http.Request) eventForm {
	v := r.PostFormValue
	return eventForm{Name: v("name"), Type: v("type"), Description: v("description"), Starts: v("starts"),
		Ends: v("ends"), TimeZone: v("time_zone"), VenueName: v("venue_name"), VenueAddress: v("venue_address"),
		Lat: v("lat"), Lng: v("lng"), Online: v("online_url"), Cap: v("capacity"), Visibility: v("visibility"),
		Tags: v("tags"), DuplicateFrom: v("duplicate_from")}
}

// input turns the form into a full EventInput (every field set, so an edit
// can also clear values).
func (f eventForm) input() (domain.EventInput, error) {
	tz := strings.TrimSpace(f.TimeZone)
	if tz == "" {
		tz = "UTC"
	}
	starts, err := service.ParseLocalTime(f.Starts, tz)
	if err != nil {
		return domain.EventInput{}, err
	}
	ends, err := service.ParseLocalTime(f.Ends, tz)
	if err != nil {
		return domain.EventInput{}, err
	}
	venue := domain.EventVenue{Name: f.VenueName, Address: f.VenueAddress}
	if strings.TrimSpace(f.Lat) != "" || strings.TrimSpace(f.Lng) != "" {
		lat, err1 := strconv.ParseFloat(strings.TrimSpace(f.Lat), 64)
		lng, err2 := strconv.ParseFloat(strings.TrimSpace(f.Lng), 64)
		if err1 != nil || err2 != nil {
			return domain.EventInput{}, fmt.Errorf("%w: the map pin needs a latitude and a longitude as numbers", domain.ErrValidation)
		}
		venue.Lat, venue.Lng = &lat, &lng
	}
	in := domain.EventInput{Name: &f.Name, Description: &f.Description, TimeZone: &tz, Venue: &venue,
		OnlineURL: &f.Online, Visibility: &f.Visibility, StartsAt: starts, EndsAt: ends,
		ClearDates: true, ClearCapacity: true, Tags: strings.Split(f.Tags, ",")}
	if f.Type != "" {
		in.Type = &f.Type
	}
	if f.Visibility == "" {
		in.Visibility = nil
	}
	if c := strings.TrimSpace(f.Cap); c != "" {
		n, err := strconv.Atoi(c)
		if err != nil {
			return domain.EventInput{}, fmt.Errorf("%w: capacity must be a whole number", domain.ErrValidation)
		}
		in.Capacity = &n
	}
	return in, nil
}

// Time zones offered in the form; the event's own zone is added if missing.
var commonZones = []string{
	"UTC", "Asia/Colombo", "Asia/Kolkata", "Asia/Dhaka", "Asia/Kathmandu", "Asia/Karachi", "Asia/Dubai",
	"Asia/Singapore", "Asia/Kuala_Lumpur", "Asia/Jakarta", "Asia/Bangkok", "Asia/Manila", "Asia/Hong_Kong",
	"Asia/Shanghai", "Asia/Tokyo", "Asia/Seoul", "Australia/Sydney", "Australia/Perth", "Pacific/Auckland",
	"Africa/Nairobi", "Africa/Lagos", "Africa/Cairo", "Africa/Johannesburg", "Europe/London", "Europe/Paris",
	"Europe/Berlin", "Europe/Madrid", "Europe/Rome", "Europe/Istanbul", "Europe/Moscow",
	"America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles", "America/Toronto",
	"America/Mexico_City", "America/Bogota", "America/Lima", "America/Sao_Paulo", "America/Buenos_Aires",
}

type eventFormData struct {
	orgPageData
	Form       eventForm
	Types      []viewLink
	Zones      []string
	Visibility []viewLink
	CopyFrom   []domain.EventCard // events the caller may duplicate (new only)
	Action     string
}

func (h *Handler) formData(o *orgReq, f eventForm, action string) eventFormData {
	d := eventFormData{orgPageData: orgPageData{Org: o.org, Tab: "events", sc: o.sc}, Form: f, Action: action,
		Zones: commonZones,
		Visibility: []viewLink{{"private", "Private: invite only"}, {"unlisted", "Unlisted: anyone with the link"},
			{"public", "Public: listed on your organization's page"}}}
	for _, t := range domain.EventTypes {
		d.Types = append(d.Types, viewLink{t, domain.EventTypeLabels[t]})
	}
	if f.TimeZone != "" && !slices.Contains(commonZones, f.TimeZone) {
		d.Zones = append([]string{f.TimeZone}, commonZones...)
	}
	return d
}

func (h *Handler) newEventPage(w http.ResponseWriter, r *http.Request, o *orgReq) {
	h.renderNewEvent(w, r, o, http.StatusOK, "", eventForm{Type: "hackathon", TimeZone: "UTC", Visibility: "private",
		DuplicateFrom: r.URL.Query().Get("from")})
}

func (h *Handler) renderNewEvent(w http.ResponseWriter, r *http.Request, o *orgReq, status int, errMsg string, f eventForm) {
	if err := o.sc.Require(authz.CreateEvent); err != nil {
		h.renderError(w, r, o.principal, err)
		return
	}
	d := h.formData(o, f, "/o/"+o.org.Slug+"/events")
	// "Copy one of your events": every event the caller could edit.
	var cards []domain.EventCard
	views := []string{domain.ViewMine, domain.ViewShared, domain.ViewArchived}
	if o.sc.Can(authz.AllEvents) {
		views = []string{"all", domain.ViewArchived}
	}
	for _, v := range views {
		c, err := h.Events.List(r.Context(), o.sc, domain.EventListQuery{View: v, Sort: domain.SortModified})
		if err != nil {
			h.renderError(w, r, o.principal, err)
			return
		}
		cards = append(cards, c...)
	}
	for _, c := range cards {
		if authz.CanEvent(c.Role, authz.EventEdit, c.Settings) {
			d.CopyFrom = append(d.CopyFrom, c)
		}
	}
	h.render(w, status, "event_new", o.principal, view{Title: "New event", Error: errMsg, Data: d})
}

func (h *Handler) createEventPost(w http.ResponseWriter, r *http.Request, o *orgReq) {
	f := readEventForm(r)
	in, err := f.input()
	var e *domain.Event
	if err == nil {
		req := service.CreateEventRequest{EventInput: in, DuplicateFrom: f.DuplicateFrom}
		if f.DuplicateFrom != "" {
			// A copy keeps the source's details unless the form says otherwise.
			req.EventInput = domain.EventInput{Name: in.Name, StartsAt: in.StartsAt, EndsAt: in.EndsAt}
			if strings.TrimSpace(f.Name) == "" {
				req.EventInput.Name = nil
			}
		}
		e, err = h.Events.Create(r.Context(), o.sc, req)
	}
	if err != nil {
		status, msg := pageErrorStatus(err)
		if status == http.StatusInternalServerError {
			h.renderError(w, r, o.principal, err)
			return
		}
		h.renderNewEvent(w, r, o, status, msg, f)
		return
	}
	http.Redirect(w, r, "/o/"+o.org.Slug+"/events/"+e.ID+"?notice=created", http.StatusSeeOther)
}

// ---- One event -----------------------------------------------------------------

type eventPageData struct {
	eventFormData
	Event  *domain.Event
	Status string
	Role   string
	Tab    string
	es     authz.EventScope
}

func (d eventPageData) Can(action string) bool { return d.es.Can(authz.EventAction(action)) }
func (d eventPageData) Base() string           { return "/o/" + d.Org.Slug + "/events/" + d.Event.ID }
func (d eventPageData) InTrash() bool          { return d.Event.DeletedAt != nil }
func (d eventPageData) CanSeeShare() bool {
	return d.es.Can(authz.EventEdit) || d.es.Can(authz.EventShare)
}

var eventNotices = map[string]string{
	"created":    "Event created. Fill in the details, then share it with your team.",
	"saved":      "Changes saved.",
	"joined":     "You now have access to this event.",
	"shared":     "Shared.",
	"invited":    "Invite sent. They'll get access when they accept.",
	"role":       "Access updated.",
	"removed":    "Access removed.",
	"revoked":    "Invite withdrawn.",
	"link-off":   "Link sharing turned off. The old link no longer works.",
	"archived":   "Event archived. It moved out of your main lists but stays searchable.",
	"unarchived": "Event restored from the archive.",
	"restored":   "Event restored from Trash.",
	"transfer":   "Ownership transferred. You keep editor access.",
	"settings":   "Settings saved.",
	"starred":    "Starred.",
	"unstarred":  "Removed from Starred.",
}

func (h *Handler) eventData(e *eventReq, tab string) eventPageData {
	return eventPageData{eventFormData: h.formData(e.orgReq, formFromEvent(e.event), ""),
		Event: e.event, Status: e.event.DisplayStatus(time.Now()), Role: e.es.Role(), Tab: tab, es: e.es}
}

func (h *Handler) eventPage(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.Events.Opened(r.Context(), e.es)
	h.renderEvent(w, r, e, http.StatusOK, "", eventNotices[r.URL.Query().Get("notice")], nil)
}

func (h *Handler) renderEvent(w http.ResponseWriter, r *http.Request, e *eventReq, status int, errMsg, notice string, form *eventForm) {
	d := h.eventData(e, "overview")
	if form != nil {
		d.Form = *form
	}
	h.render(w, status, "event", e.principal, view{Title: e.event.Name, Error: errMsg, Notice: notice, Data: d})
}

func (h *Handler) updateEventPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	f := readEventForm(r)
	in, err := f.input()
	if err == nil {
		_, err = h.Events.Update(r.Context(), e.es, e.event, in)
	}
	if err != nil {
		status, msg := pageErrorStatus(err)
		if status == http.StatusInternalServerError {
			h.renderError(w, r, e.principal, err)
			return
		}
		h.renderEvent(w, r, e, status, msg, "", &f)
		return
	}
	h.backToEvent(w, r, e, "", "saved")
}

func (h *Handler) backToEvent(w http.ResponseWriter, r *http.Request, e *eventReq, tab, notice string) {
	u := "/o/" + e.org.Slug + "/events/" + e.event.ID
	if tab != "" {
		u += "/" + tab
	}
	http.Redirect(w, r, u+"?notice="+notice, http.StatusSeeOther)
}

// eventAction finishes a change from an event page: back with a notice, or
// the error page.
func (h *Handler) eventAction(w http.ResponseWriter, r *http.Request, e *eventReq, tab, notice string, err error) {
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	h.backToEvent(w, r, e, tab, notice)
}

// ---- Sharing -------------------------------------------------------------------

type shareData struct {
	eventPageData
	Sharing *domain.SharingState
	NewLink string
	Roles   []viewLink
	Email   string
}

var shareRoleOptions = []viewLink{{domain.EventRoleEditor, "Editor"}, {domain.EventRoleCheckin, "Check-in staff"}, {domain.EventRoleViewer, "Viewer"}}

func (h *Handler) sharePage(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.renderShare(w, r, e, http.StatusOK, "", eventNotices[r.URL.Query().Get("notice")], "", "")
}

func (h *Handler) renderShare(w http.ResponseWriter, r *http.Request, e *eventReq, status int, errMsg, notice, newLink, email string) {
	st, err := h.Events.Sharing(r.Context(), e.es)
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	d := shareData{eventPageData: h.eventData(e, "share"), Sharing: st, NewLink: newLink, Roles: shareRoleOptions, Email: email}
	h.render(w, status, "event_share", e.principal, view{Title: "Share · " + e.event.Name, Error: errMsg, Notice: notice, Data: d})
}

// shareResult re-renders the Share tab with the error, or goes back to it.
func (h *Handler) shareResult(w http.ResponseWriter, r *http.Request, e *eventReq, notice string, err error, email string) {
	if err != nil {
		status, msg := pageErrorStatus(err)
		if status == http.StatusInternalServerError || status == http.StatusNotFound {
			h.renderError(w, r, e.principal, err)
			return
		}
		h.renderShare(w, r, e, status, msg, "", "", email)
		return
	}
	h.backToEvent(w, r, e, "share", notice)
}

func (h *Handler) sharePost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	email := r.PostFormValue("email")
	res, err := h.Events.Share(r.Context(), e.es, e.org, e.event, e.account, email, r.PostFormValue("role"))
	notice := "shared"
	if err == nil && res.Invite != nil {
		notice = "invited"
	}
	h.shareResult(w, r, e, notice, err, email)
}

func (h *Handler) shareRolePost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.shareResult(w, r, e, "role", h.Events.SetShareRole(r.Context(), e.es, r.PathValue("share"), r.PostFormValue("role")), "")
}

func (h *Handler) shareRemovePost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.shareResult(w, r, e, "removed", h.Events.RemoveShare(r.Context(), e.es, r.PathValue("share")), "")
}

func (h *Handler) eventInviteRevokePost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.shareResult(w, r, e, "revoked", h.Events.RevokeInvite(r.Context(), e.es, r.PathValue("id")), "")
}

// linkPost turns on (or resets) the link and shows its URL once, on this
// response: only the token's hash is stored.
func (h *Handler) linkPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	days, _ := strconv.Atoi(r.PostFormValue("expires_in_days"))
	u, err := h.Events.EnableLink(r.Context(), e.es, e.org, e.event, days)
	if err != nil {
		h.shareResult(w, r, e, "", err, "")
		return
	}
	h.renderShare(w, r, e, http.StatusOK, "", "Link sharing is on. Copy the link now: it is shown only once.", u, "")
}

func (h *Handler) linkRemovePost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.shareResult(w, r, e, "link-off", h.Events.DisableLink(r.Context(), e.es), "")
}

// ---- Settings, archive, trash, transfer, star -------------------------------------

type settingsData struct {
	eventPageData
	Members []domain.OrgMember // candidates for ownership
}

func (h *Handler) eventSettingsPage(w http.ResponseWriter, r *http.Request, e *eventReq) {
	d := settingsData{eventPageData: h.eventData(e, "settings")}
	if e.es.Can(authz.EventTransfer) && e.sc.Can(authz.ViewMembers) {
		members, err := h.Orgs.ListMembers(r.Context(), e.sc)
		if err != nil {
			h.renderError(w, r, e.principal, err)
			return
		}
		for _, m := range members {
			if m.AccountID != e.event.OwnerID {
				d.Members = append(d.Members, m)
			}
		}
	}
	h.render(w, http.StatusOK, "event_settings", e.principal, view{Title: "Settings · " + e.event.Name,
		Notice: eventNotices[r.URL.Query().Get("notice")], Data: d})
}

func (h *Handler) eventSettingsPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	st := domain.EventSettings{
		EditorsCanShare:  r.PostFormValue("editors_can_share") == "on",
		ViewersCanExport: r.PostFormValue("viewers_can_export") == "on",
	}
	h.eventAction(w, r, e, "settings", "settings", h.Events.UpdateSettings(r.Context(), e.es, st))
}

func (h *Handler) archivePost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.eventAction(w, r, e, "", "archived", h.Events.SetArchived(r.Context(), e.es, e.event, true))
}

func (h *Handler) unarchivePost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.eventAction(w, r, e, "", "unarchived", h.Events.SetArchived(r.Context(), e.es, e.event, false))
}

func (h *Handler) deletePost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	if err := h.Events.SetDeleted(r.Context(), e.es, e.event, true); err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	http.Redirect(w, r, "/o/"+e.org.Slug+"?view=trash&notice=trashed", http.StatusSeeOther)
}

func (h *Handler) restorePost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.eventAction(w, r, e, "", "restored", h.Events.SetDeleted(r.Context(), e.es, e.event, false))
}

func (h *Handler) duplicatePost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	out, err := h.Events.Duplicate(r.Context(), e.es, e.event, domain.EventInput{})
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	http.Redirect(w, r, "/o/"+e.org.Slug+"/events/"+out.ID+"?notice=created", http.StatusSeeOther)
}

func (h *Handler) transferPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.eventAction(w, r, e, "", "transfer", h.Events.Transfer(r.Context(), e.es, e.event, r.PostFormValue("account_id")))
}

// starPost and unstarPost go back where the form came from (events home or
// the event), named by a "back" field that must be a path on this site.
func (h *Handler) starPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.starred(w, r, e, true)
}

func (h *Handler) unstarPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.starred(w, r, e, false)
}

func (h *Handler) starred(w http.ResponseWriter, r *http.Request, e *eventReq, on bool) {
	if err := h.Events.SetStarred(r.Context(), e.es, on); err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	if back := service.SafeNext(r.PostFormValue("back")); back != "" {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	notice := "unstarred"
	if on {
		notice = "starred"
	}
	h.backToEvent(w, r, e, "", notice)
}

// ---- Activity -------------------------------------------------------------------

type activityData struct {
	orgPageData
	Event   *eventPageData // nil for the org-wide log
	Entries []domain.AuditEntry
}

func (h *Handler) orgActivityPage(w http.ResponseWriter, r *http.Request, o *orgReq) {
	entries, err := h.Orgs.Activity(r.Context(), o.sc)
	if err != nil {
		h.renderError(w, r, o.principal, err)
		return
	}
	h.render(w, http.StatusOK, "activity", o.principal, view{Title: "Activity · " + o.org.Name,
		Data: activityData{orgPageData: orgPageData{Org: o.org, Tab: "activity", sc: o.sc}, Entries: entries}})
}

func (h *Handler) eventActivityPage(w http.ResponseWriter, r *http.Request, e *eventReq) {
	entries, err := h.Events.Activity(r.Context(), e.es)
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	d := h.eventData(e, "activity")
	h.render(w, http.StatusOK, "activity", e.principal, view{Title: "Activity · " + e.event.Name,
		Data: activityData{orgPageData: d.orgPageData, Event: &d, Entries: entries}})
}

func (h *Handler) orgSettingsPost(w http.ResponseWriter, r *http.Request, o *orgReq) {
	err := h.Orgs.UpdateSettings(r.Context(), o.sc, o.org, r.PostFormValue("external_sharing") == "on")
	h.orgAction(w, r, o, "settings", err)
}

// ---- View-only link ---------------------------------------------------------------

type linkData struct {
	Event   *domain.Event
	OrgName string
}

// GET /l/{token}: read-only event overview for anyone with the link.
func (h *Handler) linkPage(w http.ResponseWriter, r *http.Request) {
	e, orgName, err := h.Events.LinkView(r.Context(), r.PathValue("token"))
	w.Header().Set("X-Robots-Tag", "noindex")
	if err != nil {
		if pageErrorStatusIs(err, http.StatusNotFound) {
			h.renderStatus(w, r, nil, http.StatusNotFound, "Link not available",
				"This link was turned off, has expired, or never existed. Ask the organizer for a new one.")
			return
		}
		h.renderError(w, r, nil, err)
		return
	}
	h.render(w, http.StatusOK, "link", nil, view{Title: e.Name, Data: linkData{Event: e, OrgName: orgName}})
}

func pageErrorStatusIs(err error, status int) bool {
	s, _ := pageErrorStatus(err)
	return s == status
}
