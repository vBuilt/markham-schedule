# Markham Drop-In Schedule API — Design

Date: 2026-10-29

## Intent

The City of Markham publishes its drop-in recreation programs (aquafit, swimming, group fitness, skating, sports, etc.) only through a PerfectMind booking widget meant for people using a browser. The owner wants that schedule as data, so a calendar app and their own apps can use it. Concretely, they want to subscribe to a calendar feed filtered to the categories and community centres they care about, and to have a JSON API for their own apps.

The service runs on the owner's Unraid NAS ("vNas") as a single Docker container on the home network.

## Goals

- Scrape all drop-in sessions from the PerfectMind widget on a timer (default every 3 hours) and keep a local snapshot.
- Serve a subscribable iCalendar (`.ics`) feed, filterable by category and community centre.
- Serve a JSON API with the same filters, plus date range and keyword search, and lists of valid filter values.
- Keep serving the last good data when a scrape fails, and report how old the data is.
- Be polite to Markham's servers.

## Non-goals

- Booking, registering, waitlists, or logging in to PerfectMind.
- Real-time "spots left". Spot counts are only as fresh as the last scrape (a few hours is acceptable to the owner).
- The Google Sheet / Awesome Table group-fitness schedule on markham.ca. PerfectMind is the source of truth and holds dated sessions.
- Registered (multi-week, paid) programs. Only the drop-in widget below is in scope.
- Public internet exposure, user accounts, a web UI.

## Anti-goals (would be a failure even if it "works")

- Hammering PerfectMind: no parallel request storms, and no scraping on each API request.
- Calendar events that duplicate, flicker, or change identity between refreshes.
- A failed or partial scrape wiping out good data and leaving the feed empty.
- Times that are wrong by an hour because of timezone or DST handling.

## Constraints and decisions

### Decided by the owner
- Language: **Go**.
- Host: Unraid NAS, so a Docker container.
- Freshness: refresh every few hours is fine.
- Calendar filters: by **category** and **community centre**.
- Source: the PerfectMind drop-in widget
  `https://cityofmarkham.perfectmind.com/Clients/BookMe4?widgetId=6825ea71-e5b7-4c2a-948f-9195507ad90a`

