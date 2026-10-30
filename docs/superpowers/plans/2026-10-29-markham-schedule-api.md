# Markham Drop-In Schedule API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go service that scrapes Markham's PerfectMind drop-in schedule every few hours into SQLite and serves it as a filterable `.ics` feed and JSON API. It ships as a Docker image built by GitHub Actions to GHCR for the owner's Unraid NAS.

**Architecture:**
- `perfectmind` speaks the undocumented widget API (token/cookie flow, category discovery, date-cursor pagination) and converts raw classes to `schedule.Session`.
- `store` persists sessions in SQLite, with a many-to-many session↔category link.
- `scraper` refreshes per category on a timer, keeping old data for failed categories.
- `ics` and `api` render the stored data.
- `cmd/markham-schedule` wires it together.

**Tech Stack:** Go 1.25, stdlib `net/http` / `log/slog` / `net/http/cookiejar`, `modernc.org/sqlite` (no cgo), Docker multi-stage, GitHub Actions + `docker/build-push-action`, GHCR.

**Spec:** `docs/superpowers/specs/2026-10-29-markham-schedule-api-design.md`. Read it before starting any task. The "Source: how PerfectMind works" section is the protocol reference.

## Global Constraints

- Module path: `github.com/vBuilt/markham-schedule`; image `ghcr.io/vbuilt/markham-schedule` (GHCR requires lowercase).
- Pure Go: `CGO_ENABLED=0` must build. The only third-party runtime dependency is `modernc.org/sqlite`.
- PerfectMind base `https://cityofmarkham.perfectmind.com`, widgetId `6825ea71-e5b7-4c2a-948f-9195507ad90a`.
- All PerfectMind requests are sequential, with `REQUEST_DELAY` (default `1s`) between them. Never scrape on an API request.
- Timezone `America/Toronto` everywhere. Embed `time/tzdata` in the binary.
- Env defaults: `LISTEN_ADDR=:8080`, `DATA_DIR=/data`, `REFRESH_INTERVAL=3h`, `REQUEST_DELAY=1s`, `HORIZON=2160h`, `RETENTION=336h`, `API_TOKEN=` (off), `LOG_LEVEL=info`.
- Session ID: first 16 hex chars of `sha1(EventId + "|" + OccurrenceDate + "|" + FormattedStartTime + "|" + Facility)`. ICS UID is `{id}@markham-schedule`.
- ICS feed window: from now−`RETENTION` to now+`days` (query param, default 60). JSON `/api/sessions` defaults `from` to today (Toronto).
- Filters are case-insensitive substring matches. Repeated params are ORed within a filter and ANDed across filters. `category` matches any of a session's categories.
- Logging: `log/slog`, text handler.

## Review Focus

1. **A session listed under two categories** should appear once in JSON and ICS with both categories. If one category's refresh drops it, the session must survive under the other category. Pinned in Task 3 (`TestSessionInTwoCategories`, `TestReplaceCategoryKeepsSharedSession`) and Task 5 (`TestNoDuplicateUIDs`).
2. **DST end on 2026-11-01:** a 08:05 PM class that day must be `20:05-05:00`, and a class on 2026-10-31 must be `-04:00`. Pinned in Task 1 (`TestToSessionAcrossDST`).
3. **PerfectMind returns an HTML error page or Queue-it redirect instead of JSON.** The client should retry, then fail that category only, and the old data stays. Pinned in Task 2 (`TestClassesRetriesOnHTML`) and Task 4 (`TestRefreshKeepsFailedCategory`).
4. **Calendar app subscribes before the first scrape finishes.** The response should be a valid empty VCALENDAR, and JSON should give `count: 0, updated_at: null` with no 500. Pinned in Task 6 (`TestEmptyStore`).
5. **Filter values with odd case and repeats** (`centre=CORNELL&centre=angus glen&category=aquafit`) should match as specified. An unknown value gives an empty result, not an error. Pinned in Task 3 (`TestFilterMatching`) and Task 6 (`TestSessionsFilters`).

---

## File Structure

