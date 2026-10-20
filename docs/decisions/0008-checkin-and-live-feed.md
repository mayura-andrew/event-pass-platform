# 8. Check-in: rules in the service, "once" in the database, a polled live feed

Date: 2026-10-21 · Status: accepted

## Check-ins

- **Append-only.** `checkins` rows are never deleted. An undo sets
  `undone_at`; the tenant role may only update `undone_at` and `undone_by`,
  and may only insert rows recorded as itself.
- **Once means once.** A checkpoint that isn't repeatable writes `once = true`
  and a partial unique index on `(checkpoint, participant) WHERE once AND
  undone_at IS NULL` decides between phones that scan the same person at the
  same moment. The loser gets the winner's check-in back as a duplicate, with
  its time and staff name (amber).
- **Retries are safe.** Every scan carries a client-generated idempotency
  key, unique per event; a retry returns the first result. The scanner keeps
  records that failed for lack of a connection in a local queue and resends
  them with the same key.
- **Rules** (prerequisites, repeatable, time window, roles) are checked in
  `CheckinService.decide`, in this order: window, workshop sign-up,
  prerequisites, then record (where the index has the last word).
- **Undo**: owners and editors any check-in; check-in staff their own within
  30 seconds (`service.UndoWindow`).
- **Defaults**: every event gets Confirm participation, Check in, and Kit
  (needs Check in), the current build's flow. Each workshop gets its own
  attendance checkpoint, renamed and removed with it.

## Workshops

Sign-up modes: everyone, opt-in (participants in the portal), invite
(organizers add people). The workshop row is locked while signing up, so the
last place can't go twice; when it is full new sign-ups join the waitlist,
and a cancellation promotes the first in line in the same transaction (the
spec's "waitlist promotion job" needs no worker). Participants can't sign up
for two workshops that overlap; staff adding someone at the door skip that
check but still need a free place.

## Live dashboard

The spec proposes Postgres LISTEN/NOTIFY. The current build chose polling
because Supabase's transaction pooler drops LISTEN registrations between
statements; the same applies here. `EventFeed` polls every 2 seconds for
events with new check-ins, undos, registrations or sign-ups (one query for
all dashboards, only while someone is watching) and signals their
subscribers; changes made on this server signal at once. Clients receive a
Server-Sent Events "change" and fetch fresh numbers. With several API
instances each polls on its own, so no fan-out service is needed.

## Load test

`scripts/loadtest` replaces k6: it seeds 50 orgs straight into the database
(with valid passes and scanner sessions), runs 30 scanners at 20 scans a
minute and 1,000 registrations per 10 minutes, and fails on any error or a
scan p95 over 300 ms. `.github/workflows/loadtest.yml` runs it nightly for 10
minutes. Locally (one API process, Docker Postgres): scan p95 165 ms,
registration p95 108 ms, no errors.

## Deferred

Verifying passes offline (the current build's manifest model) and photos on
the result card; drag-and-drop ordering; workshop extra questions; uPlot
charts (the dashboard uses CSS bars).
