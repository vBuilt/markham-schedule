# markham-schedule

The City of Markham's drop-in recreation schedule (swimming, aquafit, skating, group fitness,
sports and more) as a calendar web page, a subscribable calendar feed, and a JSON API.

A small Go service scrapes Markham's PerfectMind booking site every 3 hours, keeps the results
in SQLite, and serves them from a single Docker container. It runs on the Unraid NAS `vnas`.

## Quick access

Everything lives at **`http://vnas:8095`**. It's reachable from the home network, or from
anywhere on Tailscale.

| What | Where |
|---|---|
| Calendar web page | [`http://vnas:8095/`](http://vnas:8095/) |
| Calendar feed for your calendar app | `http://vnas:8095/calendar.ics?category=…&centre=…` |
| JSON API for apps | `http://vnas:8095/api/sessions?category=…&centre=…` |
| Is it working? When did it last update? | [`http://vnas:8095/api/status`](http://vnas:8095/api/status) |
| Source code | <https://github.com/vBuilt/markham-schedule> |
| Docker image | `ghcr.io/vbuilt/markham-schedule:latest` |

## The calendar web page

Open `http://vnas:8095/` in any browser, on a laptop or a phone.

- **Views:** Day, Week and List (the next two weeks). Use **Today** and **‹ ›** to move between dates.
  The keys `←` `→` `t` `d` `w` `l` do the same. Phones open in Day view.
- **Filters:** tick programs and locations in the left panel (on a phone, tap **Filters**). Use the
  search box for anything else, such as "pickleball" or "lane swim". Nothing ticked means everything.
  Your choices are remembered in that browser.
- **Session details:** click a session for its time, facility, address (opens Google Maps), ages,
  price, spots left, and a button to its booking page. Spots left are as of the last refresh,
  not live.
- **Busy days:** when too many sessions overlap, the week view folds them into a **+N more** button.
  Filtering to a few programs and locations makes the week easy to read.
- **Subscribe:** gives you a calendar-feed link for exactly what you're filtering, ready to paste
  into a calendar app (see below).

## Calendar feed (subscribe in your calendar app)

The easiest way to get a link is the **Subscribe** button on the web page. To build one by hand:

```
http://vnas:8095/calendar.ics?category=aquafit&centre=cornell
http://vnas:8095/calendar.ics?category=aquafit&category=swimming&centre=angus%20glen&days=30
```

- `category` is a program and `centre` is a location. Both can be repeated.
- Matching ignores case and accepts part of a name, so `cornell` matches "Cornell Community Centre".
- Several values of the same filter mean "any of these". Different filters must all match.
- `q` is a free-text search.
- `days` sets how far ahead the feed goes (default 60). The feed also keeps the last 14 days,
  so classes you've just been to don't disappear.

**Apple Calendar (Mac):** File, then New Calendar Subscription, then paste the link.

**iPhone / iPad:** Settings, then Calendar, then Accounts, then Add Account, then Other, then
Add Subscribed Calendar, then paste the link. The phone has to be on the home Wi-Fi or on
Tailscale when it refreshes.

**Google Calendar:** Google fetches feeds from its own servers, so it can't reach `vnas`. It only
works if the service is exposed to the internet, for example with Tailscale Funnel or a
Cloudflare Tunnel. If you do that, set `API_TOKEN` (below) and add `&token=…` to the link.
Google also only refreshes subscriptions every several hours, up to a day.

## JSON API (for apps)

| Endpoint | Returns |
|---|---|
| `GET /api/sessions` | `{updated_at, count, sessions: [...]}`, sorted by start time |
| `GET /api/categories` | Programs: `[{name, count}]` |
| `GET /api/centres` | Locations: `[{name, address, count}]` |
| `GET /api/status` | Last attempt and last success, per-program results, total, version |
| `GET /calendar.ics` | The calendar feed described above |
| `GET /healthz` | `ok` if the process is up |

`/api/sessions` takes the same `category`, `centre` and `q` filters, plus:
- `from` and `to` as `YYYY-MM-DD` in Toronto time. `from` defaults to today and `to` is inclusive.
- `limit`, which defaults to 500 and can go up to 5000.

```
curl 'http://vnas:8095/api/sessions?category=aquafit&centre=cornell&limit=3'
curl 'http://vnas:8095/api/sessions?category=skating&from=2026-11-07&to=2026-11-08'
curl 'http://vnas:8095/api/categories'
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
  "booking_url": "https://cityofmarkham.perfectmind.com/Clients/BookMe4BookingPages/Classes?…"
}
```

Session `id`s stay the same across refreshes, so apps and calendars don't get duplicates. Errors
come back as `{"error": "…"}`.

## Checking that it's healthy

Open `http://vnas:8095/api/status`:
- `last_success` is the last good refresh. It should be under 3 hours old.
- `categories` lists each program, with `ok` and how many sessions it has.
- `error` is only present when the last attempt had a problem.

When a refresh fails, the previously saved data stays in place, and the service tries again in
15 minutes. The page footer shows when the schedule was last updated.

## Running it on vNas

The container was started over SSH (`ssh vnas`, which goes through Tailscale SSH) with
`docker run`, not an Unraid template. It shows in Unraid's Docker tab, where you can start, stop
and view logs, but its settings can't be edited there.

```
docker run -d --name markham-schedule --restart unless-stopped \
  -p 8095:8080 --user 99:100 \
  -v /mnt/user/appdata/markham-schedule:/data \
  ghcr.io/vbuilt/markham-schedule:latest
```

- Host port **8095**, because Krusader already uses 8080 and vnas-dropbox uses 8090.
- `--user 99:100` (Unraid's nobody:users) lets it write to the appdata folder.
- The data is in `/mnt/user/appdata/markham-schedule/schedule.db`. You can delete it to start
  fresh; the next start re-scrapes, which takes about 4 minutes.

**Updating to a new version.** Every push to `main` on GitHub builds a new `latest` image
automatically. To install it:

```
ssh vnas
docker pull ghcr.io/vbuilt/markham-schedule:latest
docker rm -f markham-schedule
docker run -d --name markham-schedule ...   # same command as above
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
| `API_TOKEN` | empty | If set, every request except `/healthz` needs `?token=…` or `Authorization: Bearer …`. Open the web page as `http://vnas:8095/?token=…` |
| `LISTEN_ADDR` | `:8080` | Listen address inside the container |
| `DATA_DIR` | `/data` | Where `schedule.db` lives |
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

## Caveats

- The data comes from the unofficial, undocumented endpoints behind Markham's PerfectMind booking
  widget. They can change without notice. The service is gentle: one request at a time with a
  pause between them, and a few retries.
- "Spots left" are as of the last refresh. Always check the booking page before you go.
- Booking and registration happen on Markham's site. This service only reads the schedule.
- The User-Agent identifies the service honestly:
  `Mozilla/5.0 (compatible; markham-schedule/<version>; +https://github.com/vBuilt/markham-schedule)`.
