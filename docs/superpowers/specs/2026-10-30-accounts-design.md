# Accounts: sign up, sign in, password reset

## Why

markham-schedule is public at `https://markham.vincentlfilms.com` (Cloudflare Tunnel to vNas) and
also at `http://vnas:8095` on the home network. Everything it does today stays open to everyone.
The owner wants accounts so that **new features can be offered only to signed-in people**. The
first two planned are spot watching (email me when a full session opens a spot) and, later,
auto-registration. Those are separate projects; this one only builds accounts and the gate.

Anyone from the general public may sign up.

## Goals

- Anyone can create an account with email and password, confirm their email, sign in, sign out,
  and reset a forgotten password by email.
- A reusable gate in the Go service that marks an endpoint "requires a signed-in, confirmed
  user", so future features plug in with one line.
- The **Refresh now** button on the status page (`POST /api/refresh`) moves behind that gate.
- Signed-in state is visible on the calendar page and the status page.

## Non-goals

- Spot watching, alerts, auto-registration (later projects).
- Sign in with Google or any third party, two-step verification, passkeys.
- Admin roles or an admin UI. (The owner manages users through the database if ever needed.)
- Putting any existing page, feed or API behind login.

## Anti-goals (would make this a failure even if it works)

- Any existing link, calendar subscription or API client stops working or starts asking for a login.
- Passwords stored or logged in readable form; reset/confirm tokens stored in readable form.
- The site reveals whether a given email has an account (through sign-up, sign-in or reset responses).
- Bots can mass-create accounts or use the reset form to spam someone's inbox.

## Decided by the owner

- General public can sign up.
- Email and password; standard "forgot password" by emailed link.
- Email is sent with **Resend**, from `no-reply@vincentlfilms.com` (display name "Markham Drop-In").
- Refresh now requires sign-in. The rest of the status page stays public.
- Every future new function goes behind login.
- The mockups shown (top-bar Sign in button / account menu, a separate sign-in page with
  "Sign in" and "Create account" tabs, the two emails, the status page's "Sign in to refresh now")
  were approved as the direction.

## Decided by me (accepted with the playback; change only with reason)

- **Email confirmation required** before gated features work. An unconfirmed user can sign in
  but gated endpoints answer `403` with a message saying to confirm the email; the account page
  offers "Resend confirmation email". Confirm links last 24 hours.
- **Bot check:** Cloudflare Turnstile on the create-account and forgot-password forms. If the
  Turnstile keys are not configured, the check is skipped (so local development works).
- **Passwords:** at least 10 characters, at most 200; no other composition rules. Hashed with
  argon2id (`golang.org/x/crypto/argon2`) or bcrypt; never logged.
- **Sessions:** random 32-byte token in an HttpOnly, SameSite=Lax cookie; only its SHA-256 hash is
  stored. Lasts 30 days, sliding (renewed on use at most once a day). Sign out deletes it.
- **Reset:** link lasts 1 hour and works once. Setting the new password signs the user out
  everywhere (all their sessions deleted) and signs them in on the current device.
- **Rate limits:** sign-in failures slow down per email and per client IP (e.g. 5 failures →
  locked 15 minutes for that pair), and each IP gets at most 30 sign-in attempts per 15 minutes
  whatever the email; sign-up, reset and resend-confirmation are limited per IP and
  per email (e.g. at most 3 emails per address per hour). In-memory counters are fine.
- **No account enumeration:** forgot-password always says "If that email has an account, we've
  sent a link". Sign-up with an existing email says the same thing as a new sign-up ("check your
  email") and emails the owner of that address a note that someone tried to sign up, with a
  reset link. Sign-in failure is always "Email or password is incorrect".
- **Emails** are normalised (trimmed, lower-cased) and must look like an address; uniqueness is
  on the normalised form.
- **Account settings page:** shows the email; change password (needs current password); delete
  account (needs current password; deletes the user and all their sessions and tokens).
- **CSRF:** SameSite=Lax cookies plus a check that state-changing requests' `Origin` (or
  `Referer`) matches the request host; requests failing it get `403`.
- **Client IP:** `CF-Connecting-IP` when present (requests through the tunnel), otherwise the
  connection's remote address.
- **Cookie `Secure` flag:** set when the request arrived over HTTPS (`X-Forwarded-Proto: https`
  from cloudflared, or TLS). Signing in over `http://vnas:8095` on the home network still works.

### Settled after the builder check (my calls)

- **`API_TOKEN` and accounts.** `API_TOKEN` is unset in production. If it is set, it keeps doing
  exactly what it does today: every route except `/healthz` (the new auth routes and pages
  included) needs the token, and emailed links carry `?token=` so they keep working. The token
  is never a substitute for a session: `POST /api/refresh` needs a confirmed signed-in user
  either way. No existing client breaks, since nothing uses the token today.
- **Sign-up never signs you in.** You confirm by email, then sign in. The sign-up response is the
  same for new and existing addresses, and email is sent in the background (not awaited by the
  request), so response time doesn't reveal which case happened.
- **Repeat sign-up for an address that exists but is unconfirmed** replaces the password with the
  new one, signs out that account's sessions, cancels its outstanding confirmation links, and
  sends a fresh confirmation email. (Otherwise someone could pre-register a victim's address with
  a password they know and inherit the account when the victim confirms.) **Confirming** also
  signs out every session that existed before. For a confirmed address, repeat sign-up sends the
  "someone tried to sign up; here's a reset link" note and changes nothing.
- **Resetting the password also confirms the account**, since opening the emailed link proves
  ownership of the address just as a confirmation link does.
- **Repeat sign-up for an unconfirmed address, and resending the confirmation email,** restart
  that account's 7-day purge clock.