```
go.mod / go.sum
cmd/markham-schedule/main.go        config load, wiring, -once flag, HTTP server, graceful shutdown
internal/config/config.go           Config struct + Load() from env
internal/schedule/schedule.go       Session, Filter, Status, CategoryResult types (no logic beyond Matches)
internal/perfectmind/client.go      HTTP client: token/cookie flow, Calendars, Classes (pagination, retry)
internal/perfectmind/types.go       raw JSON structs
internal/perfectmind/convert.go     ToSession, ParseSpots, SessionID
internal/perfectmind/testdata/      real captured fixtures
internal/store/store.go             SQLite store
internal/scraper/scraper.go         Refresh + Run loop
internal/ics/ics.go                 iCalendar writer
internal/api/api.go                 HTTP handlers
scripts/capture-fixtures.sh         re-capture PerfectMind fixtures
Dockerfile, .dockerignore, docker-compose.yml, README.md
.github/workflows/docker.yml
```

---

### Task 1: Module, core types, raw types, and conversion

**Files:**
- Create: `go.mod`, `internal/schedule/schedule.go`, `internal/perfectmind/types.go`, `internal/perfectmind/convert.go`, `scripts/capture-fixtures.sh`, `internal/perfectmind/testdata/{categories.json,classes_page1.json,classes_empty.json}`
- Test: `internal/perfectmind/convert_test.go`

**Interfaces:**
- Produces (package `schedule`):
  ```go
  type Session struct {
      ID          string    `json:"id"`
      Categories  []string  `json:"categories"`
      Name        string    `json:"name"`
      Description string    `json:"description"`
      Start       time.Time `json:"start"`   // in America/Toronto
      End         time.Time `json:"end"`
      AllDay      bool      `json:"all_day"`
      Centre      string    `json:"centre"`
      Address     Address   `json:"address"`
      Facility    string    `json:"facility"`
      Age         string    `json:"age"`
      MinAge      *int      `json:"min_age"`
      MaxAge      *int      `json:"max_age"`
      Price       string    `json:"price"`
      Spots       string    `json:"spots"`
      SpotsLeft   *int      `json:"spots_left"`
      CourseID    string    `json:"course_id"`
      BookingURL  string    `json:"booking_url"`
      ScrapedAt   time.Time `json:"scraped_at"`
  }
  type Address struct{ Street, City, PostalCode string } // json: street, city, postal_code
  type Filter struct {
      Categories, Centres []string
      Query               string    // substring of name or description
      From, To            time.Time // zero = unbounded; session matches if Start >= From and Start < To
      Limit               int       // 0 = no limit
  }
  func (f Filter) Matches(s Session) bool
  type CategoryResult struct {
      Name string `json:"name"`; OK bool `json:"ok"`; Count int `json:"count"`; Error string `json:"error,omitempty"`
  }
  type Status struct {
      LastAttempt *time.Time `json:"last_attempt"`; LastSuccess *time.Time `json:"last_success"`
      Categories []CategoryResult `json:"categories"`; Total int `json:"total"`; Error string `json:"error,omitempty"`
  }
  ```
- Produces (package `perfectmind`):
  ```go
  type Calendar struct { ID, Name, BookingLink string } // JSON: Id, Name, BookingLink
  type CategoryGroup struct { Name string; Calendars []Calendar }
  type RawClass struct { /* fields listed in spec "Useful fields per class", JSON names verbatim; Address nested */ }
  type ClassesResponse struct { Classes []RawClass `json:"classes"`; NextKey string `json:"nextKey"` }
  func SessionID(r RawClass) string
  func ParseSpots(s string) *int
  func ToSession(r RawClass, category, bookingURL string, loc *time.Location, scrapedAt time.Time) (schedule.Session, error)
  ```

- [ ] **Step 1: Init module and capture fixtures**

`go mod init github.com/vBuilt/markham-schedule`. Write `scripts/capture-fixtures.sh` (bash, `set -euo pipefail`). It should:
- GET the widget page to get a token, then POST `GetCategoriesDataV2` and save the result to `testdata/categories.json`.
- For the Aquafit calendar `d4d891dd-9e45-474b-97c4-e43c8f8fe3b8`: GET the booking page for a token, then POST `ClassesV2` with Date Range Value = today and Value2 = today+90d, saving `testdata/classes_page1.json`.
- POST again with Value = today+400d and save `testdata/classes_empty.json`.

