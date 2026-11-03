// Package authz holds the SaaS role matrix and the scopes every tenant query
// must carry.
//
// Wall 1 of tenant isolation: repository functions for tenant data take an
// OrgScope, and an OrgScope is only built from a membership that was looked
// up for the signed-in account, so a handler can't query an org by ID alone.
package authz

import "github.com/mayura-andrew/event-pass-platform/internal/domain"

// Action is something a member may do inside an organization.
type Action string

const (
	ViewOrg       Action = "org.view"
	ViewMembers   Action = "org.members.view"
	ManageMembers Action = "org.members.manage" // invite, change roles, remove
	ViewInvites   Action = "org.invites.view"
	GrantOwner    Action = "org.owner.grant" // make someone an owner, or change an owner
	UpdateOrg     Action = "org.update"      // name, branding, sharing switches
	DeleteOrg     Action = "org.delete"
	CreateEvent   Action = "org.events.create"
	// ManageTemplates: create and edit the org's pass templates. Members
	// create events, so they design their passes too.
	ManageTemplates Action = "org.templates.manage"
	ViewAudit       Action = "org.audit.view"
	// AllEvents: implicit access to every event in the org (owner: as owner,
	// admin: as editor). See ImplicitEventRole.
	AllEvents Action = "org.events.all"
)

// orgMatrix is the org half of the spec's role table.
var orgMatrix = map[string]map[Action]bool{
	domain.OrgRoleOwner: {
		ViewOrg: true, ViewMembers: true, ManageMembers: true, ViewInvites: true,
		GrantOwner: true, UpdateOrg: true, DeleteOrg: true, CreateEvent: true, ViewAudit: true, AllEvents: true,
		ManageTemplates: true,
	},
	domain.OrgRoleAdmin: {
		ViewOrg: true, ViewMembers: true, ManageMembers: true, ViewInvites: true,
		UpdateOrg: true, CreateEvent: true, ViewAudit: true, AllEvents: true, ManageTemplates: true,
	},
	domain.OrgRoleMember: {
		ViewOrg: true, ViewMembers: true, CreateEvent: true, ManageTemplates: true,
	},
	// Someone outside the org who was shared an event: sees the org's name
	// and those events, nothing else.
	domain.OrgRoleGuest: {
		ViewOrg: true,
	},
}

// Can reports whether an org role allows an action.
func Can(role string, a Action) bool { return orgMatrix[role][a] }

// AccountScope is a signed-in account acting outside any one org (its own
// org list, creating an org, accepting an invite).
type AccountScope struct {
	accountID string
}

func NewAccountScope(accountID string) AccountScope { return AccountScope{accountID: accountID} }

func (s AccountScope) AccountID() string { return s.accountID }

// OrgScope is a signed-in account acting inside one org it belongs to.
type OrgScope struct {
	orgID     string
	accountID string
	role      string
}

// ForMember builds the scope for a membership the store returned for this
// account. It is the only way to get an OrgScope.
func ForMember(account AccountScope, orgID, role string) OrgScope {
	return OrgScope{orgID: orgID, accountID: account.accountID, role: role}
}

func (s OrgScope) OrgID() string     { return s.orgID }
func (s OrgScope) AccountID() string { return s.accountID }
func (s OrgScope) Role() string      { return s.role }

// Can reports whether this member may perform a.
func (s OrgScope) Can(a Action) bool { return Can(s.role, a) }

// Require returns domain.ErrForbidden unless this member may perform a.
func (s OrgScope) Require(a Action) error {
	if s.orgID == "" || !s.Can(a) {
		return domain.ErrForbidden
	}
	return nil
}

// ---- Events --------------------------------------------------------------------

// EventAction is something a person may do with one event.
type EventAction string

const (
	EventView       EventAction = "event.view"       // see it in lists, open its overview
	EventLookup     EventAction = "event.lookup"     // look up one participant (scanner)
	EventDashboard  EventAction = "event.dashboard"  // dashboard and participant list
	EventEdit       EventAction = "event.edit"       // event, form, workshops, checkpoints, archive
	EventDesignPass EventAction = "event.pass"       // design or assign the pass template
	EventScan       EventAction = "event.scan"       // scan and record checkpoints
	EventUndoAny    EventAction = "event.undo.any"   // undo anyone's check-in
	EventUndoOwn    EventAction = "event.undo.own"   // undo own scans within 30 s
	EventEmail      EventAction = "event.email"      // email participants, announcements
	EventExport     EventAction = "event.export"     // export data
	EventShare      EventAction = "event.share"      // share and change roles
	EventViewAudit  EventAction = "event.audit.view" // the event's activity log
	EventTransfer   EventAction = "event.transfer"   // transfer ownership
	EventDelete     EventAction = "event.delete"     // move to Trash, restore
)

