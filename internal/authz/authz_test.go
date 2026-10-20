package authz

import (
	"errors"
	"testing"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// The org half of the spec's role table, written out so a change to the
// matrix has to change this test too.
func TestOrgMatrix(t *testing.T) {
	all := []Action{ViewOrg, ViewMembers, ManageMembers, ViewInvites, GrantOwner, UpdateOrg, DeleteOrg,
		CreateEvent, ViewAudit, AllEvents, ManageTemplates}
	want := map[string][]Action{
		domain.OrgRoleOwner:  all,
		domain.OrgRoleAdmin:  {ViewOrg, ViewMembers, ManageMembers, ViewInvites, UpdateOrg, CreateEvent, ViewAudit, AllEvents, ManageTemplates},
		domain.OrgRoleMember: {ViewOrg, ViewMembers, CreateEvent, ManageTemplates},
		domain.OrgRoleGuest:  {ViewOrg}, // shared one event: sees the org's name, nothing else
		"":                   nil,
		"stranger":           nil,
	}
	for role, allowed := range want {
		ok := map[Action]bool{}
		for _, a := range allowed {
			ok[a] = true
		}
		for _, a := range all {
			if got := Can(role, a); got != ok[a] {
				t.Errorf("Can(%q, %s) = %v, want %v", role, a, got, ok[a])
			}
		}
	}
}

func TestZeroScopeAllowsNothing(t *testing.T) {
	var sc OrgScope
	if err := sc.Require(ViewOrg); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("zero scope: %v, want ErrForbidden", err)
	}
	owner := ForMember(NewAccountScope("a"), "o", domain.OrgRoleOwner)
	if err := owner.Require(DeleteOrg); err != nil {
		t.Fatalf("owner DeleteOrg: %v", err)
	}
}

// The spec's event role table, cell by cell. "if" cells depend on the
// owner's settings and are checked with the setting off and on.
func TestEventMatrixMatchesSpec(t *testing.T) {
	const (
		y   = "yes"
		n   = "no"
		if_ = "if allowed"
	)
	roles := []string{domain.EventRoleOwner, domain.EventRoleEditor, domain.EventRoleCheckin, domain.EventRoleViewer}
	table := []struct {
		action EventAction
		cells  [4]string // owner, editor, check-in staff, viewer
	}{
		{EventView, [4]string{y, y, y, y}},
		{EventLookup, [4]string{y, y, y, y}},     // check-in staff: "lookup only"
		{EventDashboard, [4]string{y, y, n, y}},  // dashboard and participant list
		{EventEdit, [4]string{y, y, n, n}},       // event, form, workshops, checkpoints
		{EventDesignPass, [4]string{y, y, n, n}}, // design or assign the pass template
		{EventScan, [4]string{y, y, y, n}},       // scan and record checkpoints
		{EventUndoAny, [4]string{y, y, n, n}},    // undo a check-in
		{EventUndoOwn, [4]string{y, y, y, n}},    // own scans, within 30 seconds
		{EventEmail, [4]string{y, y, n, n}},      // email participants, announcements
		{EventExport, [4]string{y, y, n, if_}},   // export data
		{EventShare, [4]string{y, if_, n, n}},    // share and change roles
		{EventViewAudit, [4]string{y, y, n, n}},  // activity log per event
		{EventTransfer, [4]string{y, n, n, n}},   // transfer ownership
		{EventDelete, [4]string{y, n, n, n}},     // delete the event
	}
	for _, row := range table {
		for i, role := range roles {
			off := CanEvent(role, row.action, domain.EventSettings{})
			on := CanEvent(role, row.action, domain.EventSettings{EditorsCanShare: true, ViewersCanExport: true})
			var got string
			switch {
			case off && on:
				got = y
			case !off && on:
				got = if_
			case !off && !on:
				got = n
			default:
				got = "only when the setting is off"
			}
			if got != row.cells[i] {
				t.Errorf("%s / %s: %s, spec says %s", row.action, role, got, row.cells[i])
			}
		}
	}
}

func TestEffectiveEventRole(t *testing.T) {
	cases := []struct {
		orgRole, share string
		owner          bool
		want           string
	}{
		{domain.OrgRoleOwner, "", false, domain.EventRoleOwner},                      // implicit owner of every event
		{domain.OrgRoleAdmin, "", false, domain.EventRoleEditor},                     // implicit editor
		{domain.OrgRoleAdmin, domain.EventRoleViewer, false, domain.EventRoleEditor}, // strongest wins
		{domain.OrgRoleMember, "", false, ""},                                        // members see only own and shared
		{domain.OrgRoleMember, "", true, domain.EventRoleOwner},
		{domain.OrgRoleMember, domain.EventRoleCheckin, false, domain.EventRoleCheckin},
		{domain.OrgRoleGuest, domain.EventRoleViewer, false, domain.EventRoleViewer},
		{domain.OrgRoleGuest, "", false, ""},
	}
	for _, c := range cases {
		if got := EffectiveEventRole(c.orgRole, c.owner, c.share); got != c.want {
			t.Errorf("EffectiveEventRole(%s, owner=%v, share=%s) = %q, want %q", c.orgRole, c.owner, c.share, got, c.want)
		}
	}
	// Org admins manage sharing even where editors may not.
	admin := ForMember(NewAccountScope("a"), "o", domain.OrgRoleAdmin)
	es, _ := ForEvent(admin, "e", "someone", "", domain.EventSettings{})
	if !es.Can(EventShare) || es.Can(EventTransfer) {
		t.Error("org admin: want share yes, transfer no")
	}
}
