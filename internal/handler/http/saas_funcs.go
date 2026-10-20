package httphandler

import (
	"fmt"
	"html/template"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// pageFuncs are the helpers page templates may call.
var pageFuncs = template.FuncMap{
	"statusLabel": func(s string) string { return statusLabels[s] },
	"statusIcon":  func(s string) string { return statusIcons[s] },
	"roleLabel":   roleLabel,
	"typeLabel":   func(t string) string { return domain.EventTypeLabels[t] },
	"describe":    describeAudit,
	"when":        func(t time.Time) string { return t.UTC().Format("2 Jan 2006, 15:04 UTC") },
	"whenPtr": func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format("2 Jan 2006")
	},
	"initial": func(name, email string) string {
		s := strings.TrimSpace(name)
		if s == "" {
			s = email
		}
		r, _ := utf8.DecodeRuneInString(s)
		return strings.ToUpper(string(r))
	},
	// dict builds a map for passing several values to a partial.
	"dict": func(kv ...any) map[string]any {
		m := make(map[string]any, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
	"plural": plural,
	// localTime formats a time as a datetime-local value in a time zone.
	"localTime": func(t *time.Time, tz string) string { return localValue(t, tz) },
	// humanTime shows a datetime-local value as "Sat 3 Oct, 14:00".
	"humanTime": func(v string) string {
		t, err := time.Parse("2006-01-02T15:04", v)
		if err != nil {
			return v
		}
		return t.Format("Mon 2 Jan, 15:04")
	},
	"list":     func(items ...string) []string { return items },
	"add":      func(a, b int) int { return a + b },
	"jsonText": jsonText,
	"deref": func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	},
	// dots joins the non-empty parts with " · ".
	"dots": func(parts ...string) string {
		var out []string
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return strings.Join(out, " · ")
	},
	// whenLocal is a time in a zone as "Mon 2 Jan, 15:04", or "".
	"whenLocal": func(t *time.Time, tz string) string {
		if t == nil {
			return ""
		}
		v, _ := time.Parse("2006-01-02T15:04", localValue(t, tz))
		return v.Format("Mon 2 Jan, 15:04")
	},
	// statusWord is a participant status as people say it.
	"statusWord": func(s string) string {
		return map[string]string{domain.PartPending: "Waiting for approval", domain.PartRegistered: "Registered",
			domain.PartWaitlisted: "Waitlist", domain.PartCancelled: "Cancelled"}[s]
	},
	// inputType is the HTML input type for a form field type.
	"inputType": func(t string) string {
		if it, ok := map[string]string{"email": "email", "phone": "tel", "number": "number", "date": "date", "url": "url"}[t]; ok {
			return it
		}
		return "text"
	},
}

// plural writes "1 place" or "3 places".
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Status chips show color, text and an icon together (spec).
var statusLabels = map[string]string{
	domain.StatusDraft: "Draft", domain.StatusPublished: "Published", domain.StatusLive: "Live",
	domain.StatusFinished: "Finished", domain.StatusArchived: "Archived", domain.StatusTrash: "In Trash",
}

var statusIcons = map[string]string{
	domain.StatusDraft: "✎", domain.StatusPublished: "●", domain.StatusLive: "◉",
	domain.StatusFinished: "✓", domain.StatusArchived: "▣", domain.StatusTrash: "✕",
}

func roleLabel(role string) string {
	switch role {
	case domain.EventRoleOwner:
		return "Owner"
	case domain.EventRoleEditor:
		return "Editor"
	case domain.EventRoleCheckin:
		return "Check-in staff"
	case domain.EventRoleViewer:
		return "Viewer"
	case domain.OrgRoleAdmin:
		return "Admin"
	case domain.OrgRoleMember:
		return "Member"
	case domain.OrgRoleGuest:
		return "Guest"
	}
	return role
}

// describeAudit turns an audit entry into the rest of a sentence that
// starts with the actor's name.
func describeAudit(e domain.AuditEntry) string {
	str := func(k string) string {
		if v, ok := e.Diff[k].(string); ok {
			return v
		}
		return ""
	}
	role := func(k string) string { return strings.ToLower(roleLabel(str(k))) }
	switch e.Action {
	case "org.create":
		return "created the organization"
	case "org.settings":
		if on, _ := e.Diff["external_sharing"].(bool); on {
			return "allowed sharing events outside the organization"
		}
		return "turned off sharing events outside the organization"
	case "member.role":
		return fmt.Sprintf("changed %s from %s to %s", str("email"), role("from"), role("to"))
	case "member.remove":
		return fmt.Sprintf("removed %s from the organization", str("email"))
	case "member.leave":
		return "left the organization"
	case "invite.create":
		return fmt.Sprintf("invited %s to join as %s", str("email"), role("role"))
	case "event.invite.create":
		return fmt.Sprintf("invited %s to the event as %s", str("email"), role("role"))
	case "invite.revoke":
		return fmt.Sprintf("withdrew the invite to %s", str("email"))
	case "invite.accept":
		return fmt.Sprintf("accepted an invite as %s", role("role"))
	case "event.create":
		return "created the event"
	case "event.duplicate":
		return fmt.Sprintf("created it as a copy of %s", str("from_name"))
	case "event.update":
		fields := make([]string, 0, len(e.Diff))
		for _, k := range []string{"name", "type", "description", "dates", "venue", "online_url", "capacity", "visibility", "language", "tags"} {
			if _, ok := e.Diff[k]; ok {
				fields = append(fields, strings.ReplaceAll(k, "_", " "))
			}
		}
		return "edited " + strings.Join(fields, ", ")
	case "event.archive":
		return "archived the event"
	case "event.unarchive":
		return "brought the event back from the archive"
	case "event.delete":
		return "moved the event to Trash"
	case "event.restore":
		return "restored the event from Trash"
	case "event.settings":
		yes := func(k string) string {
			if v, _ := e.Diff[k].(bool); v {
				return "yes"
			}
			return "no"
		}
		return fmt.Sprintf("changed sharing settings (editors can share: %s, viewers can export: %s)",
			yes("editors_can_share"), yes("viewers_can_export"))
	case "event.transfer":
		return fmt.Sprintf("transferred ownership to %s", str("to_email"))
	case "event.share.add":
		return fmt.Sprintf("shared the event with %s as %s", str("email"), role("role"))
	case "event.share.role":
		return fmt.Sprintf("changed %s from %s to %s", str("email"), role("from"), role("to"))
	case "event.share.remove":
		return fmt.Sprintf("removed %s's access", str("email"))
	case "event.link.on":
		if until := str("expires_at"); until != "" {
			if t, err := time.Parse(time.RFC3339, until); err == nil {
				return "turned on link sharing until " + t.Format("2 Jan 2006")
			}
		}
		return "turned on link sharing"
	case "event.link.off":
		return "turned off link sharing"
	}
	return e.Action
}
