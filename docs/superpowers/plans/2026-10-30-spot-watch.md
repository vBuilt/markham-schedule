# Spot Watching Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Signed-in, confirmed users can watch a full session. A checker looks at watched sessions
about once a minute and emails every watcher each time the session goes from full to having a
spot, until the session starts. The calendar shows your watches, the status page lists them, and
the admin sees everyone's.

**Architecture:**
- **Store:** new tables, store methods and a single-session update.
- **New package `internal/watch`:** a `Checker` loop that does its own small PerfectMind fetches
  through a separate `perfectmind.Client`, plus an `HTTP` type for the endpoints and the stop
  page.
- **Auth:** `auth` gains admin detection and exports its CSRF wrapper.
- **Pages:** the embedded pages call the new JSON endpoints.

**Tech Stack:** Go 1.25 stdlib `net/http`, modernc.org/sqlite, embedded HTML, existing `auth.Mailer`.

**Spec:** `docs/superpowers/specs/2026-10-30-spot-watch-design.md` (read its "Settled after the builder check" section; it overrides earlier lines where they differ).

## Global Constraints

- Spot checks:
  - Only a parsed `spots_left > 0` counts as open.
  - An email goes out only when the stored state moves from `last_spots == 0` to `> 0`.
  - A nil value never changes the state or sends an email.
  - A new watch starts the state at `last_spots = 0`.
- Limits: `MaxPerUser = 10` active watches per user, `MaxGroups = 20` distinct (first category, day)
  groups across all watched sessions, `MissLimit = 3`.
- `WATCH_INTERVAL` defaults to `1m`, with a minimum of `30s`. `ADMIN_EMAILS` is a comma-separated
  list, normalised with `auth.NormalizeEmail`.
- Error messages, verbatim:
  - `409` `This session isn't full.`
  - `409` `This session has already started.`
  - `404` `Session not found.`
  - `429` `You can watch at most 10 sessions at a time.`
  - `503` `Spot watching is at capacity right now. Try again later.`
- Every watch endpoint needs a confirmed user (`auth.HTTP.RequireConfirmed`). Every POST goes
  through the CSRF check. `/api/watches/all` also needs admin, and answers `403` `Admins only.`
  otherwise.
- `/watch/stop?t=` works without sign-in. Stop tokens are 32 random bytes, and only their SHA-256
  is stored. Each email gets a fresh token, and all of a watch's tokens stay valid until the watch
  is deleted.
- Email: subject `A spot opened: <name>, <Mon Jan 2>, <3:04 p.m.>`. Times are in America/Toronto,
  lower-case `a.m.`/`p.m.`, matching the site.
- Public routes and `/api/status` JSON don't change. Signed-out pages make no watch requests.
- Never log email addresses at info level or above, nor any token.

## Review Focus

1. **Session sits under several categories, and the first one doesn't list it.** The checker must
   try the next category before counting a miss. Pinned in Task 3 (`TestCycleTriesNextCategory`).
2. **PerfectMind errors or returns an empty day mid-watch.** No state change, no miss, no email.
   Pinned in Task 3 (`TestCycleErrorNoChange`).
3. **Session refills and reopens repeatedly.** One email per full→open move, never on open→open.
   Pinned in Task 3 (`TestCycleEmailsOnEachReopen`).
4. **User deleted while watching.** Their watches go with them. Pinned in Task 1
   (`TestDeleteUserRemovesWatches`).
5. **A second click on an emailed Stop link, or a link from an earlier email.** Shows "already
   stopped" or stops, and never errors. Pinned in Task 4 (`TestStopLink`).

---

### Task 1: Store: watch tables and session spot update

**Files:**
- Create: `internal/store/watches.go`, `internal/store/watches_test.go`
- Modify: `internal/store/store.go` (schema), `internal/store/accounts.go` (`DeleteUser` cascade)

