# Registration Reminders Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Signed-in, confirmed users can set a reminder on a session and get one email 2 minutes before its Residents registration opens, with the booking link.

**Architecture:** A new `internal/remind` package mirrors `internal/watch`. It contains:
- a `Sender` loop that ticks every 15s over pending reminders, reading each session's current Residents time from the store and making no outside requests;
- an `HTTP` type with JSON endpoints and the cancel page.

New tables live in `internal/store/reminders.go`. The UI changes extend the existing watch code in `index.html` and `status.html`.

**Tech Stack:** Go 1.25, stdlib net/http, modernc.org/sqlite, embedded HTML, slog.

**Spec:** `docs/superpowers/specs/2026-10-31-registration-reminders-design.md`. Its "Settled after the builder check" section wins over the rest.

## Global Constraints

- Lead time: send at `residents − 2m`. Never send at or after `residents`; drop the reminder instead.
- Creating a reminder requires `residents − now > 2m15s` (lead plus one tick).
- Tick every 15 seconds. Make no requests to PerfectMind.
- Allow 10 pending reminders per user, as a constant. There is no global cap.
- Keep sent reminders for 7 days, then purge them.
- Mark a reminder sent before its email is queued. Never retry a failed send. Record failures in `send_error`.
- The cancel link uses the spot-watch pattern: a 32-byte token with only its SHA-256 stored. GET shows a confirm page. POST is CSRF-checked and deletes the reminder.
- The admin list reuses `IsAdmin` and returns `403 "Admins only."` to everyone else.
- Email subject: `Registration opens in 2 minutes: <name>, <Mon Jan 2>, <3:04 p.m.>`, using the session start in America/Toronto and the same `p.m.` style as `watch.Checker.clock`.
- If `booking_url` is empty, the email omits the button and points to `https://cityofmarkham.perfectmind.com`.
- Never put secrets in code or the README. No new config.

## Review Focus

1. **Service down across the send window.** When it comes back after `residents`, it drops the reminder and sends nothing. Test: `TestTickDropsAfterOpen`.
2. **Overlapping ticks or a restart mid-tick.** The atomic `MarkReminderSent` claim means one email at most. Tests: `TestMarkReminderSentOnce` (store) and `TestTickSendsOnce` (sender).
3. **Registration time moved.** Moved later: no send yet, and the stored time is updated. Moved earlier into the window: send now. Moved earlier past now: drop. Test: `TestTickFollowsMovedTime`.
4. **Session briefly missing, then back.** Skipped while missing and sent when it returns. Dropped only after the stored time passes. Test: `TestTickMissingSession`.
5. **Times shown in Toronto time,** not UTC, in the subject and body. Test: `TestEmailToronto`.

---

### Task 1: Reminder storage

**Files:**
- Create: `internal/store/reminders.go`, `internal/store/reminders_test.go`
- Modify: `internal/store/store.go` (schema), `internal/store/accounts.go` (`DeleteUser`)

**Interfaces:**
- Produces:
  ```go
  var ErrReminderExists = errors.New("reminder exists")
  type Reminder struct {
      ID, UserID  int64
      SessionID   string
      ResidentsAt time.Time  // last Residents time seen
      Created     time.Time
      SentAt      *time.Time
      SendError   string
  }
  func (s *Store) AddReminder(ctx, userID int64, sessionID string, residentsAt, now time.Time) (Reminder, error) // ErrReminderExists on (user, session) clash
  func (s *Store) DeleteReminder(ctx, userID int64, sessionID string) (bool, error)
  func (s *Store) UserReminders(ctx, userID int64) ([]Reminder, error)   // pending and sent, ordered by ResidentsAt
  func (s *Store) AllReminders(ctx) ([]Reminder, error)                  // same order
  func (s *Store) PendingReminders(ctx) ([]Reminder, error)              // sent_at IS NULL
  func (s *Store) SetReminderTime(ctx, id int64, residentsAt time.Time) error
  func (s *Store) MarkReminderSent(ctx, id int64, now time.Time) (bool, error) // true only for the caller that flipped sent_at from NULL
  func (s *Store) SetReminderError(ctx, id int64, msg string) error
  func (s *Store) DropReminder(ctx, id int64) error                      // reminder and its tokens
  func (s *Store) AddReminderCancelToken(ctx, reminderID int64, hash []byte) error
  func (s *Store) DeleteReminderByCancelToken(ctx, hash []byte) (bool, error) // deletes reminder and all its tokens
  func (s *Store) PurgeReminders(ctx, sentBefore time.Time) (int64, error)   // sent reminders older than sentBefore, with tokens
  ```