- **Known, accepted:** sign-up followed by sign-in can reveal whether an address already has a
  confirmed account (sign-in succeeds only if the sign-up took effect). Each probe needs a bot
  check, is limited to 3 per address and 10 per IP per hour, and emails the address's owner, so
  this is accepted rather than giving up sign-in before confirmation.
- **Changing or resetting the password** cancels any other outstanding reset links. The current
  password checks in change-password and delete-account are rate limited like sign-in.
- **Cleanup:** a background job deletes unconfirmed accounts older than 7 days and expired
  sessions and tokens, once an hour or so.
- **Resend failures** are logged; the user sees the usual neutral message (no retry queue). The
  account page's "Resend confirmation email" is the recovery path.
- **Turnstile:** the keys are a pair: it is on only when both are set, and setting just one is a
  startup error. If both are empty, the service logs a warning at startup that bot protection
  is off. If Cloudflare's siteverify can't be reached, the form fails closed with "Couldn't check
  you're not a robot. Try again."
- **Trusted proxy headers.** `CF-Connecting-IP` and `X-Forwarded-Proto` are trusted only when the
  connection comes from a private-range address used by Docker (default `172.16.0.0/12`,
  overridable with `TRUSTED_PROXIES`, a comma-separated CIDR list); the cloudflared container
  reaches the service from there. Otherwise the remote address is the client IP and the cookie's
  `Secure` flag follows actual TLS.
- **CSRF check:** applies to cookie-authenticated state-changing requests (all `POST`s under
  `/api/auth/` and gated endpoints). A request needs `Origin` (or, failing that, `Referer`) whose
  host equals the request's `Host` header or `BASE_URL`'s host; missing both → `403`. This works
  for both `markham.vincentlfilms.com` and `vnas:8095`.
- **Change password** keeps the current session and signs out all others.
- **Email address can't be changed** (delete and re-create the account instead). Nothing to build.
- **Schema:** follow the store's existing `CREATE TABLE IF NOT EXISTS` style; no migration system.

## How it fits the existing service

Go 1.25, stdlib `net/http` mux, pure-Go SQLite (`modernc.org/sqlite`), pages are static HTML
embedded with `//go:embed` that call JSON endpoints with `fetch`. Accounts follow the same
pattern: JSON endpoints under `/api/auth/…` and embedded pages.

- New tables in the existing `schedule.db` for users, sessions and one-time tokens (confirm,
  reset). Existing tables untouched.
- `API_TOKEN` (existing, currently unset) keeps its meaning: if set, it still gates every
  request. Accounts are independent of it.
- The status JSON (`/api/status`) keeps `refresh_now` meaning "the endpoint exists"; the page
  decides what to show from whether the viewer is signed in.

### Pages

- `/login`: tabs **Sign in** and **Create account**; "Forgot your password?" link. After sign-in,
  return to the page the user came from (`?next=`, same-site paths only).
- `/forgot`: email field (+ Turnstile).
- `/reset?t=…`: new password twice.
- `/verify?t=…`: confirms and says so, with a link back to the calendar.
- `/account`: settings described above; sign out.
- Calendar page top bar: **Sign in** button when signed out; when signed in, a small menu with the
  email, "Account settings", "Sign out". Unconfirmed users see a short "Confirm your email" notice
  in that menu.
- Status page: Refresh now shows as disabled with "Sign in to refresh now" when signed out, and
  "Confirm your email to refresh now" when unconfirmed.

All pages match the existing look (Atkinson Hyperlegible, the tile/ink palette, light and dark),
work at phone width, and use plain, specific wording.

### Endpoints (shape left to the builder, behaviour fixed above)

Sign up, confirm, resend confirmation, sign in, sign out, current user ("me": email, confirmed),
forgot password, reset password, change password, delete account.

### Configuration (new environment variables)

| Variable | Meaning |
|---|---|
| `BASE_URL` | Public address used in email links, e.g. `https://markham.vincentlfilms.com` |
| `RESEND_API_KEY` | Resend API key. If empty, emails are logged instead of sent (development) |
| `MAIL_FROM` | Default `Markham Drop-In <no-reply@vincentlfilms.com>` |
| `TURNSTILE_SITE_KEY`, `TURNSTILE_SECRET` | If empty, the bot check is skipped (with a startup warning) |
| `TRUSTED_PROXIES` | CIDRs whose `CF-Connecting-IP` / `X-Forwarded-Proto` headers are trusted; default `172.16.0.0/12` |

Resend is called over its HTTPS API (no SDK needed). Turnstile tokens are verified server-side
with Cloudflare's `siteverify` endpoint.

## Owner setup (outside the code; the builder writes the steps, the owner does them)

1. Resend: create an account, add domain `vincentlfilms.com`, add the DNS records it shows in
   Cloudflare, create an API key.
2. Cloudflare Turnstile: add a widget for `markham.vincentlfilms.com` (and `vnas` if wanted),
   copy the site key and secret.
3. Recreate the vNas container with the new `-e` variables. The README's run command and
   memory notes must be updated.

## Left to the builder

- Package layout (e.g. a new `internal/auth`), endpoint paths and JSON shapes, table schemas.
- argon2id vs bcrypt and parameters; exact rate-limit numbers within the spirit above.
- Email wording beyond the mockups (plain text plus simple HTML).
- How the gate is expressed in code, as long as a future handler can require "confirmed user"
  in one line and read the current user from the request context.
- Tests: the service is built test-first; auth logic (tokens, expiry, rate limits, enumeration
  rules, CSRF, gate) gets unit tests; email and Turnstile behind interfaces with fakes.