**Interfaces:**
- Produces (package `store`):
  ```go
  var ErrWatchExists = errors.New("already watching")
  type Watch struct {
      ID int64; UserID int64; SessionID string; SessionStart time.Time
      Created time.Time; EmailsSent int; LastEmailed *time.Time
  }
  type WatchState struct {
      SessionID string; LastSpots int; LastChecked *time.Time; Misses int; LastError string
  }
  func (s *Store) SessionByID(ctx, id string) (schedule.Session, error)            // ErrNotFound
  func (s *Store) UpdateSessionSpots(ctx, id, spots string, left *int, at time.Time) error
  func (s *Store) AddWatch(ctx, userID int64, sessionID string, start, now time.Time) (Watch, error) // ErrWatchExists; inserts watch_state(last_spots=0) if absent
  func (s *Store) DeleteWatch(ctx, userID int64, sessionID string) (bool, error)
  func (s *Store) AddStopToken(ctx, watchID int64, hash []byte) error
  func (s *Store) DeleteWatchByStopToken(ctx, hash []byte) (bool, error)
  func (s *Store) UserWatches(ctx, userID int64) ([]Watch, error)   // by SessionStart asc
  func (s *Store) AllWatches(ctx) ([]Watch, error)                  // by SessionStart, UserID
  func (s *Store) WatchersOf(ctx, sessionID string) ([]Watch, error)
  func (s *Store) MarkEmailed(ctx, watchID int64, now time.Time) error // emails_sent++, last_emailed=now
  func (s *Store) WatchStates(ctx) (map[string]WatchState, error)    // only sessions with ≥1 watch
  func (s *Store) SaveWatchState(ctx, st WatchState) error
  func (s *Store) DeleteSessionWatches(ctx, sessionID string) error  // watches, their tokens, state
  func (s *Store) PurgeWatches(ctx, now time.Time) (int64, error)    // SessionStart<=now, plus orphaned tokens/state
  ```
- Tables:
  - `watches(id INTEGER PK AUTOINCREMENT, user_id, session_id, session_start_unix, created_unix, emails_sent DEFAULT 0, last_emailed_unix NULL, UNIQUE(user_id, session_id))`
  - `watch_stop_tokens(token_hash BLOB PK, watch_id)`
  - `watch_state(session_id PK, last_spots INTEGER NOT NULL DEFAULT 0, last_checked_unix NULL, misses DEFAULT 0, last_error TEXT DEFAULT '')`
  - All are added to the `CREATE TABLE IF NOT EXISTS` schema.

- [ ] **Step 1: Write failing tests** in `watches_test.go` (use `openT`, `t0`):
  - `TestAddWatchAndList`:
    - Add, then `UserWatches` returns it, with `SessionStart` and `Created` round-tripped.
    - A second `AddWatch` with the same pair returns `ErrWatchExists`.
    - `WatchStates()[sid].LastSpots == 0`.
  - `TestDeleteWatchKeepsOthersState`:
    - Two users watch one session, and one deletes. The state remains.
    - When the last watcher deletes, `WatchStates` no longer has the session.
  - `TestStopTokens`:
    - Two tokens on one watch. Deleting by the first returns `true`.
    - Deleting by the second then returns `false` and does not error.
  - `TestPurgeWatches`: a watch whose start ≤ now is gone along with its tokens; a future watch remains.
  - `TestMarkEmailed`: `EmailsSent == 1` and `LastEmailed == now`.
  - `TestSessionByIDAndUpdateSpots`:
    - `ReplaceCategory` stores a session, then `UpdateSessionSpots(id, "2 spots", &2, t1)`.
    - `SessionByID` now has `Spots=="2 spots"`, `*SpotsLeft==2`, `ScrapedAt==t1`.
    - A missing ID gives `ErrNotFound`.
  - `TestDeleteUserRemovesWatches`: `DeleteUser` removes the user's watches and stop tokens.
- [ ] **Step 2:** `go test ./internal/store/ -run 'Watch|Stop|Purge|MarkEmailed|SessionByID|DeleteUserRemoves'` should fail to compile.
- [ ] **Step 3: Implement.**
  - `UpdateSessionSpots` uses `json_set(data, '$.spots', ?, '$.spots_left', ?, '$.scraped_at', ?)`, with `scraped_at` in RFC3339Nano UTC to match the existing JSON encoding.
  - `DeleteUser` gains deletes of `watch_stop_tokens` for the user's watches and of `watches`, inside its transaction. Empty `watch_state` rows are removed too.
- [ ] **Step 4:** `go test ./internal/store/` should PASS.
- [ ] **Step 5: Commit** `feat(store): watch tables and single-session spot update`.

### Task 2: Config and admin flag

**Files:**
- Modify: `internal/config/config.go` (+ test), `internal/auth/service.go`, `internal/auth/http.go` (+ tests)