Use a cookie jar, `X-Requested-With: XMLHttpRequest`, and a `Referer` set to the page URL. The form fields are exactly as in the spec. Run it with bash, not zsh. Check: `jq '.classes|length' testdata/classes_page1.json` > 0, and `classes_empty.json` has `nextKey == "0001-01-01"`.

- [ ] **Step 2: Write failing tests in `convert_test.go`**

```go
func TestToSessionFromFixture(t *testing.T)  // first class of classes_page1.json: ID len 16, Start.Location()==Toronto,
                                             // End == Start + DurationInMinutes, Centre == Address.AddressTag, Categories == {"Aquafit"}
func TestToSessionAcrossDST(t *testing.T)    // RawClass{OccurrenceDate:"20261101", FormattedStartTime:"08:05 PM", FormattedEndTime:"08:50 PM", DurationInMinutes:45}
                                             // → Start.Format(time.RFC3339)=="2026-11-01T20:05:00-05:00"; same on "20261031" → "-04:00"
func TestToSessionEndFallback(t *testing.T)  // DurationInMinutes 0 → End parsed from FormattedEndTime; end "12:15 AM" after start "11:30 PM" → next day
func TestToSessionAllDay(t *testing.T)       // AllDayEvent true → AllDay true, Start = local midnight, End = next midnight
func TestSessionIDStable(t *testing.T)       // same inputs → same ID; changing Spots/PriceRange doesn't change it; changing start time does
func TestParseSpots(t *testing.T)            // "4 spots left"→4, "1 spot left"→1, ""→nil, "Full"→nil, "Waitlist"→nil
func TestToSessionBadDate(t *testing.T)      // OccurrenceDate "garbage" → error
```

- [ ] **Step 3: Run tests, confirm they fail**

Run: `go test ./internal/perfectmind/...` Expected: FAIL (undefined symbols).

- [ ] **Step 4: Implement `schedule.go`, `types.go`, `convert.go`**

Parse the date with `time.ParseInLocation("20060102 03:04 PM", date+" "+start, loc)`. Store `MinAge`/`MaxAge` from the raw nullable ints. `Description` = `Details`, trimmed, with `\r\n` normalised to `\n`. `Filter.Matches` implements the Global Constraints matching rules. A zero `To` means unbounded.

- [ ] **Step 5: Run tests, confirm they pass**

Run: `go test ./internal/...` Expected: PASS.

- [ ] **Step 6: Commit**

`git add -A && git commit -m "feat: core types and PerfectMind class conversion"`

---

### Task 2: PerfectMind client

**Files:**
- Create: `internal/perfectmind/client.go`
- Test: `internal/perfectmind/client_test.go`

**Interfaces:**
- Consumes: Task 1 types.
- Produces:
  ```go
  type Options struct {
      BaseURL, WidgetID, UserAgent string
      Delay    time.Duration // between every request, including token GETs
      Timeout  time.Duration // per request, default 30s
      Retries  int           // per POST on non-JSON / 5xx / network error, default 2, backoff 3s*attempt
      MaxPages int           // default 40
      HTTPClient *http.Client // optional; client sets a cookiejar if nil Jar
  }
  func New(o Options) *Client
  func (c *Client) Calendars(ctx context.Context) ([]Calendar, error) // flattened across groups; calendars with empty BookingLink are dropped (logged at info)
  func (c *Client) Classes(ctx context.Context, cal Calendar, from, to time.Time) ([]RawClass, error)
  func (c *Client) AbsURL(path string) string
  ```

Token flow: GET the HTML page and extract `name="__RequestVerificationToken" ... value="..."` with a regexp (attribute order may vary). POST form-urlencoded with that token, the same cookie jar, `X-Requested-With: XMLHttpRequest`, and `Referer` set to the page URL. `Calendars` uses the widget page plus `GetCategoriesDataV2?embed=False`. `Classes` uses the booking page plus `ClassesV2`.

Pagination algorithm (`Classes`), from the confirmed spec:
```
start := date(from)
for page := 0; page < MaxPages; page++ {
    resp := post(Value=start 00:00Z, Value2=date(to) 00:00Z, page=0)
    if len(resp.Classes)==0 { break }
    append
    next := parse(resp.NextKey,"2006-01-02") + 1 day
    if !next.After(start) || !next.Before(to) { break }
    start = next
}
if page hit MaxPages → return what we have plus a warning log, not an error
```
The response is JSON only when `Content-Type` contains `application/json` and the body decodes. Anything else (HTML "Oops" page, a Queue-it redirect to another host) counts as retryable. After the retries run out, return an error that names the calendar.

