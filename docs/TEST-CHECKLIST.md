# Manual test checklist

Everything on the `saas/m1-foundation` branch, run locally. Tick items as you
go (`[ ]` → `[x]`). Anything that fails or reads badly: note the page, who you
were signed in as, and what you expected.

## Before you start

- [ ] Containers up: `docker compose -f nsac-qr.compose.yaml up -d` (all three running)
- [ ] App: http://localhost:8080 · Mailpit (all emails): http://localhost:8025
- [ ] On localhost:8080, DevTools (F12) → Application → Storage → **Clear site data** (removes the old service worker)
- [ ] In Mailpit, **Delete all** so only new emails show
- [ ] One browser profile or private window per person you play

Staff accounts already in the local database: `superadmin@test.local`,
`lead@test.local` (admin), `admin@spaceapps.org` (admin), `gate1@test.local`
and `gate2@test.local` (scanners). Use their passwords, or set a new one
(it asks for the password twice):

```sh
DATABASE_URL='postgres://nsac:nsac-local-password@127.0.0.1:5433/nsac_qr?sslmode=disable' \
  go run ./scripts/create-admin -email lead@test.local -name "Event lead" -role admin
```

---

## A. Current build (live event app)

### A1. Delegate pass: http://localhost:8080/pass.html

- [ ] Look up `NSAC-1092` + `participant@spaceapps.org`: the pass shows name, team and QR
- [ ] Wrong email for that ID: the same "no confirmed pass" message as an unknown ID
- [ ] **Food preference**: pick one → "Saved."; reload → still selected
- [ ] Offline: DevTools → Network → Offline, change it → "Saved on this device…"; go back online → "Saved."
- [ ] **Event schedule (demo)**: two days of sessions; tapping one moves the map (needs `GOOGLE_MAPS_API_KEY`) and shows "Directions to this session"
- [ ] Download: **Save image** and **Print-ready PDF** both produce the card
- [ ] Offline again and reload: the saved pass still opens and its QR still shows
- [ ] **Use a different pass** clears it and shows the lookup form
- [ ] Phone width (DevTools device toolbar, Ctrl+Shift+M): no sideways scrolling

### A2. Scanner: http://localhost:8080/scanner.html

- [ ] Sign in as `gate1@test.local`; only that account's checkpoints are offered
- [ ] Scan the pass QR from A1 (a phone or a second screen): the result card shows the name, ID and food preference
- [ ] Scan the same pass again at a once-only checkpoint: amber duplicate warning with time and staff name
- [ ] Offline, scan, go back online: the scan uploads (it shows on the dashboard)

### A3. Organizer dashboard: http://localhost:8080/admin/

- [ ] Sign in as `lead@test.local`: stats, recent scans and the live feed update after a scan
- [ ] Filter delegates by food preference: the value from A1 is there
- [ ] Add a delegate (single, and by CSV with `scripts/sample/delegates_examples.csv`)
- [ ] Email a pass: it arrives in Mailpit and its link opens the pass
- [ ] Staff page (superadmin): create a scanner; the credentials email arrives in Mailpit
- [ ] Fields page: the food preference field exists and shows on the scanner

---

## B. SaaS M1: sign-in and organizations

### B1. Sign in

- [ ] http://localhost:8080/login → email `owner@test.local` → "Check your email"
- [ ] Mailpit: open the **newest** email → link → "Finish signing in" → **Sign in** → "Your organizations"
- [ ] Use the same link again: "expired or was already used" (expected)
- [ ] Ask for 6 links in a row for one address: the 6th is refused ("several sign-in links…")
- [ ] **Sign out** → you're back at sign-in; http://localhost:8080/app sends you to sign in
- [ ] Google button shows only when `GOOGLE_OAUTH_CLIENT_ID` and `GOOGLE_OAUTH_CLIENT_SECRET` are set

### B2. Organizations and members