**Interfaces:**
- `config.Config` gains `WatchInterval time.Duration` (`WATCH_INTERVAL`, default 1m, error if < 30s) and `AdminEmails []string` (`ADMIN_EMAILS`, each normalised; an invalid entry is a startup error).
- `auth.Options` gains `AdminEmails []string`, and adds `func (s *Service) IsAdmin(u store.User) bool`, which requires `u.Confirmed` and a matching email.
- `GET /api/auth/me` user JSON gains `"is_admin": bool`.
- Exported: `func (h *HTTP) CSRF(next http.HandlerFunc) http.HandlerFunc`, the existing `csrf` renamed, with internal callers updated. Also `func (h *HTTP) IsAdmin(u store.User) bool`, which delegates.

- [ ] **Step 1: Failing tests.**
  - Config:
    - `WATCH_INTERVAL=20s` is an error, and the default is `time.Minute`.
    - `ADMIN_EMAILS=" A@x.com ,b@y.com"` gives `["a@x.com","b@y.com"]`.
    - `ADMIN_EMAILS=nope` is an error.
  - Auth: `TestIsAdmin`:
    - An unconfirmed admin email gives false.
    - A confirmed one gives true.
    - Others give false.
    - `me` shows `is_admin`.
- [ ] **Step 2:** Run them; they should FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** `go test ./internal/config/ ./internal/auth/` should PASS.
- [ ] **Step 5: Commit** `feat: WATCH_INTERVAL, ADMIN_EMAILS and admin flag`.

### Task 3: Checker

**Files:**
- Create: `internal/watch/checker.go`, `internal/watch/checker_test.go`

**Interfaces:**
- Consumes: the Task 1 store methods; `perfectmind.Calendar`, `perfectmind.RawClass`, `perfectmind.ToSession`, `perfectmind.SessionID`, `perfectmind.ParseSpots`; `auth.Mailer`, `auth.NewToken`.
- Produces:
  ```go
  const (MaxPerUser = 10; MaxGroups = 20; MissLimit = 3)
  type Source interface {
      Calendars(ctx context.Context) ([]perfectmind.Calendar, error)
      Classes(ctx context.Context, cal perfectmind.Calendar, from, to time.Time) ([]perfectmind.RawClass, error)
  }
  type Store interface { /* the Task 1 methods the checker uses, plus UserByID */ }
  type Config struct {
      Interval time.Duration; Loc *time.Location
      BaseURL, APIToken string            // stop/booking links; APIToken appended as &token=
      Now func() time.Time; Async func(func()); Log *slog.Logger
  }
  type Summary struct { LastCycle *time.Time `json:"last_cycle"`; Sessions int `json:"sessions"`; People int `json:"people"` }
  func New(src Source, st Store, m auth.Mailer, cfg Config) *Checker
  func (c *Checker) Cycle(ctx context.Context)          // one pass; exported for tests
  func (c *Checker) Run(ctx context.Context)            // Cycle every Interval; next starts immediately if one overran; never overlapping
  func (c *Checker) Summary() Summary
  func GroupKey(s schedule.Session) string              // first category + "|" + YYYY-MM-DD (Loc)
  ```
- **Cycle algorithm.** The spec's settled section decides every branch:
  1. `PurgeWatches(now)`.
  2. Load `WatchStates`, then for each session ID call `SessionByID`. If the session is
     not found, count a miss.
  3. For each session, try its `Categories` in order. Fetch `(category, day)` lazily, and fetch each
     pair at most once per cycle, caching the result or error.
     - Resolve the calendar from a cached `Calendars()` map. On an unknown name, re-discover at most
       once every 10 minutes. If it is still unknown, treat it as an error for that category.
     - Fetch with `from` = day start and `to` = next day start (Loc). Match rows with
       `SessionID(raw) == id`.
  4. When the session is found:
     - Parse the spots. Call `UpdateSessionSpots`, building the new spots text with `ToSession`'s
       conversion or the raw string.
     - If the parsed value is non-nil, apply the email rule from Global Constraints, then
       `LastSpots = v`.
     - Set `Misses = 0` and `LastError = ""`.
  5. When it is not found and every tried category returned ≥1 row with no error, `Misses++`. At
     `MissLimit`, call `DeleteSessionWatches` (no email). Otherwise set `LastError`.
  6. Always set `LastChecked = now`, then `SaveWatchState`.
  7. Update `Summary`: distinct sessions, and distinct users from `AllWatches`.