// EventRoles from most to least access.
var EventRoles = []string{domain.EventRoleOwner, domain.EventRoleEditor, domain.EventRoleCheckin, domain.EventRoleViewer}

func roleRank(r string) int {
	for i, x := range EventRoles {
		if x == r {
			return len(EventRoles) - i
		}
	}
	return 0
}

// eventMatrix is the spec's event role table. Two cells depend on the
// event's settings and are applied in EventScope.Can: editors share only if
// the owner allows, viewers export only if the owner allows.
var eventMatrix = map[string]map[EventAction]bool{
	domain.EventRoleOwner: {
		EventView: true, EventLookup: true, EventDashboard: true, EventEdit: true, EventDesignPass: true,
		EventScan: true, EventUndoAny: true, EventUndoOwn: true, EventEmail: true, EventExport: true,
		EventShare: true, EventViewAudit: true, EventTransfer: true, EventDelete: true,
	},
	domain.EventRoleEditor: {
		EventView: true, EventLookup: true, EventDashboard: true, EventEdit: true, EventDesignPass: true,
		EventScan: true, EventUndoAny: true, EventUndoOwn: true, EventEmail: true, EventExport: true,
		EventShare: true, EventViewAudit: true,
	},
	domain.EventRoleCheckin: {
		EventView: true, EventLookup: true, EventScan: true, EventUndoOwn: true,
	},
	domain.EventRoleViewer: {
		EventView: true, EventLookup: true, EventDashboard: true, EventExport: true,
	},
}

// EventSettings are the owner's switches that change the matrix.
type EventSettings = domain.EventSettings

// CanEvent reports whether an event role allows an action under settings.
func CanEvent(role string, a EventAction, st EventSettings) bool {
	if !eventMatrix[role][a] {
		return false
	}
	switch {
	case role == domain.EventRoleEditor && a == EventShare:
		return st.EditorsCanShare
	case role == domain.EventRoleViewer && a == EventExport:
		return st.ViewersCanExport
	}
	return true
}

// ImplicitEventRole is the access an org role gives to every event in the
// org: owners act as event owners, admins as editors.
func ImplicitEventRole(orgRole string) string {
	switch orgRole {
	case domain.OrgRoleOwner:
		return domain.EventRoleOwner
	case domain.OrgRoleAdmin:
		return domain.EventRoleEditor
	}
	return ""
}

// EffectiveEventRole combines every source of access to one event and
// returns the strongest, or "" for none.
func EffectiveEventRole(orgRole string, isOwner bool, shareRole string) string {
	best := ImplicitEventRole(orgRole)
	if isOwner && roleRank(domain.EventRoleOwner) > roleRank(best) {
		best = domain.EventRoleOwner
	}
	if roleRank(shareRole) > roleRank(best) {
		best = shareRole
	}
	return best
}

// EventScope is a signed-in account acting on one event it can open.
type EventScope struct {
	org      OrgScope
	eventID  string
	role     string
	settings EventSettings
	// Org admins may manage shares even where editors may not (spec: sharing
	// is for the event editor or an org admin).
	orgManager bool
}

// ForEvent builds the scope for an event the store returned in this org,
// given who owns it and the caller's share. ok is false when the caller has
// no access, which callers report as not found.
func ForEvent(org OrgScope, eventID, ownerID, shareRole string, st EventSettings) (EventScope, bool) {
	role := EffectiveEventRole(org.role, ownerID == org.accountID, shareRole)
	if role == "" || org.orgID == "" {
		return EventScope{}, false
	}
	return EventScope{org: org, eventID: eventID, role: role, settings: st,
		orgManager: org.Can(ManageMembers)}, true
}

func (s EventScope) Org() OrgScope           { return s.org }
func (s EventScope) OrgID() string           { return s.org.orgID }
func (s EventScope) AccountID() string       { return s.org.accountID }
func (s EventScope) EventID() string         { return s.eventID }
func (s EventScope) Role() string            { return s.role }
func (s EventScope) Settings() EventSettings { return s.settings }

// Can reports whether the caller may perform a on this event.
func (s EventScope) Can(a EventAction) bool {
	if s.eventID == "" {
		return false
	}
	if s.orgManager && a == EventShare {
		return true
	}
	return CanEvent(s.role, a, s.settings)
}

// Require returns domain.ErrForbidden unless the caller may perform a.
func (s EventScope) Require(a EventAction) error {
	if !s.Can(a) {
		return domain.ErrForbidden
	}
	return nil
}