- [ ] Create an org: the address fills in from the name; you land on an empty events home
- [ ] Create another org with the same address: "is taken"
- [ ] **Members** → invite `editor@test.local` (Member) and `admin2@test.local` (Admin)
- [ ] In another window: sign in as `editor@test.local`, open the invite email → **Accept and join**
- [ ] The invite opened while signed in as someone else: "sign in as … to accept"
- [ ] As owner: change a role, withdraw a pending invite, remove a member
- [ ] As owner: try to make yourself a member while you're the only owner → refused ("needs at least one owner")
- [ ] As a member: no invite form, roles shown read-only; **Leave** works
- [ ] Open another org's URL (`/o/some-other-slug`): "Not found"

---

## C. SaaS M2: events and sharing

Use the owner from B, plus people in separate windows:
`editor@test.local` (member), `scanner@test.local` (member),
`guest@test.local` (not a member).

### C1. Create and edit

- [ ] **New event**: name, dates, time zone, venue, capacity, tags → it opens with "Event created"
- [ ] The time zone defaults to your computer's zone
- [ ] Edit with the end before the start: the error keeps what you typed
- [ ] **New event → Start from: Copy of …**: the copy keeps the details and the people it was shared with
- [ ] **Settings → Duplicate event**: a "Copy of …" draft opens

### C2. Events home (the "Docs list")

- [ ] Views: My events, All events (owner/admin only), Shared with me, Recent, Starred, Archived, Trash
- [ ] Star an event (☆ on the card) → it's in Starred; another person's Starred doesn't show it
- [ ] Open an event → it's first in Recent
- [ ] Press `/` → the cursor jumps to search; search by name, venue and tag
- [ ] Sort options and the Grid / List switch work
- [ ] Card menu (⋯): Duplicate, Archive, Move to Trash
- [ ] Archived: gone from My events, listed under Archived, and still found by search
- [ ] Trash: listed under Trash with **Restore**; restoring brings it back
- [ ] Phone width: the views scroll sideways in their own row, the page doesn't

### C3. Sharing and roles

As owner, on the event's **Share** tab:

- [ ] Share with `editor@test.local` as Editor and `scanner@test.local` as Check-in staff: they appear at once, and each gets an email
- [ ] Share with `guest@test.local` as Viewer: listed under "Waiting to accept"; the invite email arrives
- [ ] As guest: sign in, then accept from that email → the event opens; the org is listed as "Guest"

Check what each person sees:

| Person | Expected |
| --- | --- |
| Editor | Can edit; sees Share, Activity, Settings; **can't** add people until the owner ticks "Editors can share" (Settings) |
| Check-in staff | Overview only, read-only; no Share, Settings or Activity |
| Viewer / guest | Overview read-only; guest sees only Shared with me and no Members tab |
| Member not shared | The event isn't listed; its URL says "Not found" |
| Org admin | Sees every event (All events), can edit and share, can't transfer or delete |

- [ ] Owner ticks **Editors can share** → the editor can now add people
- [ ] Change someone's role; remove someone; withdraw a pending invite
- [ ] **Transfer ownership** to a member: you become an editor; they can delete it
- [ ] Move to Trash: only the owner and org owners see the event in Trash and can restore it

### C4. Link sharing

- [ ] **Turn on link** (30 days): the link shows once; copy it
- [ ] Open it in a private window (signed out): a view-only page with the event details
- [ ] **Reset link**: the old link stops working, the new one works
- [ ] **Turn off link**: the link says "not available"
- [ ] Members → untick outside sharing → sharing with a new outside address and turning on a link are both refused

### C5. Activity log

- [ ] The org **Activity** tab (owners and admins) lists everything above in plain sentences, newest first
- [ ] The event **Activity** tab lists only that event
- [ ] Members can't see the org Activity tab

---

## D. Automated tests

```sh
docker compose -f nsac-qr.compose.yaml exec -T db psql -U nsac -d postgres -c "create database itest"
DATABASE_URL='postgres://nsac:nsac-local-password@127.0.0.1:5433/itest?sslmode=disable' go run ./cmd/migrate up
NSAC_TEST_DATABASE_URL='postgres://nsac:nsac-local-password@127.0.0.1:5433/itest?sslmode=disable' go test ./...
```

- [ ] All packages `ok` (about 45 seconds)