- [ ] **Step 1: Write failing tests using an `httptest.Server` fake PerfectMind**

The fake serves the booking/widget page HTML containing a token input and sets a cookie. It serves `ClassesV2`/`GetCategoriesDataV2` and asserts the token, cookie and `X-Requested-With` are present (400 otherwise). It returns pages keyed by the posted `values[0][Value]` date.
```go
func TestCalendarsDropsNullBookingLink(t *testing.T) // categories.json fixture → no entry with empty BookingLink; contains "Aquafit"
func TestClassesPaginates(t *testing.T)              // 3 pages then empty → all classes returned; posted start dates == [from, nextKey1+1d, nextKey2+1d]
func TestClassesStopsAtHorizon(t *testing.T)         // nextKey+1 >= to → no further request
func TestClassesStopsWhenNextKeyStalls(t *testing.T) // same nextKey twice → stops, no infinite loop
func TestClassesRetriesOnHTML(t *testing.T)          // first POST returns text/html "Oops", second JSON → success; always HTML → error mentioning calendar name
func TestClassesSendsTokenAndCookie(t *testing.T)    // fake rejects missing token → test passes only when client sends both
func TestDelayBetweenRequests(t *testing.T)          // Delay 50ms, 3 requests → elapsed >= 100ms
```

- [ ] **Step 2: Run, confirm fail.** `go test ./internal/perfectmind/ -run 'Calendars|Classes|Delay'` gives FAIL.
- [ ] **Step 3: Implement `client.go`.**
- [ ] **Step 4: Run, confirm pass.** `go test ./internal/perfectmind/...` gives PASS.
- [ ] **Step 5: Commit.** `git commit -am "feat: PerfectMind client with token flow and pagination"` (add new files first).

---

### Task 3: SQLite store

