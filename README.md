# markham-schedule

The City of Markham's drop-in recreation schedule (swimming, aquafit, skating, group fitness,
sports and more) as a calendar web page, a subscribable calendar feed, and a JSON API.

A small Go service scrapes Markham's PerfectMind booking site every 3 hours, keeps the results
in SQLite, and serves them from a single Docker container. It runs on the Unraid NAS `vnas` and is
published to the internet through a Cloudflare Tunnel.

## Quick access

Everything lives at two addresses, which serve exactly the same thing:

- **`https://markham.vincentlfilms.com`**: public, from anywhere, through a Cloudflare Tunnel.
- **`http://vnas:8095`**: from the home network, or from anywhere on Tailscale.

Browsing is open to anyone: no password or token needed. Using new features needs a free account
(see [Accounts](#accounts)).

| What | Where |
|---|---|
| Calendar web page | `https://markham.vincentlfilms.com/` |
| Calendar feed for your calendar app | `https://markham.vincentlfilms.com/calendar.ics?category=…&centre=…` |
| JSON API for apps | `https://markham.vincentlfilms.com/api/sessions?category=…&centre=…` |
| Scrape status page: is it working, how each refresh went | `https://markham.vincentlfilms.com/status` |
| Status as JSON | `https://markham.vincentlfilms.com/api/status` |
| Sign in or create an account | `https://markham.vincentlfilms.com/login` |
| Health check | `https://markham.vincentlfilms.com/healthz` |
| Source code | <https://github.com/vBuilt/markham-schedule> |
| Docker image | `ghcr.io/vbuilt/markham-schedule:latest` |

To require a token instead, see `API_TOKEN` under [Settings](#settings-environment-variables).

## The calendar web page

Open `https://markham.vincentlfilms.com/` in any browser, on a laptop or a phone.

- **Views:** Day, Week and List (the next two weeks). Use **Today** and **‹ ›** to move between dates.
  The keys `←` `→` `t` `d` `w` `l` do the same. Phones open in Day view; swipe left or right on the
  calendar to go to the next or previous day (or week, in Week view).
- **Filters:** tick programs and locations in the left panel (on a phone, tap **Filters**).
  Click **▶** next to a program to pick individual activities in it, e.g. only Lane Swim under
  Swimming, or only Pickleball under Sports & Activities. Use the
  search box for anything else, such as "pickleball" or "lane swim". Nothing ticked means everything.
  Your choices are remembered in that browser. A browser's first visit starts with only
  **Badminton: Adult and Child** selected.
- **Session details:** click a session for its time, facility, address (opens Google Maps), ages,
  price, spots left, **registration dates**, and a button that opens that session's own page on
  Markham's site, where you book it. Spots left are as of
  the last refresh, not live.
- **Full sessions** are hatched, struck through and tagged **Full**. Sessions with 1–3 spots left
  say "Only N left".
- **Registration dates:** when online registration opens for residents, members and the public,
  plus how long until then (or "open now"). The list view also shows when residents can
  register. Drop-ins usually open to residents 21 hours before the start and to members and the
  public 3 hours before. "In person only" means the session can't be booked online, e.g. Aquafit.
- **Busy days:** when too many sessions overlap, the week view folds them into a **+N more** button.
  Filtering to a few programs and locations makes the week easy to read.
- **Subscribe:** gives you a calendar-feed link for exactly what you're filtering, ready to paste
  into a calendar app (see below). The link uses whichever address you opened the page from, so
  open the page from the public address to get links that work everywhere.

## Calendar feed (subscribe in your calendar app)

The easiest way to get a link is the **Subscribe** button on the web page. To build one by hand:

```
https://markham.vincentlfilms.com/calendar.ics?category=aquafit&centre=cornell
https://markham.vincentlfilms.com/calendar.ics?category=aquafit&category=swimming&centre=angus%20glen&days=30
```

- `category` is a program and `centre` is a location. Both can be repeated.
- `activity` is one exact activity name (as listed by `/api/activities`), e.g.
  `activity=Drop-In%20Lane%20Swim`. Programs and activities combine: a session matches if it's in any
  ticked program **or** is any ticked activity.
- Matching ignores case and accepts part of a name, so `cornell` matches "Cornell Community Centre".
- Several values of the same filter mean "any of these". Different filters must all match.
- `q` is a free-text search.
- `days` sets how far ahead the feed goes (default 60). The feed also keeps the last 14 days,
  so classes you've just been to don't disappear.

**Apple Calendar (Mac):** File, then New Calendar Subscription, then paste the link.

**iPhone / iPad:** Settings, then Calendar, then Accounts, then Add Account, then Other, then
Add Subscribed Calendar, then paste the link.

**Google Calendar:** on a computer, next to **Other calendars** click **+**, then
**From URL**, then paste the link. It must be a public `https://markham.vincentlfilms.com/…` link;
Google fetches feeds from its own servers, so it can't reach `vnas`. Google only refreshes
subscriptions every several hours, up to a day.

Use the public link everywhere and the calendar refreshes wherever you are.

## JSON API (for apps)

| Endpoint | Returns |
|---|---|
| `GET /api/sessions` | `{updated_at, count, sessions: [...]}`, sorted by start time |
| `GET /api/categories` | Programs: `[{name, count}]` |
| `GET /api/centres` | Locations: `[{name, address, count}]` |
| `GET /api/activities` | Activities in each program: `[{category, name, count}]` |
| `GET /api/status` | Last attempt and last success, per-program results, total, version, refresh interval, registration-date coverage |
| `GET /api/runs` | History of recent refreshes, newest first (`limit`, default 50, max 300) |
| `POST /api/refresh` | Starts a refresh now. Needs a signed-in, confirmed account (`401` signed out, `403` not confirmed). `202` started, `409` one is already running, `429` too soon after the last one (with `Retry-After` in seconds) |
| `GET /status` | The scrape status page |
| `GET /api/auth/me` | `{user: {email, confirmed} or null, turnstile_site_key}`; `user` is `null` when signed out |
| `POST /api/auth/signup` | `{email, password, turnstile}`: creates an account and emails a confirmation link. `202` |
| `POST /api/auth/confirm` | `{t}`: confirms an email with the token from the link |
| `POST /api/auth/resend` | Sends the confirmation email again. Needs sign-in |
| `POST /api/auth/signin` | `{email, password}`: sets the session cookie |
| `POST /api/auth/signout` | Ends the session. `204` |
| `POST /api/auth/forgot` | `{email, turnstile}`: emails a reset link. `202` |
| `POST /api/auth/reset` | `{t, password}`: sets a new password from the emailed token and signs you in |
| `POST /api/auth/password` | `{current, new}`: changes your password. Needs sign-in. `204` |
| `POST /api/auth/delete` | `{password}`: deletes your account. Needs sign-in. `204` |
| `GET /calendar.ics` | The calendar feed described above |
| `GET /healthz` | `ok` if the process is up |

`/api/sessions` takes the same `category`, `activity`, `centre` and `q` filters, plus:
- `from` and `to` as `YYYY-MM-DD` in Toronto time. `from` defaults to today and `to` is inclusive.
- `limit`, which defaults to 500 and can go up to 5000.

```
curl 'https://markham.vincentlfilms.com/api/sessions?category=aquafit&centre=cornell&limit=3'
curl 'https://markham.vincentlfilms.com/api/sessions?category=skating&from=2026-11-07&to=2026-11-08'
curl 'https://markham.vincentlfilms.com/api/categories'
```

Each session looks like this:

```json
{
  "id": "4f1c…",
  "categories": ["Aquafit"],
  "name": "Drop-In Aquafit: Shallow/Deep",
  "start": "2026-10-30T09:10:00-04:00",
  "end": "2026-10-30T09:55:00-04:00",
  "centre": "Cornell Community Centre",
  "facility": "Pool: Main - Lane 1-8 (Full)",
  "address": { "street": "3201 Bur Oak Drive", "city": "Markham", "postal_code": "L6B 0T2" },
  "age": "16+", "price": "No fee", "spots": "60 spots left", "spots_left": 60,
  "course_id": "359564",
  "booking_url": "https://cityofmarkham.perfectmind.com/Clients/BookMe4LandingPages/Class?classId=…&occurrenceDate=20261030&…",
  "registration": {
    "residents": "2026-10-29T12:10:00-04:00",
    "members": "2026-10-30T06:10:00-04:00",
    "public": "2026-10-30T06:10:00-04:00"
  }
}
```

`spots_left` is `0` for full sessions. `registration` is `null` until that course's registration
page has been looked up. All three values `null` means in person only.

The `/api/auth/*` and `/api/refresh` endpoints are for the web pages and use the session cookie,
not a token. They take JSON, and every `POST` must come from the site itself: a request whose
`Origin` (or `Referer`) host doesn't match the site is refused with `403`. They're not meant for
scripts.

Session `id`s stay the same across refreshes, so apps and calendars don't get duplicates. Errors
come back as `{"error": "…"}`.

## Accounts

Anyone can create an account with an email address and a password. For now an account only unlocks
**Refresh now** on the status page; every future new function will also need one.

**Public, no sign-in needed:** the calendar page, the calendar feed (`/calendar.ics`), the read-only
JSON API (`/api/sessions`, `/api/categories`, `/api/centres`, `/api/activities`, `/api/status`,
`/api/runs`), the status page and `/healthz`. Existing links and calendar subscriptions keep working.

**Needs a confirmed account:** **Refresh now** (`POST /api/refresh`). A signed-out visitor sees the
button disabled with "Sign in to refresh now". Someone who has signed in but not confirmed their
email sees "Confirm your email to refresh now".

| Page | What it does |
|---|---|
| `/login` | Two tabs, **Sign in** and **Create account**, with a "Forgot your password?" link |
| `/verify` | Where the link in the confirmation email lands |
| `/forgot` | Asks for your email and sends a password reset link |
| `/reset` | Where the link in the reset email lands; you choose a new password |
| `/account` | Resend the confirmation email, change your password, sign out, delete your account |

The calendar's top bar shows **Sign in**, or a menu once you're signed in.

How it works:
- **Create account:** enter an email and a password (10 to 200 characters, no other rules). The
  service emails a confirmation link that works for 24 hours. Until you click it you can sign in
  but can't use gated features; **Resend confirmation email** on `/account` sends a new link.
  Accounts that are still unconfirmed after 7 days are deleted.
- **Forgot password:** the reset link works once and lasts 1 hour. Setting a new password signs you
  out everywhere else. The page always says "if that email has an account, we've sent a link", so
  it never reveals which emails are registered.
- **Sessions:** a cookie keeps you signed in for 30 days from your last visit. **Sign out** ends it.
  Only a hash of the cookie and of every emailed link is stored, and passwords are stored hashed
  (argon2id).
- **Limits:** repeated wrong passwords lock that email and address out for a while, and the
  sign-up, reset and resend emails are rate limited.
- **Bot check:** Cloudflare Turnstile guards **Create account** and **Forgot password**.
- Accounts live in the same `schedule.db` as the schedule, so they survive updates.

Emails come from `Markham Drop-In <no-reply@vincentlfilms.com>` through Resend. The owner has to set
that up once; see [Setting up accounts](#setting-up-accounts-owner-one-time-steps).

## Checking that it's healthy

Open **`https://markham.vincentlfilms.com/status`** (also linked at the bottom of the calendar's filter panel). It shows:
- an overall verdict: Healthy, Partly working, Behind, Failing or Starting up
- when the last good refresh was and when the next is due
- how many sessions have registration dates so far
- the latest refresh broken down by program: OK or failed, session count, change since the
  previous refresh, fetch time and any error
- a chart and table of the last 100 refreshes: time taken, programs OK, sessions, registration
  lookups and errors
- a **Refresh now** button that scrapes right away instead of waiting for the next scheduled
  refresh. You have to be signed in with a confirmed account; otherwise it's disabled and says
  "Sign in to refresh now" or "Confirm your email to refresh now". While it runs, the page checks every few seconds and updates when it's done. To go
  easy on Markham's site, only one refresh runs at a time and the button stays off for 15 minutes
  after a refresh finishes; it shows when it'll be available again. A manual refresh also
  restarts the 3-hour schedule.

The same information as JSON is at `/api/status` and `/api/runs`. In `/api/status`:
- `last_success` is the last good refresh. It should be under 3 hours old.
- `categories` lists each program, with `ok` and how many sessions it has.
- `error` is only present when the last attempt had a problem.
- `refreshing` is `true` while a refresh runs. `refresh_now_at` is when **Refresh now** will next
  be accepted, or `null` if it can be used now.

When a refresh fails, the previously saved data stays in place, and the service tries again in
15 minutes. The page footer shows when the schedule was last updated.

## Running it on vNas

Two containers run on vNas. They were started over SSH (`ssh vnas`, which goes through Tailscale
SSH) with `docker run`, not Unraid templates. They show in Unraid's Docker tab, where you can
start, stop and view logs, but their settings can't be edited there.

**`markham-schedule`**, the service:

```
docker run -d --name markham-schedule --restart unless-stopped \
  -p 8095:8080 --user 99:100 \
  -v /mnt/user/appdata/markham-schedule:/data \
  -e BASE_URL=https://markham.vincentlfilms.com \
  -e RESEND_API_KEY=<resend key> \
  -e TURNSTILE_SITE_KEY=<turnstile site key> \
  -e TURNSTILE_SECRET=<turnstile secret key> \
  ghcr.io/vbuilt/markham-schedule:latest
```

Replace the `<…>` placeholders with the real values from the steps below. Without the last three
`-e` lines the service still runs, but emails are only written to the log and the bot check is off.

- Host port **8095**, because Krusader already uses 8080 and vnas-dropbox uses 8090.
- `--user 99:100` (Unraid's nobody:users) lets it write to the appdata folder.
- The data is in `/mnt/user/appdata/markham-schedule/schedule.db`. You can delete it to start
  fresh; the next start re-scrapes, which takes about 4 minutes.
- It runs without `API_TOKEN`, so the public address is open to anyone to browse. Add
  `-e API_TOKEN=<secret>` to require a token; every link then needs `?token=<secret>`. Accounts
  work either way, and a token is never a substitute for signing in to use **Refresh now**.

**`cloudflared`**, the Cloudflare Tunnel that publishes it:

```
docker run -d --name cloudflared --restart unless-stopped \
  cloudflare/cloudflared:latest tunnel --no-autoupdate run --token <tunnel token>
```

- The tunnel is named **vNas** in the Cloudflare dashboard, under Networking, then Tunnels.
  Its route sends `markham.vincentlfilms.com` to `http://192.168.68.132:8095`, which is vNas's
  address on the home network. If vNas's IP changes, update the route there.
- The tunnel token is on the tunnel's Overview page in Cloudflare. **Rotate token** there
  invalidates the old one; then recreate this container with the new one.
- Other vNas apps can be published through the same tunnel: add another route.

**Updating to a new version.** Every push to `main` on GitHub builds a new `latest` image
automatically. To install it:

```
ssh vnas
docker pull ghcr.io/vbuilt/markham-schedule:latest
docker rm -f markham-schedule
docker run -d --name markham-schedule ...   # same command as above, including the -e lines
```

The saved schedule is kept. `GET /api/status` shows the running `version`.

**Logs:** `docker logs -f markham-schedule`, or the container's log icon in the Unraid Docker tab.

### Setting up accounts (owner, one-time steps)

Sign-up emails go out through [Resend](https://resend.com), and the bot check is Cloudflare
Turnstile. Both are free at this scale. Do these once, then add the keys to the `docker run` command.

**1. Resend: send email from `vincentlfilms.com`**

1. Create a Resend account at <https://resend.com> and sign in.
2. Go to **Domains**, then **Add Domain**. Enter `vincentlfilms.com` and pick a region close to
   you (for example `us-east-1`).
3. Resend lists the DNS records to add (a TXT record for SPF, a CNAME or TXT for DKIM, and an MX
   record for bounces). Keep that page open.
4. In the Cloudflare dashboard, open the `vincentlfilms.com` zone, then **DNS**, then **Records**,
   then **Add record**. Copy each record's type, name and value from Resend exactly. Set every one
   to **DNS only** (grey cloud), not proxied.
5. Back in Resend, click **Verify DNS Records**. It can take a few minutes. Wait for the status to
   say **Verified**.
6. Go to **API Keys**, then **Create API Key**. Give it the name `markham-schedule` and the
   permission **Sending access**, limited to `vincentlfilms.com`. Copy the key (it starts with
   `re_`) now: Resend shows it only once. This is `RESEND_API_KEY`.

Mail is sent as `Markham Drop-In <no-reply@vincentlfilms.com>`. To use another sender on the same
verified domain, add `-e MAIL_FROM="Name <address@vincentlfilms.com>"`.

**2. Cloudflare Turnstile: the bot check**

1. In the Cloudflare dashboard, open **Turnstile** (in the left menu), then **Add widget**
   (or **Add site**).
2. Name it `markham-schedule`.
3. Under **Hostname**, add `markham.vincentlfilms.com`, then add `vnas` as well so the sign-up form
   also works at `http://vnas:8095`.
4. Choose **Managed** as the widget mode, then **Create**.
5. Copy the **Site Key** (this is `TURNSTILE_SITE_KEY`) and the **Secret Key** (this is
   `TURNSTILE_SECRET`).

**3. Recreate the container with the keys**

```
ssh vnas
docker pull ghcr.io/vbuilt/markham-schedule:latest
docker rm -f markham-schedule
docker run -d --name markham-schedule --restart unless-stopped \
  -p 8095:8080 --user 99:100 \
  -v /mnt/user/appdata/markham-schedule:/data \
  -e BASE_URL=https://markham.vincentlfilms.com \
  -e RESEND_API_KEY=<resend key> \
  -e TURNSTILE_SITE_KEY=<turnstile site key> \
  -e TURNSTILE_SECRET=<turnstile secret key> \
  ghcr.io/vbuilt/markham-schedule:latest
```

Check `docker logs markham-schedule`. A warning at startup means a key is missing (it says that
bot protection or email is off).

**4. Try it**

Open `https://markham.vincentlfilms.com/login`, create an account with your own email, click the
link in the email, then sign in and press **Refresh now** on `/status`.

`BASE_URL` is the address used for the links in emails, so it must be the public address. A link
in an email opened from the home network still works, because the public address works there too.

### Settings (environment variables)

Add them to the `docker run` command with `-e NAME=value`.

| Variable | Default | Meaning |
|---|---|---|
| `REFRESH_INTERVAL` | `3h` | Time between scrapes |
| `REQUEST_DELAY` | `1s` | Pause between requests to Markham's site |
| `HORIZON` | `2160h` | How far ahead to scrape (90 days) |
| `RETENTION` | `336h` | How long past sessions are kept (14 days) |
| `API_TOKEN` | empty | If set, every request except `/healthz` needs `?token=<secret>` or `Authorization: Bearer <secret>`. The web page passes it along to its own links |
| `LISTEN_ADDR` | `:8080` | Listen address inside the container |
| `DATA_DIR` | `/data` | Where `schedule.db` lives |
| `REG_LOOKUPS` | `600` | Maximum registration pages fetched per refresh, at `REQUEST_DELAY` spacing. `0` turns registration lookups off |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `BASE_URL` | empty | The public address, used for links in emails, e.g. `https://markham.vincentlfilms.com`. Must be a full URL. Also accepted as the site's own address when checking that sign-in requests come from the site. If empty, the service picks a local default |
| `RESEND_API_KEY` | empty | Resend API key for sending email. If empty, emails are written to the log instead of sent |
| `MAIL_FROM` | `Markham Drop-In <no-reply@vincentlfilms.com>` | Sender of account emails. Its domain must be verified in Resend |
| `TURNSTILE_SITE_KEY` | empty | Cloudflare Turnstile site key, shown to the browser. With the secret, turns the bot check on. If either is empty the check is skipped and a warning is logged at startup |
| `TURNSTILE_SECRET` | empty | Cloudflare Turnstile secret key, used by the server to check answers |
| `TRUSTED_PROXIES` | `172.16.0.0/12` | Comma-separated networks whose `CF-Connecting-IP` and `X-Forwarded-Proto` headers are believed (Docker's range, where `cloudflared` connects from). Used for rate limits and the cookie's `Secure` flag |

Durations use Go syntax (`30m`, `3h`). The timezone is always America/Toronto.

## Development

```
go test ./...
go vet ./...
DATA_DIR=$(mktemp -d) go run ./cmd/markham-schedule -once   # one live scrape, ~4 minutes, prints a summary
DATA_DIR=<same dir> go run ./cmd/markham-schedule           # serve on :8080
```

Without `RESEND_API_KEY`, emails (confirmation and reset links) are written to the log instead of
being sent, so run it in a terminal and copy the link from there. Without Turnstile keys the bot
check is skipped. To test the Turnstile widget locally, use Cloudflare's test keys: site key
`1x00000000000000000000AA` with secret `1x0000000000000000000000000000000AA` always passes. Set
`BASE_URL=http://localhost:8080` so links in emails point at your local server.

Other flags: `-version`, and `-healthcheck`, which the Docker health check uses.

Test fixtures are real captured responses in `internal/perfectmind/testdata`. If Markham's site
changes format, re-capture them with `bash scripts/capture-fixtures.sh`.

GitHub Actions runs the tests, then builds and pushes `linux/amd64` and `linux/arm64` images to
GHCR on every push to `main` (tag `latest` plus `sha-…`). A `v1.2.3` git tag also publishes `1.2.3` and `1.2`.

## How registration dates are collected

Registration dates aren't in the schedule listing. They're only on each session's own page, and
there are about 5,000 sessions. So the service fetches **one page per course** (about 630 courses)
and learns its rule: "opens N hours before the start" for each audience, or a fixed season-wide
date. Every session of that course gets its dates calculated from the rule. The hours follow the
local clock across daylight-saving changes, the same way Markham's site does.

- New courses are looked up first, soonest session first, up to `REG_LOOKUPS` per refresh.
  A fresh install needs about two refreshes to cover everything.
- Each rule is re-checked after 14 days.
- After 5 lookup failures in a row it stops for that refresh. Sessions whose course hasn't been
  looked up show "Not looked up yet".

## Caveats

- The data comes from the unofficial, undocumented endpoints behind Markham's PerfectMind booking
  widget. They can change without notice. The service is gentle: one request at a time with a
  pause between them, and a few retries.
- "Spots left" are as of the last refresh. Always check the booking page before you go.
- Booking and registration happen on Markham's site. This service only reads the schedule.
- The User-Agent identifies the service honestly:
  `Mozilla/5.0 (compatible; markham-schedule/<version>; +https://github.com/vBuilt/markham-schedule)`.