- Schema: `reminders(id INTEGER PRIMARY KEY, user_id, session_id, residents_unix, created_unix, sent_unix NULL, send_error TEXT NOT NULL DEFAULT '', UNIQUE(user_id, session_id))` and `reminder_cancel_tokens(token_hash BLOB PRIMARY KEY, reminder_id INTEGER NOT NULL)`, each with an index on the foreign column, added with `CREATE TABLE IF NOT EXISTS` beside the watch tables.

- [ ] **Step 1: Write failing tests** in `reminders_test.go`, following `watches_test.go`'s setup:
  - `TestAddReminder`: adds one, and a second add for the same user and session returns `ErrReminderExists`. `UserReminders` returns it with `ResidentsAt` equal to the input (compare `.Unix()`).
  - `TestMarkReminderSentOnce`: the first `MarkReminderSent` returns true and the second returns false. `PendingReminders` no longer lists it, but `UserReminders` still does, with `SentAt` set.
  - `TestSetReminderTimeAndError`: both values round-trip.
  - `TestDeleteReminderByCancelToken`: returns true once, then false, and the reminder is gone.
  - `TestPurgeReminders`: removes a reminder sent 8 days ago and keeps one sent 1 day ago and one still pending.
  - `TestDeleteUserRemovesReminders`: `DeleteUser` removes the user's reminders and cancel tokens.
- [ ] **Step 2: Run** `go test ./internal/store/ -run 'Reminder|DeleteUserRemoves'`. Expect FAIL (undefined).
- [ ] **Step 3: Implement** the schema, the methods, and two extra `DeleteUser` statements (tokens first, then reminders), ordered before `DELETE FROM users`. `MarkReminderSent` is `UPDATE … SET sent_unix=? WHERE id=? AND sent_unix IS NULL`, checking `RowsAffected()==1`.
- [ ] **Step 4: Run** `go test -race ./internal/store/`. Expect PASS.
- [ ] **Step 5: Commit** `feat(store): reminder tables`.

### Task 2: Sender loop and email

**Files:**
- Create: `internal/remind/sender.go`, `internal/remind/sender_test.go`
- Modify: `cmd/markham-schedule/main.go`

**Interfaces:**
- Consumes: Task 1's store methods; `store.Store.SessionByID`, `UserByID`; `auth.Mailer`, `auth.NewToken`; `schedule.Session.Registration.Residents *time.Time`.
- Produces:
  ```go
  package remind
  const (
      MaxPerUser = 10
      Lead       = 2 * time.Minute
      TickEvery  = 15 * time.Second
      MinAhead   = Lead + TickEvery // creation cutoff
      KeepSent   = 7 * 24 * time.Hour
  )
  type Store interface { /* the Task 1 methods it uses + SessionByID + UserByID */ }
  type Config struct {
      Loc               *time.Location
      BaseURL, APIToken string // cancel links; APIToken appended as &token=
      Now               func() time.Time
      Async             func(func())
      Log               *slog.Logger
  }
  func New(st Store, m auth.Mailer, cfg Config) *Sender  // defaults like watch.New
  func (s *Sender) Run(ctx context.Context)              // Tick now, then every TickEvery until ctx done
  func (s *Sender) Tick(ctx context.Context)
  ```
- Tick rule, per pending reminder `r` at `now`:
  1. Load the session. If it exists and has `Registration.Residents`, use that as `res`. If it differs from `r.ResidentsAt`, call `SetReminderTime`. Otherwise use `res = r.ResidentsAt`, marked "unconfirmed".
  2. If `!now.Before(res)`: `DropReminder`, log, and continue.
  3. If unconfirmed or `now.Before(res.Add(-Lead))`: continue.
  4. If `MarkReminderSent(r.ID, now)` is false: continue.
  5. Load the user. If not found, continue. Make a token, `AddReminderCancelToken`, and build the cancel URL `BaseURL + "/reminder/cancel?t=" + raw`, plus `&token=` when needed. Queue the send with `Async`. Its own 30s timeout context covers `m.Send`, and on error it calls `SetReminderError` using a `context.WithoutCancel` context.
  6. After the loop, `PurgeReminders(now.Add(-KeepSent))`.

  Store errors are logged and skip that reminder; they never stop the tick.
- Email: the subject is from Global Constraints. The plain-text and simple-HTML bodies follow `watch.Checker.email`'s shape:
  - intro: "Online registration for residents opens at <3:04 p.m.> on <Monday, January 2>."
  - the name, centre, and the session's day and time range
  - a "Book it" link, or the PerfectMind fallback line
  - "Cancel this reminder: <url>"

