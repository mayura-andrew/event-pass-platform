# 6. Event access: one resolver, one route table, audit in the same transaction

Date: 2026-10-20 · Status: accepted

## Decision

- **Who can open an event** is decided in one place, `authz.ForEvent`: the
  strongest of the org role's implicit access (org owner = event owner, org
  admin = editor), event ownership (`events.owner_id`) and the caller's
  share. No access is 404, access without the action is 403.
- **Guests** are people outside the org with a share. They are not stored as
  members: the org resolves for them with the role `guest`, which only
  allows seeing the org's name and the events shared with them.
- **Every event endpoint** is a row in `eventRoutes` naming the action it
  needs. The wrapper enforces that action before the handler runs, and the
  authorization matrix test is generated from the same table (every route ×
  ten kinds of caller), so a new endpoint can't skip the check or the test.
- **Audit entries** are written by the repository inside the transaction of
  the change, so a change and its record commit together. The tenant role may
  only INSERT and SELECT on `audit_log`, and only as itself.
- **Link sharing** stores only the token's hash, so the URL is shown once (on
  creation or reset). Links are view-only by constraint, and switching off
  "share outside the organization" blocks new links and new guests.
- **The Share dialog** is a page tab with plain forms for now; the spec's
  Web Component people picker can be layered on it later.

## Deferred

- "Move to another org" (owners only): needs a cross-tenant transaction that
  moves every child row; left until events have children (M3).
- Purging events 30 days after deletion: they stop being restorable and
  listable then; the nightly purge job comes with the worker (M3).
- Event templates as starting points: the type presets exist, the content
  (form, workshops) arrives with M3.