**Files:**
- Create: `internal/store/store.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: `schedule.Session`, `schedule.Filter`, `schedule.Status`.
- Produces:
  ```go
  func Open(path string) (*Store, error)          // creates schema; WAL mode; busy_timeout
  func (s *Store) Close() error
  func (s *Store) ReplaceCategory(ctx context.Context, category string, sessions []schedule.Session) error
  func (s *Store) RemoveCategoriesExcept(ctx context.Context, keep []string) error
  func (s *Store) Prune(ctx context.Context, endedBefore time.Time) (int, error)
  func (s *Store) Sessions(ctx context.Context, f schedule.Filter) ([]schedule.Session, error) // sorted by Start, then Name; Categories populated & sorted
  func (s *Store) Categories(ctx context.Context) ([]CategoryCount, error) // {Name string `json:"name"`; Count int `json:"count"`}, sorted by name
  func (s *Store) Centres(ctx context.Context) ([]CentreCount, error)      // {Name string; Address schedule.Address; Count int}, json name/address/count
  func (s *Store) SaveStatus(ctx context.Context, st schedule.Status) error
  func (s *Store) LoadStatus(ctx context.Context) (schedule.Status, error) // zero Status if none
  ```
Schema (the implementer may add indexes):
- `sessions(id TEXT PRIMARY KEY, start_unix INTEGER, end_unix INTEGER, data TEXT /* JSON of Session minus Categories */)`
- `session_categories(session_id TEXT, category TEXT, PRIMARY KEY(session_id, category))`
- `status(id INTEGER PRIMARY KEY CHECK(id=1), data TEXT)`

`ReplaceCategory` runs in one transaction:
1. Delete that category's links.
2. Upsert each session's row (de-duplicate the input by ID first).
3. Insert the links.
4. Delete sessions that have no links left.

`RemoveCategoriesExcept` deletes the links for categories not in `keep`, then removes orphans. Filtering can be done in SQL for the time range and in Go via `Filter.Matches` for the rest. `Limit` applies after filtering.

- [ ] **Step 1: Write failing tests** (use `Open(filepath.Join(t.TempDir(), "t.db"))`)
```go
func TestReplaceCategoryRoundTrip(t *testing.T)        // save 2 sessions, Sessions(Filter{}) returns both with all fields equal (Start equal via time.Equal, loc Toronto)
func TestReplaceCategoryRemovesMissing(t *testing.T)   // replace with [A,B] then [A] → only A
func TestSessionInTwoCategories(t *testing.T)          // A in "Aquafit" and "Swimming" → one result, Categories ["Aquafit","Swimming"]; Categories() counts 1 each
func TestReplaceCategoryKeepsSharedSession(t *testing.T) // then replace "Swimming" with [] → A still present with ["Aquafit"]
func TestRemoveCategoriesExcept(t *testing.T)
func TestPrune(t *testing.T)                           // ended before cutoff removed, others kept, returns count
func TestFilterMatching(t *testing.T)                  // centre "CORNELL" matches "Cornell Community Centre"; two centres ORed; category+centre ANDed; unknown → empty slice (not nil error)
func TestFilterTimeRangeAndLimit(t *testing.T)
func TestStatusRoundTrip(t *testing.T)                 // LoadStatus on fresh db → zero Status, no error
func TestReopenPersists(t *testing.T)
```
- [ ] **Step 2: Run, confirm fail.** `go test ./internal/store/` gives FAIL.
- [ ] **Step 3: Implement** with `database/sql` + `_ "modernc.org/sqlite"` (driver name `sqlite`). `go get modernc.org/sqlite`.
- [ ] **Step 4: Run, confirm pass.** `go test ./internal/store/` gives PASS. `CGO_ENABLED=0 go build ./...` succeeds.
- [ ] **Step 5: Commit.** `git add -A && git commit -m "feat: SQLite store with many-to-many categories"`

---

### Task 4: Scraper

**Files:**
- Create: `internal/scraper/scraper.go`
- Test: `internal/scraper/scraper_test.go`

**Interfaces:**
- Consumes: `perfectmind.Calendar`, `perfectmind.RawClass`, `perfectmind.ToSession`, `*store.Store`.
- Produces:
  ```go
  type Source interface {
      Calendars(ctx context.Context) ([]perfectmind.Calendar, error)
      Classes(ctx context.Context, cal perfectmind.Calendar, from, to time.Time) ([]perfectmind.RawClass, error)
      AbsURL(path string) string
  }
  type Config struct { Horizon, Retention, Interval time.Duration; Loc *time.Location; Now func() time.Time }
  func New(src Source, st *store.Store, cfg Config, log *slog.Logger) *Scraper
  func (s *Scraper) Refresh(ctx context.Context) (schedule.Status, error) // mutex: concurrent call returns ErrBusy
  func (s *Scraper) Run(ctx context.Context) // refresh at start if LastSuccess nil or older than Interval, then every Interval until ctx done
  var ErrBusy = errors.New("refresh already running")
  ```
Refresh semantics:
1. Discover calendars. If discovery fails, record the error in status and change nothing else.
2. For each calendar in order, fetch classes from today 00:00 Toronto to `now+Horizon` and convert them. Skip rows that fail to convert, with a warning, and don't fail the category for them. Then `ReplaceCategory`. A failed fetch leaves that category untouched and records `OK:false, Error`.
3. `RemoveCategoriesExcept(names of discovered calendars)`.
4. `Prune(now - Retention)`.
5. Set `LastAttempt` = now. Set `LastSuccess` = now only if at least one category succeeded. `Total` = count of all stored sessions. Save the status.

Log the latest session date seen per category (the spec asks the builder to observe the publishing horizon).

- [ ] **Step 1: Write failing tests** with a fake `Source` and a real temp `Store`.
```go
func TestRefreshStoresAllCategories(t *testing.T)
func TestRefreshKeepsFailedCategory(t *testing.T)   // run 1 ok for A,B; run 2 A errors → A's old sessions remain, status A OK=false, LastSuccess updated
func TestRefreshDiscoveryFailure(t *testing.T)      // Calendars error → store unchanged, Status.Error set, LastSuccess unchanged
func TestRefreshDropsVanishedCategory(t *testing.T)
func TestRefreshPrunesOld(t *testing.T)             // session ended now-Retention-1h removed; ended now-1h kept
func TestRefreshBusy(t *testing.T)                  // second concurrent Refresh → ErrBusy
func TestRefreshSkipsBadRows(t *testing.T)
```
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run, confirm pass.** `go test ./internal/scraper/`
- [ ] **Step 5: Commit.** `git commit -m "feat: scraper with per-category failure isolation"`

---

### Task 5: iCalendar writer

**Files:**
- Create: `internal/ics/ics.go`
- Test: `internal/ics/ics_test.go`

**Interfaces:**
- Consumes: `schedule.Session`.
- Produces:
  ```go
  type Options struct { Name string; Refresh time.Duration; Now time.Time }
  func Write(w io.Writer, sessions []schedule.Session, o Options) error
  ```
Output rules:
- CRLF line endings, folded at 75 octets (continuation lines start with a single space, and a fold never splits a UTF-8 rune).
- Escape `\` `;` `,` and newline (`\n`) in text values.
- Header: `VERSION:2.0`, `PRODID:-//markham-schedule//EN`, `CALSCALE:GREGORIAN`, `METHOD:PUBLISH`, `X-WR-CALNAME:{Name}`, `X-WR-TIMEZONE:America/Toronto`, `REFRESH-INTERVAL;VALUE=DURATION:PT{h}H`, `X-PUBLISHED-TTL:PT{h}H`, then the VTIMEZONE below verbatim.
- Each VEVENT:
  - `UID:{id}@markham-schedule`
  - `DTSTAMP:{Now UTC}Z`
  - `DTSTART;TZID=America/Toronto:20261029T200500` and `DTEND` (all-day: `DTSTART;VALUE=DATE:20261029` and DTEND the next day)
  - `SUMMARY:{Name}`
  - `LOCATION:{Centre} – {Facility}, {Street}, {City}` (skip empty parts)
  - `DESCRIPTION`: lines `Category: a, b` / `Age: …` / `Price: …` / `Spots: …` (only if non-empty, noting "as of {ScrapedAt local}") / `Course ID: …` / blank line / Description / blank line / BookingURL
  - `URL:{BookingURL}`
  - De-duplicate by ID defensively.

