package httphandler

// Integration tests for participant profiles and the participant map (beta).
// Above all: nobody is seen unless they chose to be, and only by registered
// participants of the same event while the organizer has the map on.

import (
	"context"
	"encoding/json"
	"image/color"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type mapResponse struct {
	Me     domain.MapPerson   `json:"me"`
	Public bool               `json:"public"`
	People []domain.MapPerson `json:"people"`
}

func getMap(t *testing.T, u *user, pid string) (int, mapResponse) {
	t.Helper()
	status, b := u.api("GET", "/api/v1/me/passes/"+pid+"/map", nil)
	var m mapResponse
	json.Unmarshal(b, &m)
	return status, m
}

func findPerson(m mapResponse, id string) *domain.MapPerson {
	for i := range m.People {
		if m.People[i].ParticipantID == id {
			return &m.People[i]
		}
	}
	return nil
}

func TestParticipantMap(t *testing.T) {
	f := newCheckinFixture(t, 3)
	env, ctx := f.env, context.Background()
	people := map[string]*user{}
	for i, name := range []string{"p00", "p01", "p02"} {
		u := env.newBrowser(t, name)
		u.signIn("")
		u.get("/me").Body.Close() // links the registration to the account
		people[f.people[i].ID] = u
	}
	annID, bobID, caraID := f.people[0].ID, f.people[1].ID, f.people[2].ID
	ann, bob, cara := people[annID], people[bobID], people[caraID]
	evPage := "/o/" + f.org.Slug + "/events/" + f.ev.ID

	// Off by default: the profile card is there, the map isn't.
	html := body(ann.get("/me/passes/" + annID))
	if !strings.Contains(html, "Your profile") || strings.Contains(html, "Find participants on the map") {
		t.Fatal("pass page: profile card missing or map link shown while the map is off")
	}
	if status, _ := getMap(t, ann, annID); status != http.StatusForbidden {
		t.Errorf("map while off: HTTP %d, want 403", status)
	}

	// The organizer turns it on.
	res := f.owner.form("POST", evPage+"/map", url.Values{"enabled": {"on"}})
	if res.Request.URL.Query().Get("notice") != "on" {
		t.Fatalf("turn map on: HTTP %d at %s", res.StatusCode, res.Request.URL)
	}

	// A bad link is shown next to its field; nothing is saved.
	res = uploadAs(t, ann, "/me/passes/"+annID+"/profile", "photo", map[string]string{"headline": "Data scientist",
		"linkedin": "linkedin.com/in/ann", "github": "evil.example/ann", "visibility": "public"}, artworkPNG(600, 400, color.NRGBA{0, 160, 80, 255}))
	html = body(res)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(html, "Enter your GitHub link") || !strings.Contains(html, `value="linkedin.com/in/ann"`) {
		t.Fatalf("invalid profile: HTTP %d", res.StatusCode)
	}
	// Location access is allowed on the map page only.
	if pp := ann.get("/me/passes/" + annID).Header.Get("Permissions-Policy"); !strings.Contains(pp, "geolocation=()") {
		t.Errorf("pass page Permissions-Policy %q: location should stay blocked", pp)
	}

	// A good one, with a photo, made public.
	res = uploadAs(t, ann, "/me/passes/"+annID+"/profile", "photo", map[string]string{"headline": "Data scientist",
		"linkedin": "linkedin.com/in/ann", "website": "ann.example.org", "visibility": "public"}, artworkPNG(600, 400, color.NRGBA{0, 160, 80, 255}))
	html = body(res)
	if !strings.Contains(html, "Profile saved") || !strings.Contains(html, "Find participants on the map") {
		t.Fatalf("save profile: HTTP %d at %s", res.StatusCode, res.Request.URL)
	}
	res = ann.get("/me/passes/" + annID + "/photo/" + annID)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/jpeg" {
		t.Errorf("own photo: HTTP %d", res.StatusCode)
	}
	res.Body.Close()

	if pp := bob.get("/me/passes/" + bobID + "/map").Header.Get("Permissions-Policy"); !strings.Contains(pp, "geolocation=(self)") {
		t.Errorf("map page Permissions-Policy %q: the page can't ask for the location", pp)
	}

	// Bob (private) sees Ann in the list, with her links, but not on the map yet.
	status, m := getMap(t, bob, bobID)
	a := findPerson(m, annID)
	if status != http.StatusOK || a == nil || a.Lat != nil || a.Links.LinkedIn != "https://linkedin.com/in/ann" || a.PhotoVersion == 0 || m.Public {
		t.Fatalf("bob's map: HTTP %d %+v", status, m)
	}
	if res := bob.get("/me/passes/" + bobID + "/photo/" + annID); res.StatusCode != http.StatusOK {
		t.Errorf("bob viewing ann's public photo: HTTP %d", res.StatusCode)
	}
	// Bob can't share while private.
	if status, b := bob.api("PUT", "/api/v1/me/passes/"+bobID+"/location", map[string]float64{"lat": 6.9, "lng": 79.86}); status != http.StatusConflict || !strings.Contains(string(b), "public") {
		t.Errorf("private share: HTTP %d %s", status, b)
	}

	// Ann shares: Bob sees her on the map.
	if status, b := ann.api("PUT", "/api/v1/me/passes/"+annID+"/location", map[string]float64{"lat": 6.9010, "lng": 79.8722, "accuracy": 12}); status != http.StatusNoContent {
		t.Fatalf("share: HTTP %d %s", status, b)
	}
	_, m = getMap(t, bob, bobID)
	if a := findPerson(m, annID); a == nil || a.Lat == nil || *a.Lat != 6.9010 {
		t.Fatalf("ann not on bob's map: %+v", a)
	}
	// Ann never sees Bob: his profile is private.
	_, m = getMap(t, ann, annID)
	if findPerson(m, bobID) != nil {
		t.Error("a private profile is visible")
	}
	if res := ann.get("/me/passes/" + annID + "/photo/" + bobID); res.StatusCode != http.StatusNotFound {
		t.Errorf("private photo: HTTP %d", res.StatusCode)
	}

	// Cancelled participants and outsiders get nothing.
	f.owner.form("POST", evPage+"/participants/"+caraID+"/status", url.Values{"status": {"cancelled"}})
	if status, _ := getMap(t, cara, caraID); status != http.StatusForbidden {
		t.Errorf("cancelled participant's map: HTTP %d, want 403", status)
	}
	outsider := env.newBrowser(t, "outsider")
	outsider.signIn("")
	if status, _ := getMap(t, outsider, annID); status != http.StatusNotFound {
		t.Errorf("outsider using ann's pass ID: HTTP %d, want 404", status)
	}
	if res := outsider.get("/me/passes/" + annID + "/photo/" + annID); res.StatusCode != http.StatusNotFound {
		t.Errorf("outsider photo: HTTP %d", res.StatusCode)
	}

	// A location stops showing once stale...
	env.db.ExecContext(ctx, `UPDATE participant_locations SET updated_at = NOW() - interval '4 minutes' WHERE participant_id = $1`, annID)
	if _, m = getMap(t, bob, bobID); findPerson(m, annID).Lat != nil {
		t.Error("a stale location is still shown")
	}
	// ...and Stop deletes it.
	env.db.ExecContext(ctx, `UPDATE participant_locations SET updated_at = NOW() WHERE participant_id = $1`, annID)
	ann.api("DELETE", "/api/v1/me/passes/"+annID+"/location", nil)
	locations := func() (n int) {
		env.db.QueryRowContext(ctx, `SELECT count(*) FROM participant_locations WHERE event_id = $1`, f.ev.ID).Scan(&n)
		return
	}
	if locations() != 0 {
		t.Error("stop sharing left the location")
	}
	// Going private deletes it too.
	ann.api("PUT", "/api/v1/me/passes/"+annID+"/location", map[string]float64{"lat": 6.9, "lng": 79.87})
	uploadAs(t, ann, "/me/passes/"+annID+"/profile", "photo", map[string]string{"headline": "Data scientist", "visibility": "private"}, nil).Body.Close()
	if locations() != 0 {
		t.Error("going private left the location")
	}

	// Feedback reaches the organizer.
	if status, _ := bob.api("POST", "/api/v1/me/passes/"+bobID+"/feedback", map[string]any{"rating": 1, "comment": "Found my teammate in 2 minutes"}); status != http.StatusCreated {
		t.Errorf("feedback: HTTP %d", status)
	}
	html = body(f.owner.get(evPage + "/map"))
	if !strings.Contains(html, "Found my teammate in 2 minutes") || !strings.Contains(html, "Person 01") {
		t.Error("organizer doesn't see the feedback")
	}

	// Turning the map off deletes every location and closes the map.
	uploadAs(t, ann, "/me/passes/"+annID+"/profile", "photo", map[string]string{"visibility": "public"}, nil).Body.Close()
	ann.api("PUT", "/api/v1/me/passes/"+annID+"/location", map[string]float64{"lat": 6.9, "lng": 79.87})
	f.owner.form("POST", evPage+"/map", url.Values{})
	if locations() != 0 {
		t.Error("turning the map off left locations")
	}
	if status, _ := getMap(t, ann, annID); status != http.StatusForbidden {
		t.Errorf("map after turning off: HTTP %d", status)
	}
}
