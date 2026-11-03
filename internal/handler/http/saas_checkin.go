package httphandler

// Check-in, workshops and the live dashboard (milestone M4).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/service"
)

func init() {
	eventRoutes = append(eventRoutes, []eventRoute{
		{"GET", ev + "/checkpoints", true, authz.EventLookup, "", (*Handler).apiCheckpoints},
		{"POST", ev + "/checkpoints", true, authz.EventEdit, "checkpoint.add", (*Handler).apiAddCheckpoint},
		{"PATCH", ev + "/checkpoints/{checkpoint}", true, authz.EventEdit, "checkpoint.update", (*Handler).apiUpdateCheckpoint},
		{"POST", ev + "/checkpoints/{checkpoint}/archive", true, authz.EventEdit, "checkpoint.archive", (*Handler).apiArchiveCheckpoint},
		{"POST", ev + "/checkpoints/{checkpoint}/restore", true, authz.EventEdit, "checkpoint.restore", (*Handler).apiRestoreCheckpoint},
		{"POST", ev + "/scan", true, authz.EventScan, "", (*Handler).apiScan},
		{"POST", ev + "/checkpoints/{checkpoint}/record", true, authz.EventScan, "", (*Handler).apiRecord},
		{"POST", ev + "/checkins/{checkin}/undo", true, authz.EventUndoOwn, "checkin.undo", (*Handler).apiUndo},
		{"GET", ev + "/checkins/recent", true, authz.EventLookup, "", (*Handler).apiRecent},
		{"GET", ev + "/lookup", true, authz.EventLookup, "", (*Handler).apiLookup},
		{"GET", ev + "/stats", true, authz.EventDashboard, "", (*Handler).apiStats},
		{"GET", ev + "/stream", true, authz.EventLookup, "", (*Handler).apiStream},
		{"GET", ev + "/workshops", true, authz.EventView, "", (*Handler).apiWorkshops},
		{"POST", ev + "/workshops", true, authz.EventEdit, "workshop.add", (*Handler).apiAddWorkshop},
		{"PATCH", ev + "/workshops/{workshop}", true, authz.EventEdit, "workshop.update", (*Handler).apiUpdateWorkshop},
		{"DELETE", ev + "/workshops/{workshop}", true, authz.EventEdit, "workshop.remove", (*Handler).apiDeleteWorkshop},
		{"GET", ev + "/workshops/{workshop}/signups", true, authz.EventDashboard, "", (*Handler).apiSignups},
		{"POST", ev + "/workshops/{workshop}/signups", true, authz.EventScan, "workshop.signup.add", (*Handler).apiAddSignup},
		{"DELETE", ev + "/workshops/{workshop}/signups/{participant}", true, authz.EventEdit, "workshop.signup.cancel", (*Handler).apiCancelSignup},

		{"GET", evPage + "/scan", false, authz.EventScan, "", (*Handler).scannerPage},
		{"GET", evPage + "/dashboard", false, authz.EventDashboard, "", (*Handler).dashboardPage},
		{"GET", evPage + "/checkin", false, authz.EventEdit, "", (*Handler).checkinSettingsPage},
		{"POST", evPage + "/checkpoints", false, authz.EventEdit, "checkpoint.add", (*Handler).addCheckpointPost},
		{"POST", evPage + "/checkpoints/{checkpoint}", false, authz.EventEdit, "checkpoint.update", (*Handler).updateCheckpointPost},
		{"POST", evPage + "/checkpoints/{checkpoint}/archive", false, authz.EventEdit, "checkpoint.archive", (*Handler).archiveCheckpointPost},
		{"POST", evPage + "/checkpoints/{checkpoint}/restore", false, authz.EventEdit, "checkpoint.restore", (*Handler).restoreCheckpointPost},
		{"GET", evPage + "/workshops", false, authz.EventView, "", (*Handler).workshopsPage},
		{"POST", evPage + "/workshops", false, authz.EventEdit, "workshop.add", (*Handler).addWorkshopPost},
		{"POST", evPage + "/workshops/{workshop}", false, authz.EventEdit, "workshop.update", (*Handler).updateWorkshopPost},
		{"POST", evPage + "/workshops/{workshop}/remove", false, authz.EventEdit, "workshop.remove", (*Handler).removeWorkshopPost},
	}...)
}

// =============================================================================
// JSON API
// =============================================================================

func (h *Handler) apiCheckpoints(w http.ResponseWriter, r *http.Request, e *eventReq) {
	cps, err := h.Checkins.Checkpoints(r.Context(), e.es, r.URL.Query().Get("archived") != "")
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cps)
}