```
BEGIN:VTIMEZONE
TZID:America/Toronto
BEGIN:DAYLIGHT
TZOFFSETFROM:-0500
TZOFFSETTO:-0400
TZNAME:EDT
DTSTART:19700308T020000
RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU
END:DAYLIGHT
BEGIN:STANDARD
TZOFFSETFROM:-0400
TZOFFSETTO:-0500
TZNAME:EST
DTSTART:19701101T020000
RRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU
END:STANDARD
END:VTIMEZONE
```

- [ ] **Step 1: Write failing tests**
```go
func TestWriteBasicEvent(t *testing.T)   // contains "UID:abc@markham-schedule", "DTSTART;TZID=America/Toronto:20261101T200500", "SUMMARY:Drop-In Aquafit: Shallow/Deep" (colon in value not escaped), CRLF everywhere
func TestEscaping(t *testing.T)          // "a, b; c\nd\\e" → "a\, b\; c\nd\\e"
func TestFolding(t *testing.T)           // no physical line > 75 octets; unfolding (remove CRLF+space) restores original; multibyte "–" not split
func TestAllDay(t *testing.T)
func TestNoDuplicateUIDs(t *testing.T)   // same session twice in input → one VEVENT
func TestEmptyCalendarValid(t *testing.T) // BEGIN/END:VCALENDAR + VTIMEZONE, zero VEVENT
```
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run, confirm pass.** `go test ./internal/ics/`
- [ ] **Step 5: Commit.** `git commit -m "feat: iCalendar writer"`

---

### Task 6: HTTP API

**Files:**
- Create: `internal/api/api.go`
- Test: `internal/api/api_test.go`

**Interfaces:**
- Consumes: `*store.Store` (through the interface below), `ics.Write`.
- Produces:
  ```go
  type Store interface {
      Sessions(ctx context.Context, f schedule.Filter) ([]schedule.Session, error)
      Categories(ctx context.Context) ([]store.CategoryCount, error)
      Centres(ctx context.Context) ([]store.CentreCount, error)
      LoadStatus(ctx context.Context) (schedule.Status, error)
  }
  type Options struct {
      Token, CalName, Version string
      Refresh, Retention time.Duration
      FeedDays int            // default 60
      Loc *time.Location
      Now func() time.Time
  }
  func New(st Store, o Options) http.Handler
  ```
