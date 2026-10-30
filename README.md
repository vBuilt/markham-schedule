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

**Every link needs the access token.** Add `?token=…` to the address, or `&token=…` if it already
has a `?`. Without it you get `401 Unauthorized`. The token isn't in this repo. Find it on vNas
with `docker inspect markham-schedule | grep API_TOKEN`.

| What | Where (add the token) |
|---|---|
| Calendar web page | `https://markham.vincentlfilms.com/?token=…` |
| Calendar feed for your calendar app | `https://markham.vincentlfilms.com/calendar.ics?category=…&centre=…&token=…` |
| JSON API for apps | `https://markham.vincentlfilms.com/api/sessions?category=…&centre=…&token=…` |
| Scrape status page: is it working, how each refresh went | `https://markham.vincentlfilms.com/status?token=…` |
| Status as JSON | `https://markham.vincentlfilms.com/api/status?token=…` |
| Health check (no token needed) | `https://markham.vincentlfilms.com/healthz` |
| Source code | <https://github.com/vBuilt/markham-schedule> |
| Docker image | `ghcr.io/vbuilt/markham-schedule:latest` |

Open the web page once with the token and it carries the token along: the **Subscribe** links and
the **Scrape status** link include it. Anyone who has a link with the token can use it, so share
links only with people you'd give access to.

## The calendar web page

Open `https://markham.vincentlfilms.com/?token=…` in any browser, on a laptop or a phone.

- **Views:** Day, Week and List (the next two weeks). Use **Today** and **‹ ›** to move between dates.
  The keys `←` `→` `t` `d` `w` `l` do the same. Phones open in Day view.
- **Filters:** tick programs and locations in the left panel (on a phone, tap **Filters**).
  Click **▶** next to a program to pick individual activities in it, e.g. only Lane Swim under
  Swimming, or only Pickleball under Sports & Activities. Use the
  search box for anything else, such as "pickleball" or "lane swim". Nothing ticked means everything.
  Your choices are remembered in that browser. A browser's first visit starts with only
  **Badminton: Adult and Child** selected.
- **Session details:** click a session for its time, facility, address (opens Google Maps), ages,
  price, spots left, **registration dates**, and a button to its booking page. Spots left are as of
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
https://markham.vincentlfilms.com/calendar.ics?category=aquafit&centre=cornell&token=…
https://markham.vincentlfilms.com/calendar.ics?category=aquafit&category=swimming&centre=angus%20glen&days=30&token=…
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
| `GET /status` | The scrape status page |
| `GET /calendar.ics` | The calendar feed described above |
| `GET /healthz` | `ok` if the process is up |

`/api/sessions` takes the same `category`, `activity`, `centre` and `q` filters, plus:
- `from` and `to` as `YYYY-MM-DD` in Toronto time. `from` defaults to today and `to` is inclusive.
- `limit`, which defaults to 500 and can go up to 5000.

```
T=…   # the access token
curl "https://markham.vincentlfilms.com/api/sessions?category=aquafit&centre=cornell&limit=3&token=$T"
curl "https://markham.vincentlfilms.com/api/sessions?category=skating&from=2026-11-07&to=2026-11-08&token=$T"
curl -H "Authorization: Bearer $T" https://markham.vincentlfilms.com/api/categories
```

The token works either as `?token=…` or as an `Authorization: Bearer …` header.

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
  "booking_url": "https://cityofmarkham.perfectmind.com/Clients/BookMe4BookingPages/Classes?…",
  "registration": {
    "residents": "2026-10-29T12:10:00-04:00",
    "members": "2026-10-30T06:10:00-04:00",
    "public": "2026-10-30T06:10:00-04:00"
  }
}
```

`spots_left` is `0` for full sessions. `registration` is `null` until that course's registration
page has been looked up. All three values `null` means in person only.

Session `id`s stay the same across refreshes, so apps and calendars don't get duplicates. Errors
come back as `{"error": "…"}`.

## Checking that it's healthy

Open **`https://markham.vincentlfilms.com/status?token=…`** (also linked at the bottom of the calendar's filter panel). It shows:
- an overall verdict: Healthy, Partly working, Behind, Failing or Starting up
- when the last good refresh was and when the next is due
- how many sessions have registration dates so far
- the latest refresh broken down by program: OK or failed, session count, change since the
  previous refresh, fetch time and any error
- a chart and table of the last 100 refreshes: time taken, programs OK, sessions, registration
  lookups and errors

The same information as JSON is at `/api/status` and `/api/runs`. In `/api/status`:
- `last_success` is the last good refresh. It should be under 3 hours old.
- `categories` lists each program, with `ok` and how many sessions it has.
- `error` is only present when the last attempt had a problem.

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
  -e API_TOKEN=<token> \
  -v /mnt/user/appdata/markham-schedule:/data \
  ghcr.io/vbuilt/markham-schedule:latest
```

- Host port **8095**, because Krusader already uses 8080 and vnas-dropbox uses 8090.
- `--user 99:100` (Unraid's nobody:users) lets it write to the appdata folder.
- The data is in `/mnt/user/appdata/markham-schedule/schedule.db`. You can delete it to start
  fresh; the next start re-scrapes, which takes about 4 minutes.
- **Always keep `-e API_TOKEN=…` when recreating it.** Without it, the public address is open to
  anyone. Read the current value first with `docker inspect markham-schedule | grep API_TOKEN`.
- To change the token, recreate the container with a new value. Every saved link and calendar
  subscription then needs the new token.

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
docker inspect markham-schedule | grep API_TOKEN   # note the token
docker rm -f markham-schedule
docker run -d --name markham-schedule ...   # same command as above, same API_TOKEN
```

The saved schedule is kept. `GET /api/status` shows the running `version`.

**Logs:** `docker logs -f markham-schedule`, or the container's log icon in the Unraid Docker tab.

### Settings (environment variables)

Add them to the `docker run` command with `-e NAME=value`.

| Variable | Default | Meaning |
|---|---|---|
| `REFRESH_INTERVAL` | `3h` | Time between scrapes |
| `REQUEST_DELAY` | `1s` | Pause between requests to Markham's site |
| `HORIZON` | `2160h` | How far ahead to scrape (90 days) |
| `RETENTION` | `336h` | How long past sessions are kept (14 days) |
| `API_TOKEN` | empty (set on vNas) | If set, every request except `/healthz` needs `?token=…` or `Authorization: Bearer …` |
| `LISTEN_ADDR` | `:8080` | Listen address inside the container |
| `DATA_DIR` | `/data` | Where `schedule.db` lives |
| `REG_LOOKUPS` | `600` | Maximum registration pages fetched per refresh, at `REQUEST_DELAY` spacing. `0` turns registration lookups off |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

Durations use Go syntax (`30m`, `3h`). The timezone is always America/Toronto.

## Development

```
go test ./...
go vet ./...
DATA_DIR=$(mktemp -d) go run ./cmd/markham-schedule -once   # one live scrape, ~4 minutes, prints a summary
DATA_DIR=<same dir> go run ./cmd/markham-schedule           # serve on :8080
```

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