func (h *Handler) apiAddCheckpoint(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var c domain.Checkpoint
	if !decodeJSON(w, r, 8<<10, &c) {
		return
	}
	c.ID = ""
	out, err := h.Checkins.SaveCheckpoint(r.Context(), e.es, c)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) apiUpdateCheckpoint(w http.ResponseWriter, r *http.Request, e *eventReq) {
	cur, err := h.checkpoint(r, e)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	if !decodeJSON(w, r, 8<<10, cur) {
		return
	}
	cur.ID = r.PathValue("checkpoint")
	out, err := h.Checkins.SaveCheckpoint(r.Context(), e.es, *cur)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) checkpoint(r *http.Request, e *eventReq) (*domain.Checkpoint, error) {
	cps, err := h.Checkins.Checkpoints(r.Context(), e.es, true)
	if err != nil {
		return nil, err
	}
	for i := range cps {
		if cps[i].ID == r.PathValue("checkpoint") {
			return &cps[i], nil
		}
	}
	return nil, domain.ErrNotFound
}

func (h *Handler) apiArchiveCheckpoint(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Checkins.SetCheckpointArchived(r.Context(), e.es, r.PathValue("checkpoint"), true))
}

func (h *Handler) apiRestoreCheckpoint(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Checkins.SetCheckpointArchived(r.Context(), e.es, r.PathValue("checkpoint"), false))
}

// POST /…/scan {payload, checkpoint_id, record, idempotency_key, device_id, client_time}
func (h *Handler) apiScan(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var req service.ScanRequest
	if !decodeJSON(w, r, 8<<10, &req) {
		return
	}
	res, err := h.Checkins.Scan(r.Context(), e.es, req)
	h.scanReply(w, r, e, res, err)
}

func (h *Handler) apiRecord(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var req service.RecordRequest
	if !decodeJSON(w, r, 8<<10, &req) {
		return
	}
	res, err := h.Checkins.Record(r.Context(), e.es, r.PathValue("checkpoint"), req)
	h.scanReply(w, r, e, res, err)
}