- **Notify.** For each watch in `WatchersOf`:
  - Call `UserByID` and skip the user if they're missing.
  - Create a `NewToken()` and call `AddStopToken`.
  - Send in the background via `Async` with a fresh context and a 30s timeout.
  - Call `MarkEmailed`.
  - Email links:
    - "Book it now" goes to the session's `booking_url`.
    - "Stop watching" goes to `BaseURL + "/watch/stop?t=" + raw` (+ `&token=`).
  - The text and HTML bodies follow the spec mockup. HTML-escape every value.

- [ ] **Step 1: Failing tests** use a fake `Source` (per-calendar rows and errors, a call counter), the real store in a temp dir, a fake mailer that records `to` and `subject`, `Async` run inline, and a fixed clock. A helper seeds one full session under category "A" for tomorrow 19:00 and one confirmed user watching it.
  - `TestCycleEmailsOnOpen`:
    - The fake returns the row with `Spots: "1 spot"`.
    - One email to the user, with the subject verbatim per Global Constraints.
    - `EmailsSent == 1`, and the stored session shows `SpotsLeft == 1`.
  - `TestCycleNoEmailWhileOpen`: open on two cycles in a row sends exactly one email.
  - `TestCycleEmailsOnEachReopen`: the sequence full, open, full, open sends 2 emails.
  - `TestCycleNilSpotsKeepsState`: a `"Waitlist"` row after full sends no email and keeps `LastSpots == 0`.
  - `TestCycleErrorNoChange`:
    - The source errors, and then a later cycle sees an empty day.
    - No email and `Misses == 0`, with `LastError` set.
  - `TestCycleMissesCancel`:
    - The day has other rows but not this session, for 3 cycles.
    - The watches are deleted and no email is sent.
  - `TestCycleTriesNextCategory`:
    - The session is stored under "A" and "B". "A" returns other rows only, and "B" has the session open.
    - The email is sent and `Misses == 0`.
  - `TestCycleOneFetchPerGroup`: two watched sessions on the same calendar and day make 1 `Classes` call.
  - `TestCyclePurgesStarted`: after the start time there are no watches and no fetch.
  - `TestCycleNoWatchesNoRequests`: 0 `Calendars` and 0 `Classes` calls.
- [ ] **Step 2:** `go test ./internal/watch/` should FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** `go test -race ./internal/watch/` should PASS.
- [ ] **Step 5: Commit** `feat(watch): spot checker`.

### Task 4: Watch HTTP endpoints, stop page and wiring

**Files:**
- Create: `internal/watch/http.go`, `internal/watch/http_test.go`, `internal/web/watchstop.html`
- Modify:
  - `internal/web/web.go`: embed and add a `WatchStop` handler or a template.
  - `internal/api/api.go`: `Options.Watch` registers routes when non-nil.
  - `cmd/markham-schedule/main.go`: a second `perfectmind.New` with `Delay: 0`, then `watch.New`, `go checker.Run(ctx)`, `AdminEmails` into auth.

**Interfaces:**
- `func NewHTTP(c *Checker, st Store, a *auth.HTTP) *HTTP`; `func (h *HTTP) Register(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc)`
- Routes. All POSTs are wrapped `a.CSRF`, and all JSON routes `a.RequireConfirmed`:
  - `GET /api/watches`. Returns:
    `{watches:[{session_id, name, centre, start, spots, spots_left, booking_url, emails_sent, last_emailed}], summary: Summary}`.
    The session fields come from `SessionByID`. When the session is missing, return what the watch
    has (`start`) and `name:""`.
  - `POST /api/watches` `{session_id}`:
    - `201` with the watch, or `200` if it already exists.
    - Validation order:
      1. Session not found gives `404`.
      2. Started gives `409`.
      3. Not full gives `409`.
      4. The user already has `MaxPerUser` watches gives `429`.
      5. A new `GroupKey` beyond `MaxGroups` gives `503`.
  - `POST /api/watches/delete` `{session_id}` returns `204` whether or not it existed.
  - `GET /api/watches/all`: admin only. Returns
    `{watches:[{name, email, session_id, session_name, start, spots_left, emails_sent}]}`.
  - `GET /watch/stop?t=`. Needs no auth or CSRF, since it is a GET that deletes a watch by an
    unguessable token. It serves `watchstop.html`, with the message picked server-side:
    - "You've stopped watching this session." when a watch was deleted.
    - "This watch was already stopped." otherwise.
    - The page has a link to the calendar. Use `html/template`.

