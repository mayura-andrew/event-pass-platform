# Platform progress

The event platform implementation is complete through the core registration,
pass, check-in, workshop, designer, and integration milestones.

## Completed

- Multi-tenant accounts, organizations, invitations, roles, and RLS
- Event creation, sharing, archiving, search, and audit history
- Public event registration with configurable forms, dates, capacity, and waitlists
- Signed event pass tokens with key rotation and PNG/PDF rendering
- Participant portal at `/me` and pass email outbox
- Checkpoints, workshops, attendance rules, duplicate detection, undo, and live feed
- Pass templates, uploaded artwork, previews, and design validation
- Google Forms import and Google Sheets synchronization
- Participant profiles, event maps, and birth-date/age rules

## Verification

Run the unit and integration test suite with:

```sh
go test ./...
```

Integration tests are enabled with `EVENT_PASS_TEST_DATABASE_URL` pointing to a
database that has all migrations applied.

## Next

Billing and launch-readiness work remains tracked in the product backlog.