- [ ] **Step 1: Write failing tests** in `sender_test.go`. Use a fake store (in-memory, implementing `Store`), a fake mailer recording sends, `Async` run inline, and a settable clock. Residents is 17:30 Toronto on 2026-10-30, and the session starts 2026-10-31 14:30.
  - `TestTickNotBeforeLead`: at 17:27:59, no send and the reminder is still pending.
  - `TestTickSendsAtLead`: at 17:28:00, one send to the user's email. The subject is `Registration opens in 2 minutes: Drop-In Badminton: All Ages, Sat Oct 31, 2:30 p.m.`, the body contains the booking URL and `/reminder/cancel?t=`, and the reminder is marked sent.
  - `TestTickSendsOnce`: running two Ticks at 17:28:10 gives one send.
  - `TestTickDropsAfterOpen`: first Tick at 17:30:00 gives no send and the reminder is gone.
  - `TestTickFollowsMovedTime`:
    - Session residents moved to 18:00: no send at 17:28, and the stored `ResidentsAt` becomes 18:00.
    - Then at 17:58: a send.
    - A separate case where residents moved to 17:00 with now at 17:28: no send, dropped.
  - `TestTickMissingSession`:
    - Session absent at 17:28: no send, still pending.
    - Session restored at 17:29: a send.
    - A separate case where it is still absent at 17:30: dropped, no send.
  - `TestTickSendErrorRecorded`: the mailer returns an error, and `SendError` is non-empty while `SentAt` is set.
  - `TestEmailToronto`: with the session times in UTC, the subject and body show `2:30 p.m.` and `5:30 p.m.`.
  - `TestEmailNoBookingURL`: an empty `BookingURL` means the body contains `https://cityofmarkham.perfectmind.com` and no "Book it" link.
- [ ] **Step 2: Run** `go test ./internal/remind/`. Expect FAIL.
- [ ] **Step 3: Implement** `sender.go`.
- [ ] **Step 4: Wire** it in `main.go`, next to the spot checker. Build `remind.New(st, mailer, remind.Config{Loc: loc, BaseURL: publicURL, APIToken: cfg.APIToken, Log: log})` only where the checker is built (accounts and mail on). Run it in a goroutine, and join it before `st.Close` the same way `watchDone` is joined. Log `"registration reminders on"`.
- [ ] **Step 5: Run** `go vet ./... && go test -race ./internal/remind/ ./internal/store/`. Expect PASS.
- [ ] **Step 6: Commit** `feat(remind): reminder sender`.

### Task 3: Endpoints and cancel page

**Files:**
- Create: `internal/remind/http.go`, `internal/remind/http_test.go`
- Modify: `internal/web/web.go`, `internal/web/watchstop.html`, `internal/api/api.go`, `cmd/markham-schedule/main.go`

**Interfaces:**
- Consumes: Task 1 store; `auth.HTTP` (`RequireConfirmed`, `CSRF`, `IsAdmin`), `auth.UserFrom`, `auth.HashToken`.
- Produces:
  ```go
  type HTTPStore interface { SessionByID; AddReminder; DeleteReminder; DeleteReminderByCancelToken; UserReminders; AllReminders; UserByID }
  func NewHTTP(st HTTPStore, a *auth.HTTP, now func() time.Time) *HTTP
  func (h *HTTP) Register(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc)
  // web package:
  func ReminderCancel(w http.ResponseWriter, msg, cancelToken, apiToken string)
  ```
- Routes:
  - `GET /api/reminders` returns `{"reminders":[…]}`.
  - `POST /api/reminders`, CSRF-checked.
  - `POST /api/reminders/delete`, CSRF-checked.
  - `GET /api/reminders/all`. The admin check runs inside it.
  - `GET /reminder/cancel` shows the confirm page.
  - `POST /reminder/cancel`, CSRF-checked.
  - The `/api/*` routes are wrapped in `RequireConfirmed`, exactly as in `watch.HTTP.Register`.
- Reminder JSON: `session_id, name, centre, start, residents_at, remind_at (residents_at − Lead), booking_url, sent_at, send_error`. When the session is missing, name and centre are empty. The admin list adds `email`.
- `POST /api/reminders` checks in this order, under a mutex:
  1. Bad JSON → 400 `invalid request`.
  2. Unknown session → 404 `Session not found.`
  3. The user already has one for this session (pending or sent) → 200 with it.
  4. No `Registration.Residents` → 409 `Online registration time isn't known for this session.`
  5. `residents − now <= MinAhead` → 409 `Registration opens too soon for a reminder.`, or `Registration has already opened.` when `residents <= now`.
  6. The user has 10 or more pending → 429 `You can have at most 10 reminders at a time.`
  7. Otherwise 201. On `ErrReminderExists` from a race, return 200 with the existing reminder.
