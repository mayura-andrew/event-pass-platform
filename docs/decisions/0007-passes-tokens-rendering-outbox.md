# 7. Passes: Ed25519 tokens, pure-Go rendering, an outbox in the API

Date: 2026-10-20 · Status: accepted

## Tokens

`EP1.<payload>.<signature>`: version, key ID, org, event, participant (UUIDs
as bytes) and pass ID, signed with Ed25519 over a text form of the same
fields (`internal/passes`). No personal data, never a login. Keys are
numbered in `PASS_SIGNING_KEYS`; the highest signs, the rest verify, so keys
rotate without reissuing passes. Without the setting a key is derived from
`JWT_SECRET` for local development and the API logs a warning.

Passes from the current build (`{"id", "sig"}` with HMAC) are recognised by
`ParseLegacy`. Imported delegates keep their UUID and stored signature, so a
check compares the scanned signature with the stored one and needs no HMAC
key. A pass for another event says so ("Pass is for another event").

## Rendering

The spec proposes SVG rendered by resvg and svg2pdf binaries. For M3 the
Classic band design is drawn in Go: `go-text/typesetting` shapes text
(needed for Sinhala and Tamil names) with Atkinson Hyperlegible, then Noto
Sans Sinhala and Tamil as fallbacks (all OFL, embedded); `go-qrcode` draws
the code with its 4-module quiet zone; PNG is 1080 x 1920 and the PDF is A6
with the image embedded as JPEG. No external tools, files or storage:
passes render on demand in tens of milliseconds. The template designer (M5)
decides whether to move to SVG; the same `Card` data feeds either.

## Email

Participant emails are rows in `outbox`, written in the same transaction as
the registration or status change. A worker goroutine in the API (only with
SMTP configured) claims due rows with `FOR UPDATE SKIP LOCKED` and a lease,
sends through the existing `Mailer`, and retries after 30 s, 2 min, 10 min,
1 h and 6 h. An email whose participant has since changed status is
skipped. The spec's separate worker process and river queue can replace the
goroutine without changing the table. Registering again re-sends the pass,
at most every 5 minutes.

## Deferred

Photo and file questions (need storage), drag-and-drop form builder
(SortableJS), invite-only registration by CSV, workshops (M4), the scanner
using the pass check (M4), Apple and Google Wallet.