func (h *Handler) scanReply(w http.ResponseWriter, r *http.Request, e *eventReq, res *service.ScanResult, err error) {
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	if res.Result == domain.ScanRecorded && h.LiveFeed != nil {
		h.LiveFeed.Notify(e.event.ID) // dashboards on this server update at once
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) apiUndo(w http.ResponseWriter, r *http.Request, e *eventReq) {
	err := h.Checkins.Undo(r.Context(), e.es, r.PathValue("checkin"))
	if err == nil && h.LiveFeed != nil {
		h.LiveFeed.Notify(e.event.ID)
	}
	noContent(w, r, err)
}

func (h *Handler) apiRecent(w http.ResponseWriter, r *http.Request, e *eventReq) {
	rows, err := h.Checkins.Recent(r.Context(), e.es)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *Handler) apiLookup(w http.ResponseWriter, r *http.Request, e *eventReq) {
	list, err := h.Checkins.Lookup(r.Context(), e.es, r.URL.Query().Get("q"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) apiStats(w http.ResponseWriter, r *http.Request, e *eventReq) {
	st, err := h.Checkins.Stats(r.Context(), e.es)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, st)
}

// GET /…/stream: Server-Sent Events. "change" means the event's numbers
// moved; clients fetch what they show. A comment every 25 s keeps proxies
// from closing the connection.
func (h *Handler) apiStream(w http.ResponseWriter, r *http.Request, e *eventReq) {
	if h.LiveFeed == nil {
		writeError(w, http.StatusServiceUnavailable, "live updates are off")
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // long-lived by design
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	_ = rc.Flush()

	ch, stop := h.LiveFeed.Subscribe(e.event.ID)
	defer stop()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprint(w, "event: change\ndata: {}\n\n")
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

func (h *Handler) apiWorkshops(w http.ResponseWriter, r *http.Request, e *eventReq) {
	list, err := h.Checkins.Workshops(r.Context(), e.es)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) apiAddWorkshop(w http.ResponseWriter, r *http.Request, e *eventReq) {
	ws := domain.Workshop{Waitlist: true} // fields left out keep the form's defaults
	if !decodeJSON(w, r, 16<<10, &ws) {
		return
	}
	ws.ID = ""
	out, err := h.Checkins.SaveWorkshop(r.Context(), e.es, ws)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) apiUpdateWorkshop(w http.ResponseWriter, r *http.Request, e *eventReq) {
	list, err := h.Checkins.Workshops(r.Context(), e.es)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	var cur *domain.Workshop
	for i := range list {
		if list[i].ID == r.PathValue("workshop") {
			cur = &list[i]
		}
	}
	if cur == nil {
		writeDomainError(w, r, domain.ErrNotFound)
		return
	}
	if !decodeJSON(w, r, 16<<10, cur) {
		return
	}
	cur.ID = r.PathValue("workshop")
	out, err := h.Checkins.SaveWorkshop(r.Context(), e.es, *cur)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) apiDeleteWorkshop(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Checkins.DeleteWorkshop(r.Context(), e.es, r.PathValue("workshop")))
}

func (h *Handler) apiSignups(w http.ResponseWriter, r *http.Request, e *eventReq) {
	list, err := h.Checkins.Signups(r.Context(), e.es, r.PathValue("workshop"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// POST /…/workshops/{workshop}/signups {participant_id}: add someone, at the
// door or from the dashboard, if there's room.
func (h *Handler) apiAddSignup(w http.ResponseWriter, r *http.Request, e *eventReq) {
	var req struct {
		ParticipantID string `json:"participant_id"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	status, err := h.Checkins.AddToWorkshop(r.Context(), e.es, r.PathValue("workshop"), req.ParticipantID)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (h *Handler) apiCancelSignup(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Checkins.CancelSignup(r.Context(), e.es, r.PathValue("workshop"), r.PathValue("participant")))
}

// =============================================================================
// Pages
// =============================================================================

type scannerData struct {
	eventPageData
	Checkpoints []domain.Checkpoint
	APIBase     string
	CanUndoAny  bool
}

func (h *Handler) scannerPage(w http.ResponseWriter, r *http.Request, e *eventReq) {
	cps, err := h.Checkins.Checkpoints(r.Context(), e.es, false)
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	d := scannerData{eventPageData: h.eventData(e, "scan"), Checkpoints: cps,
		APIBase: "/api/v1/orgs/" + e.org.Slug + "/events/" + e.event.ID, CanUndoAny: e.es.Can(authz.EventUndoAny)}
	h.render(w, http.StatusOK, "scanner", e.principal, view{Title: "Scan · " + e.event.Name, Data: d})
}

type dashboardData struct {
	eventPageData
	Stats   *domain.EventStats
	Recent  []domain.CheckinRow
	APIBase string
	// For the server-rendered bars (the page then updates live).
	MaxBucket int
	AgeOn     string // the under-18 cut-off, as shown
}

func (h *Handler) dashboardPage(w http.ResponseWriter, r *http.Request, e *eventReq) {
	st, err := h.Checkins.Stats(r.Context(), e.es)
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	recent, err := h.Checkins.Recent(r.Context(), e.es)
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	d := dashboardData{eventPageData: h.eventData(e, "dashboard"), Stats: st, Recent: recent,
		APIBase: "/api/v1/orgs/" + e.org.Slug + "/events/" + e.event.ID}
	for _, b := range st.PerTenMinutes {
		d.MaxBucket = max(d.MaxBucket, b.Count)
	}
	if day := service.AgeOn(e.event); !day.IsZero() {
		d.AgeOn = day.Format("2 Jan 2006")
	}
	h.render(w, http.StatusOK, "event_dashboard", e.principal, view{Title: "Dashboard · " + e.event.Name, Data: d})
}

type checkinSettingsData struct {
	eventPageData
	Checkpoints []domain.Checkpoint
	ScanURL     string
}

// Names returns checkpoint names for the "requires" display.
func (d checkinSettingsData) Name(id string) string {
	for _, c := range d.Checkpoints {
		if c.ID == id {
			return c.Name
		}
	}
	return "a removed checkpoint"
}

var checkinNotices = map[string]string{
	"added": "Checkpoint added.", "saved": "Checkpoint saved.", "archived": "Checkpoint closed: scanners no longer offer it.",
	"restored": "Checkpoint reopened.",
}

func (h *Handler) checkinSettingsPage(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.renderCheckinSettings(w, r, e, http.StatusOK, "", checkinNotices[r.URL.Query().Get("notice")])
}

func (h *Handler) renderCheckinSettings(w http.ResponseWriter, r *http.Request, e *eventReq, status int, errMsg, notice string) {
	cps, err := h.Checkins.Checkpoints(r.Context(), e.es, true)
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	d := checkinSettingsData{eventPageData: h.eventData(e, "checkin"), Checkpoints: cps,
		ScanURL: "/o/" + e.org.Slug + "/events/" + e.event.ID + "/scan"}
	h.render(w, status, "event_checkin", e.principal, view{Title: "Check-in · " + e.event.Name, Error: errMsg, Notice: notice, Data: d})
}

func (h *Handler) checkinResult(w http.ResponseWriter, r *http.Request, e *eventReq, notice string, err error) {
	if err != nil {
		status, msg := pageErrorStatus(err)
		if status == http.StatusInternalServerError || status == http.StatusNotFound {
			h.renderError(w, r, e.principal, err)
			return
		}
		h.renderCheckinSettings(w, r, e, status, msg, "")
		return
	}
	h.backToEvent(w, r, e, "checkin", notice)
}

// checkpointFromForm reads the checkpoint form: name, rules.
func checkpointFromForm(r *http.Request, tz string) (domain.Checkpoint, error) {
	c := domain.Checkpoint{Name: r.PostFormValue("name")}
	c.Rules.Repeatable = r.PostFormValue("repeatable") == "on"
	c.Rules.Requires = r.PostForm["requires"]
	c.Rules.Roles = r.PostForm["roles"]
	var err error
	if c.Rules.OpensAt, err = service.ParseLocalTime(r.PostFormValue("opens"), tz); err != nil {
		return c, err
	}
	c.Rules.ClosesAt, err = service.ParseLocalTime(r.PostFormValue("closes"), tz)
	return c, err
}

func (h *Handler) addCheckpointPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	r.ParseForm()
	c, err := checkpointFromForm(r, e.event.TimeZone)
	if err == nil {
		_, err = h.Checkins.SaveCheckpoint(r.Context(), e.es, c)
	}
	h.checkinResult(w, r, e, "added", err)
}

func (h *Handler) updateCheckpointPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	r.ParseForm()
	c, err := checkpointFromForm(r, e.event.TimeZone)
	if err == nil {
		c.ID = r.PathValue("checkpoint")
		_, err = h.Checkins.SaveCheckpoint(r.Context(), e.es, c)
	}
	h.checkinResult(w, r, e, "saved", err)
}

func (h *Handler) archiveCheckpointPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.checkinResult(w, r, e, "archived", h.Checkins.SetCheckpointArchived(r.Context(), e.es, r.PathValue("checkpoint"), true))
}

func (h *Handler) restoreCheckpointPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.checkinResult(w, r, e, "restored", h.Checkins.SetCheckpointArchived(r.Context(), e.es, r.PathValue("checkpoint"), false))
}

type workshopForm struct {
	ID, Title, Description, Speakers, Room, Starts, Ends, Capacity, SignupMode string
	Waitlist                                                                   bool
}

type workshopsData struct {
	eventPageData
	Workshops []domain.Workshop
	Signups   map[string][]domain.WorkshopSignup
	Forms     map[string]workshopForm // per workshop ID, "" for the new one
}

func workshopToForm(ws domain.Workshop, tz string) workshopForm {
	f := workshopForm{ID: ws.ID, Title: ws.Title, Description: ws.Description, Speakers: ws.Speakers, Room: ws.Room,
		SignupMode: ws.SignupMode, Waitlist: ws.Waitlist, Starts: localValue(ws.StartsAt, tz), Ends: localValue(ws.EndsAt, tz)}
	if ws.Capacity != nil {
		f.Capacity = strconv.Itoa(*ws.Capacity)
	}
	return f
}

var workshopNotices = map[string]string{"added": "Workshop added, with its own attendance checkpoint.",
	"saved": "Workshop saved.", "removed": "Workshop removed."}

func (h *Handler) workshopsPage(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.renderWorkshops(w, r, e, http.StatusOK, "", workshopNotices[r.URL.Query().Get("notice")], nil)
}

func (h *Handler) renderWorkshops(w http.ResponseWriter, r *http.Request, e *eventReq, status int, errMsg, notice string, failed *workshopForm) {
	list, err := h.Checkins.Workshops(r.Context(), e.es)
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	d := workshopsData{eventPageData: h.eventData(e, "workshops"), Workshops: list,
		Signups: map[string][]domain.WorkshopSignup{}, Forms: map[string]workshopForm{"": {SignupMode: domain.SignupOptIn, Waitlist: true}}}
	for _, ws := range list {
		d.Forms[ws.ID] = workshopToForm(ws, e.event.TimeZone)
		if e.es.Can(authz.EventDashboard) {
			if d.Signups[ws.ID], err = h.Checkins.Signups(r.Context(), e.es, ws.ID); err != nil {
				h.renderError(w, r, e.principal, err)
				return
			}
		}
	}
	if failed != nil {
		d.Forms[failed.ID] = *failed
	}
	h.render(w, status, "event_workshops", e.principal, view{Title: "Workshops · " + e.event.Name, Error: errMsg, Notice: notice, Data: d})
}

func workshopFromForm(r *http.Request, tz string) (domain.Workshop, workshopForm, error) {
	f := workshopForm{Title: r.PostFormValue("title"), Description: r.PostFormValue("description"),
		Speakers: r.PostFormValue("speakers"), Room: r.PostFormValue("room"), Starts: r.PostFormValue("starts"),
		Ends: r.PostFormValue("ends"), Capacity: strings.TrimSpace(r.PostFormValue("capacity")),
		SignupMode: r.PostFormValue("signup_mode"), Waitlist: r.PostFormValue("waitlist") == "on"}
	ws := domain.Workshop{Title: f.Title, Description: f.Description, Speakers: f.Speakers, Room: f.Room,
		SignupMode: f.SignupMode, Waitlist: f.Waitlist}
	var err error
	if ws.StartsAt, err = service.ParseLocalTime(f.Starts, tz); err != nil {
		return ws, f, err
	}
	if ws.EndsAt, err = service.ParseLocalTime(f.Ends, tz); err != nil {
		return ws, f, err
	}
	if f.Capacity != "" {
		n, err := strconv.Atoi(f.Capacity)
		if err != nil {
			return ws, f, fmt.Errorf("%w: capacity must be a whole number", domain.ErrValidation)
		}
		ws.Capacity = &n
	}
	return ws, f, nil
}

func (h *Handler) workshopResult(w http.ResponseWriter, r *http.Request, e *eventReq, notice string, f *workshopForm, err error) {
	if err != nil {
		status, msg := pageErrorStatus(err)
		if status == http.StatusInternalServerError || status == http.StatusNotFound {
			h.renderError(w, r, e.principal, err)
			return
		}
		h.renderWorkshops(w, r, e, status, msg, "", f)
		return
	}
	h.backToEvent(w, r, e, "workshops", notice)
}

func (h *Handler) addWorkshopPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	ws, f, err := workshopFromForm(r, e.event.TimeZone)
	if err == nil {
		_, err = h.Checkins.SaveWorkshop(r.Context(), e.es, ws)
	}
	h.workshopResult(w, r, e, "added", &f, err)
}

func (h *Handler) updateWorkshopPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	ws, f, err := workshopFromForm(r, e.event.TimeZone)
	ws.ID, f.ID = r.PathValue("workshop"), r.PathValue("workshop")
	if err == nil {
		_, err = h.Checkins.SaveWorkshop(r.Context(), e.es, ws)
	}
	h.workshopResult(w, r, e, "saved", &f, err)
}

func (h *Handler) removeWorkshopPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.workshopResult(w, r, e, "removed", nil, h.Checkins.DeleteWorkshop(r.Context(), e.es, r.PathValue("workshop")))
}

// =============================================================================
// Portal: a participant's workshops
// =============================================================================

func (h *Handler) myWorkshopSignUp(w http.ResponseWriter, r *http.Request, p *principal) {
	h.myWorkshopAction(w, r, p, true)
}

func (h *Handler) myWorkshopCancel(w http.ResponseWriter, r *http.Request, p *principal) {
	h.myWorkshopAction(w, r, p, false)
}

func (h *Handler) myWorkshopAction(w http.ResponseWriter, r *http.Request, p *principal, signUp bool) {
	pc, err := h.Portal.MyPass(r.Context(), p.scope(), r.PathValue("participant"))
	if err != nil {
		h.renderError(w, r, p, err)
		return
	}
	notice := "cancelled"
	if signUp {
		var status string
		status, err = h.Checkins.MySignUp(r.Context(), p.scope(), pc, r.PathValue("workshop"))
		notice = status
	} else {
		err = h.Checkins.MyCancel(r.Context(), p.scope(), pc, r.PathValue("workshop"))
	}
	if err != nil {
		h.renderMyPass(w, r, p, pc, err)
		return
	}
	http.Redirect(w, r, "/me/passes/"+pc.Participant.ID+"?notice="+notice, http.StatusSeeOther)
}

// jsonText is for embedding small JSON in templates (data attributes).
func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
