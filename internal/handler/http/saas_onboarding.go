package httphandler

// The setup checklist on the events home (milestone M6): the steps from a
// new organization to a first test scan, each ticked off from what exists.

import (
	"net/http"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

func init() {
	orgRoutes = append(orgRoutes, []orgRoute{
		{"POST", "/o/{org}/checklist/hide", false, (*Handler).hideChecklistPost},
		{"POST", "/o/{org}/checklist/show", false, (*Handler).showChecklistPost},
	}...)
}

type checklistStep struct {
	Title, Detail string
	Href          string // where to do it
	Help          string // the help topic
	Done          bool
	IsNext        bool // the first step not done
}

type setupChecklist struct {
	Steps []checklistStep
	Done  int
}

// Next is the first step not done yet.
func (c *setupChecklist) Next() *checklistStep {
	for i := range c.Steps {
		if !c.Steps[i].Done {
			return &c.Steps[i]
		}
	}
	return nil
}

func buildChecklist(slug string, o *domain.Onboarding) *setupChecklist {
	org := "/o/" + slug
	ev := org + "/events/new"
	if o.EventID != "" {
		ev = org + "/events/" + o.EventID
	}
	tab := func(t string) string {
		if o.EventID == "" {
			return ev
		}
		return ev + "/" + t
	}
	c := &setupChecklist{Steps: []checklistStep{
		{"Create an event", "Name, dates and venue. You can change them later.", org + "/events/new", "getting-started", o.HasEvent, false},
		{"Add registration questions", "Ask what you need beyond name and email: food, T-shirt size, team.", tab("registration"), "registration", o.HasQuestions, false},
		{"Design the pass", "Start from a gallery design, or keep the Classic pass.", org + "/pass-templates", "pass-design", o.HasDesign, false},
		{"Publish the event", "Puts the event online so people can register and get their passes.", tab("registration"), "registration", o.Published, false},
		{"Share with check-in staff", "Give the people at the door the Check-in staff role.", tab("share"), "sharing", o.SharedCheckin, false},
		{"Do a test scan", "Register yourself, then scan your own pass on a phone.", tab("scan"), "check-in-day", o.Scanned, false},
	}}
	for _, s := range c.Steps {
		if s.Done {
			c.Done++
		}
	}
	if n := c.Next(); n != nil {
		n.IsNext = true
	}
	return c
}

// checklist is the events home's checklist, or nil when it isn't shown:
// only to owners and admins, until done or hidden.
func (h *Handler) checklist(r *http.Request, o *orgReq) *setupChecklist {
	if !o.sc.Can(authz.UpdateOrg) || o.org.Settings.ChecklistHidden {
		return nil
	}
	ob, err := h.Orgs.Onboarding(r.Context(), o.sc)
	if err != nil {
		return nil // the checklist is a guide; never fail the page over it
	}
	c := buildChecklist(o.org.Slug, ob)
	if c.Done == len(c.Steps) {
		return nil
	}
	return c
}

func (h *Handler) hideChecklistPost(w http.ResponseWriter, r *http.Request, o *orgReq) {
	if err := h.Orgs.SetChecklistHidden(r.Context(), o.sc, o.org, true); err != nil {
		h.orgAction(w, r, o, "", err)
		return
	}
	http.Redirect(w, r, "/o/"+o.org.Slug+"?notice=checklist", http.StatusSeeOther)
}

func (h *Handler) showChecklistPost(w http.ResponseWriter, r *http.Request, o *orgReq) {
	if err := h.Orgs.SetChecklistHidden(r.Context(), o.sc, o.org, false); err != nil {
		h.orgAction(w, r, o, "", err)
		return
	}
	http.Redirect(w, r, "/o/"+o.org.Slug, http.StatusSeeOther)
}