Routes (stdlib `http.ServeMux` method patterns), per the spec "HTTP interface":
- `GET /calendar.ics`
- `GET /api/sessions`
- `GET /api/categories`
- `GET /api/centres`
- `GET /api/status`, whose response adds `"version"` and `"updated_at"` (= LastSuccess)
- `GET /healthz`

Details:
- `/api/sessions` returns `{"updated_at": LastSuccess|null, "count": n, "sessions": [...]}`. The `sessions` key must be `[]`, never `null`. `limit` defaults to 500, is clamped to 5000, and a non-integer gives 400.
- `from`/`to` are `YYYY-MM-DD` in Toronto. `to` is inclusive, so it's converted to the start of the next day. A bad date gives 400 `{"error": "..."}`.
- `/calendar.ics` takes `days` (1–365, default `FeedDays`). Its window runs from now−Retention to now+days. Content-Type is `text/calendar; charset=utf-8`.
- With `Token` set, every route except `/healthz` requires `?token=` or `Authorization: Bearer`. Otherwise 401 JSON. Compare with `subtle.ConstantTimeCompare`.

- [ ] **Step 1: Write failing tests** with a fake Store and `httptest`.
```go
func TestSessionsFilters(t *testing.T)   // ?category=AQUAFIT&centre=cornell&centre=angus+glen → Filter passed to store has those values; response shape
func TestSessionsBadParams(t *testing.T) // from=2026-13-01 → 400 JSON; limit=abc → 400
func TestSessionsDefaultFromToday(t *testing.T) // Filter.From == today 00:00 Toronto
func TestCalendarWindowAndType(t *testing.T)    // From == now-Retention, To == now+60d; days=7 honoured; days=0 → 400
func TestEmptyStore(t *testing.T)        // no sessions, zero status → /api/sessions {"updated_at":null,"count":0,"sessions":[]}; /calendar.ics 200 valid VCALENDAR
func TestTokenAuth(t *testing.T)         // Token set: no token 401, ?token=x 200, Bearer x 200, /healthz 200 without token
func TestCategoriesCentresStatus(t *testing.T)
```
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run, confirm pass.** `go test ./internal/api/`
- [ ] **Step 5: Commit.** `git commit -m "feat: HTTP API and calendar feed"`

---

