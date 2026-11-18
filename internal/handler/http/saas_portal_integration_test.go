package httphandler

// Integration test for "My passes" (M3).

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

func TestMyPasses(t *testing.T) {
	env := newSaaSEnv(t)
	starts := time.Now().Add(7 * 24 * time.Hour)
	publish := func(owner *user, slug, name string) string {
		t.Helper()
		org := owner.createOrg(slug)
		base := "/api/v1/orgs/" + org.Slug + "/events"
		_, b := owner.api("POST", base, map[string]any{"name": name, "visibility": "public", "starts_at": starts})
		var ev domain.Event
		json.Unmarshal(b, &ev)
		owner.api("POST", base+"/"+ev.ID+"/fields", map[string]any{"label": "T-shirt size", "type": "text", "visibility": "participant"})
		if status, b := owner.api("POST", base+"/"+ev.ID+"/publish", nil); status != http.StatusNoContent {
			t.Fatalf("publish: %d %s", status, b)
		}
		return "/e/" + org.Slug + "/" + ev.Slug
	}
	ann, bob := env.newBrowser(t, "ann"), env.newBrowser(t, "bob")
	ann.signIn("")
	bob.signIn("")
	pageA := publish(ann, "porta", "Example Event")
	pageB := publish(bob, "portb", "Kandy Robotics Day")

	// Registered without an account, in two orgs.
	gita := env.newBrowser(t, "gita")
	anon := env.newBrowser(t, "anon")
	for _, page := range []string{pageA, pageB} {
		res := anon.form("POST", page+"/register", url.Values{"full_name": {"Gita Perera"}, "email": {gita.email}, "q.t_shirt_size": {"M"}})
		res.Body.Close()
	}

	// Signing in with that email does not link them: anyone could have typed
	// it. They wait as claims, showing no answers, until Gita confirms.
	gita.signIn("")
	if status, b := gita.api("GET", "/api/v1/me/passes", nil); status != http.StatusOK || string(b) != "[]\n" {
		t.Fatalf("passes before claiming: HTTP %d %s, want none", status, b)
	}
	var claims []struct {
		ParticipantID string `json:"participant_id"`
		EventName     string `json:"event_name"`
	}
	_, b := gita.api("GET", "/api/v1/me/claims", nil)
	json.Unmarshal(b, &claims)
	if len(claims) != 2 || strings.Contains(string(b), "\"M\"") {
		t.Fatalf("claims: %s, want 2 without answers", b)
	}
	if page := body(gita.get("/me")); !strings.Contains(page, "This is mine") || strings.Contains(page, "T-shirt") {
		t.Fatal("My passes page doesn't offer the claims, or shows their answers")
	}
	// Another account can't claim or decline Gita's registrations.
	if status, _ := bob.api("POST", "/api/v1/me/claims/"+claims[0].ParticipantID, nil); status != http.StatusNotFound {
		t.Errorf("bob claiming gita's registration: HTTP %d, want 404", status)
	}
	for _, c := range claims {
		if status, b := gita.api("POST", "/api/v1/me/claims/"+c.ParticipantID, nil); status != http.StatusNoContent {
			t.Fatalf("claim: HTTP %d %s", status, b)
		}
	}
	status, b := gita.api("GET", "/api/v1/me/passes", nil)
	var mine []struct {
		domain.EventParticipant
		EventName string `json:"event_name"`
	}
	json.Unmarshal(b, &mine)
	if status != http.StatusOK || len(mine) != 2 {
		t.Fatalf("my passes: HTTP %d, %d passes, want 2: %s", status, len(mine), b)
	}
	res := gita.get("/me")
	page := body(res)
	if !strings.Contains(page, "Example Event") || !strings.Contains(page, "Kandy Robotics Day") {
		t.Fatal("My passes page doesn't list both events")
	}
	passURL := "/me/passes/" + mine[0].ID
	res = gita.get(passURL)
	if page = body(res); res.StatusCode != http.StatusOK || !strings.Contains(page, mine[0].PassID) || !strings.Contains(page, "T-shirt size") {
		t.Fatalf("pass page: HTTP %d", res.StatusCode)
	}
	for _, kind := range []string{"png", "pdf"} {
		res := gita.get(passURL + "/pass." + kind + "?download=1")
		if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Disposition"), "attachment") {
			t.Errorf("download %s: HTTP %d", kind, res.StatusCode)
		}
		res.Body.Close()
	}

	// Nobody else can open it, not even the event's organizer, through the
	// portal.
	for _, other := range []*user{ann, bob} {
		for _, p := range []string{passURL, passURL + "/pass.png"} {
			res := other.get(p)
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("%s opening %s: HTTP %d, want 404", other.email, p, res.StatusCode)
			}
			res.Body.Close()
		}
	}
	if status, b := ann.api("GET", "/api/v1/me/passes", nil); status != http.StatusOK || string(b) != "[]\n" {
		t.Errorf("organizer's own passes: HTTP %d %s, want none", status, b)
	}

	// A later registration with her email waits as a claim; "Not me" hides
	// it without linking.
	pageC := publish(ann, "portc", "Galle Star Party")
	res = anon.form("POST", pageC+"/register", url.Values{"full_name": {"Gita Perera"}, "email": {gita.email}, "q.t_shirt_size": {"S"}})
	res.Body.Close()
	_, b = gita.api("GET", "/api/v1/me/claims", nil)
	json.Unmarshal(b, &claims)
	if len(claims) != 1 {
		t.Fatalf("after a third registration: %d claims, want 1", len(claims))
	}
	if status, _ := gita.api("POST", "/api/v1/me/claims/"+claims[0].ParticipantID+"/decline", nil); status != http.StatusNoContent {
		t.Fatalf("decline: HTTP %d", status)
	}
	_, b = gita.api("GET", "/api/v1/me/claims", nil)
	_, pb := gita.api("GET", "/api/v1/me/passes", nil)
	json.Unmarshal(pb, &mine)
	if string(b) != "[]\n" || len(mine) != 2 {
		t.Errorf("after declining: claims %s, %d passes, want none and 2", b, len(mine))
	}
}
