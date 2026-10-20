# 5. Google sign-in with two HTTP calls, no OAuth library

Date: 2026-10-20 · Status: accepted

## Decision

Authorization code flow with PKCE (S256), state and nonce. The state,
nonce, PKCE verifier and destination ride in a 10-minute HMAC-signed,
HttpOnly cookie scoped to `/auth/google`, cleared on the callback. The ID
token's signature is not checked: it comes straight from Google's token
endpoint over TLS in exchange for the client secret, which OpenID Connect
Core 3.1.3.7 allows. Issuer, audience, expiry, nonce and `email_verified`
are checked.

A first Google sign-in links to an existing account with the same (verified)
email, so email links and Google reach one account.

## Consequences

No JWKS fetching or key caching. If ID tokens are ever accepted from a
browser (for example Google One Tap), signature checking becomes required.