### Task 7: Main binary, config, packaging, CI, README

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`, `cmd/markham-schedule/main.go`, `Dockerfile`, `.dockerignore`, `docker-compose.yml`, `.github/workflows/docker.yml`, `README.md`, `.gitignore`

**Interfaces:**
- Consumes: everything above.
- Produces:
  ```go
  type Config struct {
      ListenAddr, DataDir, APIToken, LogLevel string
      RefreshInterval, RequestDelay, Horizon, Retention time.Duration
  }
  func Load(getenv func(string) string) (Config, error) // defaults from Global Constraints; invalid duration → error naming the var
  ```
`main.go`:
- `var version = "dev"`, set via `-ldflags "-X main.version=..."`.
- `import _ "time/tzdata"`.
- Flags: `-once` runs one Refresh, prints a per-category summary table (name, ok, count, error) plus the total, and exits non-zero if every category failed. `-version` prints the version.
- Otherwise: open the store at `DataDir/schedule.db`, start `scraper.Run` in a goroutine, and serve `api.New` with `CalName "Markham Drop-In"` and `FeedDays 60`.
- Shut down gracefully on SIGINT/SIGTERM (10s).
- Use the UA string `Mozilla/5.0 (compatible; markham-schedule/{version}; +https://github.com/vBuilt/markham-schedule)`. If PerfectMind rejects it during the live check, fall back to a browser-like UA and note it in the README.

`Dockerfile`:
- Build stage `golang:1.25-alpine`. Use the `TARGETOS`/`TARGETARCH` build args: `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}"`.
- Final stage `gcr.io/distroless/static-debian12:nonroot`.
- `ENV DATA_DIR=/data`, `VOLUME /data`, `EXPOSE 8080`.
- `HEALTHCHECK` isn't possible in distroless without a shell or curl. Add a `-healthcheck` flag to the binary that GETs `http://127.0.0.1{LISTEN_ADDR}/healthz` and exits 0 or 1, and use `HEALTHCHECK CMD ["/markham-schedule","-healthcheck"]`.
- `docker-compose.yml`: image `ghcr.io/vbuilt/markham-schedule:latest`, port `8080:8080`, volume `/mnt/user/appdata/markham-schedule:/data`, env with the defaults, `restart: unless-stopped`.
- Note: distroless nonroot runs as UID 65532. The README must tell Unraid users to `chown 65532:65532` the appdata folder, or set `--user 99:100` (Unraid's nobody:users) in Extra Parameters.

`.github/workflows/docker.yml`:
- Triggers on push to `main`, on tags `v*`, and on `workflow_dispatch`.
- A `test` job runs `go vet ./...` and `go test ./...`.
- A `docker` job needs `test`. Give it `permissions: packages: write, contents: read`. Steps: `docker/setup-qemu-action`, `docker/setup-buildx-action`, `docker/login-action` to ghcr.io with `GITHUB_TOKEN`, then `docker/metadata-action` with tags `type=raw,value=latest,enable={{is_default_branch}}`, `type=sha,prefix=sha-`, `type=semver,pattern={{version}}`, `type=semver,pattern={{major}}.{{minor}}`.
- Then `docker/build-push-action` with `platforms: linux/amd64,linux/arm64`, `push: true`, and build-arg `VERSION` set to the tag name or short SHA.

`README.md`:
- What it does, the endpoints with example URLs (`http://vnas:8080/calendar.ics?category=aquafit&centre=cornell`), and the env var table.
- Unraid setup: Docker tab → Add Container. Fields: repository `ghcr.io/vbuilt/markham-schedule:latest`, port, path `/data`, and env vars. Also the appdata permissions note.
- How to subscribe in Google Calendar ("From URL", which only works if the URL is reachable from Google, i.e. not a LAN address) and in Apple Calendar (works on the LAN).
- Fixture re-capture, and a caveat that the source is an undocumented API.

- [ ] **Step 1: Write failing `config_test.go`:** `TestLoadDefaults` (all defaults exactly as in Global Constraints), `TestLoadOverrides`, `TestLoadBadDuration` (`REFRESH_INTERVAL=abc` → error contains "REFRESH_INTERVAL").
- [ ] **Step 2: Run, confirm fail; implement `config.go`; run, confirm pass.** `go test ./internal/config/`
- [ ] **Step 3: Implement `main.go`.** `go build ./... && go vet ./...` must succeed.
- [ ] **Step 4: Live check.** Run `DATA_DIR=$(mktemp -d) go run ./cmd/markham-schedule -once`. Expected: about 13 categories OK, a total in the hundreds or more, and it finishes within ~5 minutes. Then run the server with the same `DATA_DIR` and check:
  - `curl -s localhost:8080/api/status`: `last_success` is set.
  - `curl -s 'localhost:8080/api/sessions?category=aquafit&centre=cornell&limit=3'`: Cornell Aquafit sessions with correct local times. Compare one against the PerfectMind website.
  - `curl -s 'localhost:8080/calendar.ics?category=aquafit' | head -40`: a valid header and events.
- [ ] **Step 5: Write Dockerfile, .dockerignore, compose, workflow, README, .gitignore** (ignore `*.db`, `/data`). Verify the image builds: `docker build --build-arg VERSION=test -t markham-schedule:test .` followed by `docker run --rm markham-schedule:test -version` printing `test`. If Docker isn't available locally, say so in the report and rely on CI.
- [ ] **Step 6: Run the full suite.** `go test ./... && CGO_ENABLED=0 go build ./...` gives PASS.
- [ ] **Step 7: Commit.** `git add -A && git commit -m "feat: main binary, Docker image, CI publish to GHCR, README"`
- [ ] **Step 8: (Owner confirmation required before running)** Create the public GitHub repo `vBuilt/markham-schedule`, push `main`, watch the Actions run, and confirm `ghcr.io/vbuilt/markham-schedule:latest` exists. GHCR packages are private by default even when the repo is public. In package settings, set visibility to Public, or tell the owner to.