- `POST /api/reminders/delete` → 204.
- Cancel page: generalise `watchstop.html` and `WatchStop` so the template takes the page title, button label and form action, keeping the `same-origin` referrer meta and its comment. `WatchStop` keeps its current output. `ReminderCancel` uses the title `Reminder cancelled`, the button `Cancel reminder` and the action `/reminder/cancel`. Messages:
  - GET with a token: `Cancel this registration reminder?`
  - Successful POST: `Your reminder is cancelled.`
  - Otherwise: `This reminder was already cancelled.`

- [ ] **Step 1: Write failing tests** in `http_test.go`, following `watch/http_test.go`'s harness: a real temp `store.Store`, a real `auth` service, and a signed-in confirmed user.
  - `TestAddReminder201AndList`
  - `TestAddReminderAgain200`
  - `TestAddReminderNoResidents409`
  - `TestAddReminderTooSoon409`, at `residents − 2m15s`
  - `TestAddReminderOpened409`
  - `TestAddReminderLimit429`, the 11th
  - `TestAddReminderUnknown404`
  - `TestRemindersAllAdminOnly`, 403 for a non-admin and 200 for an admin with an `email` field
  - `TestRemindersCSRF`, a POST with a foreign Origin → 403
  - `TestRemindersRequireConfirmed`, an unconfirmed user → 403 or 401 as `RequireConfirmed` returns
  - `TestCancelGetDoesNotDelete`
  - `TestCancelPostSingleUse`, which shows the cancelled text first and then the already-cancelled text

  In `web`, the existing `WatchStop` tests must still pass unchanged.
- [ ] **Step 2: Run** `go test ./internal/remind/ ./internal/web/`. Expect FAIL.
- [ ] **Step 3: Implement** `http.go`, the template generalisation, `api.Options.Remind *remind.HTTP` registered inside the Auth block next to `Watch`, and the `main.go` wiring.
- [ ] **Step 4: Run** `go vet ./... && go test -race ./...`. Expect PASS.
- [ ] **Step 5: Commit** `feat(remind): reminder endpoints and cancel page`.

### Task 4: Calendar, status page and README

**Files:**
- Modify: `internal/web/index.html`, `internal/web/status.html`, `README.md`

**Interfaces:**
- Consumes: Task 3's JSON and routes.

**Calendar (`index.html`):**
- Add a `reminded` Map from session id to `remind_at`. `loadReminders()` fetches `/api/reminders` in the same places `loadWatches()` runs: when it runs on page load, and after a set or cancel. It clears the map for signed-out and unconfirmed users.
- `remindTag(s)` renders `<span class="remindtag">Reminder</span>` beside `watchTag(s)` on both the row and grid tiles.
- `canRemind(s)`: `s.registration?.residents` is set and `new Date(residents) − Date.now() > 135000`.
- The dialog gets a reminder block after the watch block, written in the same style as the watch block:
  - **Signed out**, when `canRemind`: `Sign in to get an email 2 minutes before registration opens.`, with the same sign-in link as the watch note.
  - **Unconfirmed**: `Confirm your email to set reminders.` with the same account link.
  - **Confirmed**: a `Remind me before registration opens` button and the note `We'll email you 2 minutes before residents can register.`
  - **Set**: a banner `✓ Reminder set for <Fri 5:28 p.m.>`, using the page's existing date and time formatters on `remind_at`, and a `Cancel reminder` button.
  - **Errors** show in their own alert line, like `#watchErr`.
- The `.remindtag` and the reminder button's style are visibly different from both the booking button and the watch button. Check this in light and dark mode.

**Status page (`status.html`):**
- Add a "Your reminders" card in the watch section, with columns: session (name, centre, start), registration opens, status, and a Cancel button for pending rows.
  - Status reads `Email at <Fri 5:28 p.m.>` while pending, `Sent` when sent, and `Couldn't send` when `send_error` is set.
  - Empty state: `You have no reminders. Open a session on the calendar to set one.`
- Add an admin-only "All reminders" card with columns: who, session, email time, status.
- Both cards load with the existing 30s watch poll. Add no new timer.

**README:**
- Add a "Registration reminders" section after Spot watching.
- Add rows for the five API routes. There is no config.

- [ ] **Step 1: Implement** the three files.
- [ ] **Step 2: Run** `go test ./internal/web/`. Expect PASS, since the embedded pages still parse and serve.
- [ ] **Step 3: Check in a browser.** Run the binary locally against a temp data dir, with mail going to the log mailer the existing dev setup uses (see `README.md`). Use Playwright to sign up and confirm a user, then:
  - Open a session whose residents time is more than 2m15s away.
  - Set a reminder and check the tag and the banner.
  - Cancel it from the status page.
  - Screenshot the dialog in light and dark mode.
- [ ] **Step 4: Commit** `feat(web): registration reminders on calendar and status page` and `docs: registration reminders`.