- [ ] **Step 1: Failing tests.** The harness builds an auth service and HTTP as in `internal/auth/http_test.go` (reuse the pattern, not the unexported helpers), plus a confirmed user and session cookie.
  - `TestWatchEndpointsNeedConfirmed`:
    - Signed out gives 401.
    - Unconfirmed gives 403.
    - A cross-site POST gives 403.
  - `TestAddWatchValidation`:
    - Each error status and its verbatim message.
    - The 11th watch gives 429.
    - The 21st group gives 503.
    - Adding the same session twice gives 200.
  - `TestListAndDelete`: add, list shows it with `summary`, delete gives 204, then the list is empty.
  - `TestAllWatchesAdminOnly`: a non-admin gets 403 `Admins only.`, and an admin sees other users' rows with their emails.
  - `TestStopLink`:
    - The first GET says "You've stopped…" and deletes the watch.
    - The same token again says "already stopped".
    - A token from an earlier email after a later one also works.
  - In `internal/api` tests: with `Options.Watch` set, `GET /api/watches` is 401 when signed out, and `/api/status` is unchanged.
- [ ] **Step 2:** Run them; they should FAIL.
- [ ] **Step 3: Implement and wire.** In `main.go`, log once at startup: `spot watching on`, with the interval and admin count (not the addresses).
- [ ] **Step 4:** `go vet ./... && go test -race ./...` should PASS.
- [ ] **Step 5: Commit** `feat(watch): endpoints, stop link and wiring`.

### Task 5: Calendar and status page UI

**Files:**
- Modify: `internal/web/index.html`, `internal/web/status.html`, `internal/web/web_test.go`

Match the approved mockup (spec "Decided by the owner"), using the existing palette, fonts and dialog styles. Build all dynamic text with `textContent` or `esc()`, never raw user or session strings.

- **Calendar (`index.html`):**
  - After `me` resolves with a confirmed user, fetch `GET /api/watches` and keep a `Set` of watched IDs. Re-fetch after add or delete.
  - Tiles and list rows of watched sessions get a small "Watching" tag next to "Full".
  - Detail dialog, for a full, upcoming session only:
    - **Signed out:** a note: "<a>Sign in</a> to get an email when a spot opens." The link goes to `/login?next=/`.
    - **Signed in but unconfirmed:** "Confirm your email to watch for spots." with a link to `/account`.
    - **Confirmed, not watching:** an "Email me when a spot opens" button and the note "We check every minute until the session starts."
    - **Watching:** a green banner "Watching. We'll email <email> when a spot opens." and a "Stop watching" button.
    - Errors from the POST appear under the buttons.
  - A watched session that is now open still shows the Watching banner, so the user can stop.
- **Status (`status.html`):**
  - For a confirmed user, add a "Sessions you're watching" card. Its table has Session (name plus date, time and centre), Now ("Full" in red, "N left" in green, or the spots text), Last email ("Never" or a time) and a Stop button.
  - Under the table, a line: "Spot checker: last check N seconds ago, S sessions watched by P people." It reads from `summary`.
  - The card is empty-state text when there are no watches: "You're not watching any sessions. Open a full session on the calendar to watch it."
  - If `me.is_admin`, also add an "All watches" card from `/api/watches/all` (Who: name and email, Session, Now, Emails sent).
  - Signed-out visitors see neither card and make no requests.
  - While the page is visible, refresh the watch cards every 30s.
- [ ] **Step 1: Failing test** in `web_test.go`, following its existing style of asserting page markers:
  - `index.html` contains `Email me when a spot opens`, `Stop watching` and `/api/watches`.
  - `status.html` contains `Sessions you're watching`, `All watches` and `/api/watches/all`.
- [ ] **Step 2:** Run it; it should FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** `go test ./internal/web/` should PASS. Then run the server locally with `LogMailer`, sign up and confirm via the log link, and check the following in a browser at 420px and desktop widths:
  - Watch a full session.
  - See the tag and the status card.
  - Stop watching from the status page.
  - Set yourself as admin with `ADMIN_EMAILS` and see "All watches".
- [ ] **Step 5: Commit** `feat(web): watch button, calendar tag and status page watches`.

### Task 6: Docs

**Files:** `README.md`

- [ ] **Step 1:** Update the README:
  - Add a "Spot watching" section saying what it does, the 1-minute checks, the limits and admin.
  - Add `WATCH_INTERVAL` and `ADMIN_EMAILS` to the config table.
  - Add the new endpoints to the API table.
  - Add `-e ADMIN_EMAILS=you@example.com` to the docker run example. Use a placeholder, not the owner's address.
- [ ] **Step 2: Commit** `docs: spot watching`.
