# markham-schedule

A small Go service that scrapes the City of Markham's PerfectMind drop-in schedule
(aquafit, swimming, skating, group fitness, and more) into SQLite and serves it as an
`.ics` calendar feed plus a JSON API. It runs as a single Docker container.

The scraper is the only thing that talks to PerfectMind: it runs on startup and then every
`REFRESH_INTERVAL`, sequentially with a delay between requests. API requests never trigger a scrape.

## Endpoints

| Endpoint | Description |
|---|---|
| `GET /calendar.ics` | iCalendar feed. Filters: `category`, `centre` (repeatable), `q`; `days` (default 60). |
| `GET /api/sessions` | JSON sessions. Same filters plus `from`, `to` (`YYYY-MM-DD`, Toronto) and `limit` (default 500, max 5000). |
| `GET /api/categories` | `[{name, count}]` |
| `GET /api/centres` | `[{name, address, count}]` |
| `GET /api/status` | Last attempt/success, per-category results, version |
| `GET /healthz` | Liveness (never needs the token) |

Filters are case-insensitive substring matches. Repeated params are ORed within a filter and ANDed across filters.

Examples:

```
http://vnas:8080/calendar.ics?category=aquafit&centre=cornell
http://vnas:8080/calendar.ics?category=aquafit&category=swimming&centre=angus%20glen&days=30
http://vnas:8080/api/sessions?category=aquafit&centre=cornell&limit=3
http://vnas:8080/api/status
```

If `API_TOKEN` is set, send it as `Authorization: Bearer <token>` or `?token=<token>` (calendar apps need the query form).

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `DATA_DIR` | `/data` | Directory for `schedule.db` (created if missing) |
| `REFRESH_INTERVAL` | `3h` | Time between scrapes (must be > 0) |
| `REQUEST_DELAY` | `1s` | Pause between PerfectMind requests |
| `HORIZON` | `2160h` | How far ahead to scrape (90 days; must be > 0) |
| `RETENTION` | `336h` | How long past sessions are kept |
| `API_TOKEN` | empty | Optional shared token; empty disables auth |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

Durations use Go syntax (`30m`, `3h`). The timezone is always `America/Toronto`.

CLI flags: `-once` (one refresh, print a per-category table, exit non-zero if every category failed),
`-version`, `-healthcheck` (used by the Docker `HEALTHCHECK`).

## Unraid setup

1. Create the appdata folder and give the container's user access. The image is distroless and runs as UID 65532:
   ```
   mkdir -p /mnt/user/appdata/markham-schedule
   chown 65532:65532 /mnt/user/appdata/markham-schedule
   ```
   Alternatively, add `--user 99:100` (Unraid's nobody:users) to **Extra Parameters** and skip the chown.
   (This applies to bind mounts only; named Docker volumes work out of the box because the image ships `/data` owned by 65532.)
2. Docker tab, **Add Container**:
   - Repository: `ghcr.io/vbuilt/markham-schedule:latest`
   - Port: container `8080`, host `8080` (TCP)
   - Path: container `/data`, host `/mnt/user/appdata/markham-schedule`
   - Variables: any from the table above (defaults are fine; consider `API_TOKEN`)
3. Apply. The first scrape takes a few minutes (about 200 requests at 1s spacing); check
   `http://vnas:8080/api/status`. Until then the feed is a valid empty calendar.

A `docker-compose.yml` with the same settings is included. The GHCR package must be set to Public
(package settings) for Unraid to pull it without credentials.

## Subscribing

- **Apple Calendar / iOS:** File, New Calendar Subscription (iOS: Settings, Calendar, Accounts, Add Subscribed Calendar), paste
  `http://vnas:8080/calendar.ics?category=aquafit&centre=cornell`. Works on the LAN.
- **Google Calendar:** Other calendars, From URL. Google fetches from its own servers, so the URL must be reachable from the
  internet (not a LAN address like `vnas`). Google also refreshes subscriptions only every several hours to a day.

## Development

```
go test ./...
go vet ./...
CGO_ENABLED=0 go build ./...
DATA_DIR=$(mktemp -d) go run ./cmd/markham-schedule -once   # live scrape, ~4 minutes
```

Test fixtures live in `internal/perfectmind/testdata`; re-capture them with `scripts/capture-fixtures.sh`
if PerfectMind's response format changes.

Images are built and pushed to `ghcr.io/vbuilt/markham-schedule` by GitHub Actions on pushes to `main` and `v*` tags
(linux/amd64 and linux/arm64).

## Caveats

The data comes from PerfectMind's undocumented, unofficial widget endpoints, observed rather than specified.
They can change or break without notice, and the service is deliberately gentle (sequential requests, a delay,
a few retries). If a category fails to refresh, the previously stored data is kept. The default User-Agent is
`Mozilla/5.0 (compatible; markham-schedule/<version>; +https://github.com/vBuilt/markham-schedule)`; it was accepted
during live testing, so no browser-like fallback was needed.
