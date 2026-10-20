# 9. Pass designer and Google integrations

Date: 2026-10-21 · Status: accepted

## Designs are JSON drawn by one Go renderer

- A design (`passes.Design`) is a fixed-size canvas, phone (1080 x 1920) or
  badge (1200 x 900), with absolutely placed elements: status band, text,
  answers, QR code, box, line. Text takes `{{variables}}`.
- The designer's preview, the PNG download, the emailed pass and the PDF
  all call `passes.RenderDesign`. The browser shows the server's PNG
  rather than drawing its own version (SVG or canvas), so what the
  designer shows is what participants get. The cost is a round trip per
  edit, debounced to 250 ms. The overlay the organizer drags is only a set
  of handles.
- **Scan checks** (`passes.Check`) run on every save and live while editing:
  - exactly one QR code, dark modules on light, with at least 7:1 contrast
    and a quiet zone of at least 4 modules
  - at least 30 mm printed and 40% of a phone screen
  - nothing drawn over the code
  - only known variables
  - every sample name (short, long, non-Latin, cancelled) fits

  A design that fails is refused (422 with the list).
- Snapshot tests keep golden PNGs of the gallery for each sample
  (`go test ./internal/passes -update` to refresh after an intended change).

## Versions, pinned

Saving makes a new row in `pass_template_versions`; old versions are never
changed. An event pins a template and version, so editing a template never
changes passes for events already using it until someone picks the new
version. Events without a template use Classic, the current pass.

## Integrations

- **Scopes.** Sheets uses `drive.file`: the platform creates the spreadsheet
  and can reach only that file, not the organizer's Drive. Forms uses the
  two read-only scopes. Consent is a separate step from sign-in, per event.
  It uses PKCE, offline access and a signed state cookie, with no Google
  library, as in [0005](0005-google-sign-in-without-a-library.md).
- **Tokens at rest.** The refresh token is sealed with AES-256-GCM
  (`internal/secrets`). The associated data is the integration's ID, so a
  sealed token can't be moved to another row. The key is
  `INTEGRATIONS_KEY`; with a cloud KMS this becomes an envelope key, and
  the version byte allows that change. The tenant role has no SELECT on the
  column; only the worker reads it.
- **Sheets is one-way.** The worker rewrites the three tabs (clear, then
  write, values RAW so participant text never becomes a formula). It runs
  after any change to participants, check-ins or workshops, at most every
  30 s, checked every 10 s: a change shows within about 40 s.
- **Forms is polled** every minute. Each response is registered with
  `external_ref` = its response ID, and a partial unique index makes a
  second import impossible, even from two workers. Someone already
  registered with the same email is skipped. Answers that don't fit the
  event's question (an option that isn't offered) are dropped rather than
  losing the person. Imported people get their pass email like anyone
  else.
- **Failures.** If Google returns `invalid_grant`, the integration is marked
  "Needs reconnecting". Other failures are shown on the tab and retried
  every minute.
- Push notifications (Drive watch, Forms watches) would cut the delay but
  need a public HTTPS endpoint and renewal. Polling meets the one-minute
  gate without them.