### Decided by Claude (the owner can override)
- Storage: SQLite via `modernc.org/sqlite` (pure Go, no cgo) in a mounted data directory. Otherwise, use the standard library where practical.
- Refresh interval default 3h, configurable.
- Requests run one at a time with a short delay between them (default ~1s, configurable), a real browser User-Agent, and per-request timeouts.
- No auth on the home network. Optionally, an `API_TOKEN` env var that, when set, is required as `?token=` or `Authorization: Bearer` (query form needed because calendar apps can't send headers).
- Filter matching is case-insensitive substring matching. Repeated parameters are ORed within a filter and ANDed across filters.
- Timezone is fixed to `America/Toronto`.

## Source: how PerfectMind works (observed 2026-10-29)

This is an undocumented internal API, so it may change. Everything below was observed in a browser and verified with plain `curl`. No browser automation is needed.

Constants: base `https://cityofmarkham.perfectmind.com`, widgetId `6825ea71-e5b7-4c2a-948f-9195507ad90a`.

**Session/anti-forgery:** A GET of an HTML page sets cookies and embeds `<input name="__RequestVerificationToken" value="...">`. Later POSTs must send the same cookie jar and that token as a form field. Send `X-Requested-With: XMLHttpRequest` and a `Referer` matching the page. Fetch a fresh page+token per category; tokens may be tied to the page.

**1. Category discovery**
- GET `/Clients/BookMe4?widgetId={W}` → token.
- POST `/Clients/BookMe4V2/GetCategoriesDataV2?embed=False` (form-urlencoded) with `widgetId`, the filter `values[i][Name|Value|Value2|ValueKind]` (Date Range kind 6, Time Range kind 7, Keyword kind 9, Age kind 0 with Value 0 / Value2 1188, all others empty), and `__RequestVerificationToken`.
- The response is a JSON array of groups (currently one, "Drop-In Programs"), each with `Calendars[]`: `{Id, Name, BookingLink}`. There are currently 14: Activities for Age 55+, Adapted, Aquafit, Art, Group Fitness: Cardio / Mind & Body / Strength Training / Total Body Workout, Quick Fitness, Sensory Room / Indoor Playground, Skating, Sports & Activities, Swimming, Tennis Round Robins.
- Some calendars have `BookingLink: null` (e.g. "Adapted"). Skip these, but log them.
- **Category = calendar `Name`.**

**2. Sessions per category**
- GET `/Clients/BookMe4BookingPages/Classes?calendarId={C}&widgetId={W}&embed=False` → token + cookies.
- POST `/Clients/BookMe4BookingPagesV2/ClassesV2` with `calendarId`, `widgetId`, `page=0`, the filter values (Date Range `Value` = today 00:00Z, `Value2` = a far date such as today+1y, ISO format `2026-10-29T00:00:00.000Z`), and the token.
- The response is `{classes: [...], nextKey: "2026-11-03", classesMaxEndDateString: "03/11/2026 08:50 PM"}`. One response holds about a week of sessions (62 Aquafit sessions in the first one).
- **Pagination (confirmed 2026-10-29 with curl):** always send `page=0`. To get the next page, set the Date Range `Value` to `nextKey` + 1 day (00:00Z) and keep `Value2` unchanged. The site's own JS sends `page=N` and `dateString`, but the server ignores both. Each page holds about 55–62 sessions, which is 3–6 whole days. The end is an empty `classes` array with `nextKey` `"0001-01-01"`. Also stop when the next start date passes the horizon, when `nextKey` doesn't advance, or at a hard cap of 40 pages per category. Aquafit currently runs through 2026-12-31: 14 requests. Occasionally the server returns an HTML "Oops... there is an error" page instead of JSON. Treat non-JSON as a retryable error.
- *Claude's call on load:* the horizon defaults to 90 days, which is more than the 60-day feed needs. That's about 13 pages × 14 categories ≈ 200 requests per 3-hour scrape, at 1s spacing. The builder should log how far ahead PerfectMind actually publishes. If it's only a few weeks, the request count drops on its own.

**Useful fields per class:** `EventId` (GUID; appears to identify the course, not the occurrence, so verify), `CourseId`, `EventName`, `Details`, `OccurrenceDate` (`YYYYMMDD`), `FormattedStartTime` / `FormattedEndTime` (`08:05 PM`), `DurationInMinutes`, `AllDayEvent`, `Facility`, `Address.{AddressTag, Street, City, PostalCode}`, `Location`, `AgeRestrictions`, `MinAge`, `MaxAge`, `PriceRange`, `Spots` (e.g. `"4 spots left"`, may be empty), `BookButtonText`.

**Community centre = `Address.AddressTag`** (e.g. "Cornell Community Centre").

Queue-it (a waiting-room system) is wired into the CDN. If a response is not JSON or redirects to a queue page, treat it as a failure for that category and keep the previous data.

## Data model

Session (stored and returned):
- `id`: stable, `sha1(EventId | OccurrenceDate | start time | facility)` hex-truncated. It is also the iCal `UID` (`{id}@markham-schedule`). Category is deliberately not part of the ID (see below).
- `categories`: a list. *Claude's call:* the same session may be listed under more than one PerfectMind calendar (e.g. a Group Fitness category and Activities for Age 55+). Store sessions and the session↔category link separately (many-to-many), de-duplicate by `id`, and return `categories` as an array. A session matches a category filter if any of its categories match. `/api/categories` counts the sessions linked to each category. The ICS feed never emits the same UID twice.
- `name`, `description`
- `start`, `end`: RFC3339 with the Toronto offset. End = start + duration, or the parsed end time.
- `all_day`
- `centre`, `address` (street, city, postal), `facility`
- `age` (display string), `min_age`, `max_age`
- `price`, `spots` (raw string), `spots_left` (int or null, parsed when the string is numeric), `course_id`
- `booking_url`: the category's booking page (absolute URL)
- `scraped_at`

## Refresh behaviour

- *Claude's call:* a time change on PerfectMind produces a new `id`. Calendars will see a delete plus an add, which is acceptable. Sessions that disappear from PerfectMind are dropped from the feed on the next successful scrape of their categories. No `STATUS:CANCELLED` events. If the builder finds PerfectMind marking cancelled sessions explicitly (e.g. via `BookButtonText`), exclude them and note this in the README.
- Run once at startup if the snapshot is missing or older than the interval, then on each interval tick. Only one scrape runs at a time.
- Each category is scraped into memory. If a category succeeds, its session↔category links are replaced in one transaction, and sessions are upserted by `id`. A session row is deleted only when no category links to it any more. If a category fails, its previous links and sessions stay. Category rows are removed only when discovery succeeds and that category is no longer listed.
- *Claude's call:* keep 14 days of past sessions (`RETENTION`, default `336h`), then prune. The ICS feed includes the last 14 days as well as the next `days`, so recently attended classes don't vanish from the owner's calendar. The JSON API still defaults `from` to today.
- Status records the last attempt and last success time, per-category success/error/count, and the overall session count.

## HTTP interface

All endpoints are GET. Default listen address is `:8080`.

- `GET /calendar.ics`: filters `category` (repeatable), `centre` (repeatable), `q`. Optional `days` (default 60) limits how far ahead the feed goes, to keep it small. VEVENT with UID, DTSTAMP, DTSTART/DTEND (TZID=America/Toronto, VTIMEZONE included) or VALUE=DATE for all-day, SUMMARY = name, LOCATION = "centre – facility, street", DESCRIPTION = category, age, price, spots, course ID, details, booking URL, and URL. Set `X-WR-CALNAME` (e.g. "Markham Drop-In") and a refresh hint (`REFRESH-INTERVAL;VALUE=DURATION:PT3H`, `X-PUBLISHED-TTL:PT3H`). Content-Type `text/calendar; charset=utf-8`. Apply RFC 5545 line folding and escaping.
- `GET /api/sessions`: the same filters, plus `from` and `to` (`YYYY-MM-DD`, Toronto dates; default from today, to unbounded) and `limit` (default 500, max 5000). Returns `{ "updated_at": ..., "count": N, "sessions": [...] }` sorted by start.
- `GET /api/categories`: `[{name, count}]`.
- `GET /api/centres`: `[{name, address, count}]`.
- `GET /api/status`: last success/attempt, per-category results, totals, version.
- `GET /healthz`: 200 if the process is up (for Docker healthcheck).

JSON errors look like `{ "error": "..." }`.

## Configuration (env vars)

`LISTEN_ADDR` (`:8080`), `DATA_DIR` (`/data`), `REFRESH_INTERVAL` (`3h`), `REQUEST_DELAY` (`1s`), `HORIZON` (`2160h` = 90 days, how far ahead to ask PerfectMind for), `RETENTION` (`336h`), `API_TOKEN` (empty = off), `TZ` (`America/Toronto`), `LOG_LEVEL`.

## Packaging and deployment

- Multi-stage Dockerfile that builds a static Go binary (`CGO_ENABLED=0`). The final image is small (distroless static or alpine) and includes CA certificates and tzdata (or embed `time/tzdata`). It runs as non-root, has `VOLUME /data`, `EXPOSE 8080`, and a HEALTHCHECK.
- A `docker-compose.yml` example, and README notes for adding the container in Unraid's Docker UI. Map appdata `/mnt/user/appdata/markham-schedule` to `/data`, and set the port mapping and env vars.
- *Owner's decision:* the image is built by GitHub and published to a registry, and Unraid pulls it like any other app.
  - A GitHub Actions workflow builds and pushes to GitHub Container Registry (`ghcr.io/<owner>/markham-schedule`).
  - It runs tests first, then builds `linux/amd64` and `linux/arm64`.
  - Tags: a push to `main` publishes `latest` and `sha-<short>`. A `vX.Y.Z` git tag also publishes `X.Y.Z` and `X.Y`.
  - The build injects the version (tag or short SHA) into the binary via `-ldflags`, and `/api/status` reports it.
- *Claude's call (confirm before creating):* the GitHub repo is `markham-schedule` under the owner's account and is **public**, so the GHCR package is public and Unraid can pull without registry credentials. If the owner wants it private, the README must cover adding a GHCR token in Unraid.

## Testing expectations

- Unit tests for the parsing and normalisation of saved sample PerfectMind responses (fixtures in `testdata/`). Cover time and DST conversion, all-day events, ID stability, and spots parsing.
- Tests for filter matching, ICS output (folding, escaping, and that the UID is stable across refreshes), and handlers via `httptest`.
- Scraper tests against an `httptest` fake PerfectMind. Cover the token/cookie flow, pagination termination, and partial failures preserving old data.
- A manual `-once` / CLI mode that scrapes once and prints a summary, for live verification.

## Left to the builder

Package layout, logging library (stdlib `log/slog` preferred), the HTTP router (stdlib `net/http` patterns are fine), SQLite schema details, retry/backoff specifics, and the exact Docker base image.
