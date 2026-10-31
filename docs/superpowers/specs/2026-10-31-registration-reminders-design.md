# Registration reminders: email me 2 minutes before registration opens

## Why

Popular drop-ins (Badminton especially) fill within minutes of Residents registration opening, which
is 21 hours before the session for most drop-ins. Spot watching (spec `2026-10-30-spot-watch-design.md`)
helps after a session fills; this helps before. The owner first asked for automatic booking, then
dropped it in favour of a timely reminder that they act on themselves.

## Goals

- A signed-in, confirmed user can set a reminder on any session whose Residents registration time is
  known and still in the future, and cancel it.
- The user gets one email **2 minutes before** that session's Residents registration opens, with a
  link to the session's booking page.
- The calendar shows which sessions you have reminders on. The status page lists your reminders, and
  the admin sees everyone's.

## Non-goals

- Booking for the user, or storing Markham credentials (dropped by the owner on 2026-10-31).
- Reminders for Members or Public registration times.
- Choosing the lead time per reminder, or reminding for a whole activity or time slot.
- Any channel other than email.

## Anti-goals

- Any extra requests to the city's PerfectMind site. Reminders use times already in the stored
  schedule.
- A late or duplicate email. A reminder that can no longer arrive before registration opens is not
  sent.
- Showing anyone's reminders or email address to anyone but them and the admin.
- Anything already public changing or needing sign-in.

## Decided by the owner

- Lead time: 2 minutes before Residents registration opens. The email includes the booking link.
- Anyone signed in with a confirmed email can use it (same gate as spot watching).
- Approved in chat (owner said "go"):
  - Session dialog: a **Remind me before registration opens** button when Residents registration
    hasn't opened yet. Once set: "Reminder set for <Fri 5:28 p.m.>" and a **Cancel reminder** button.
  - Email subject like `Registration opens in 2 minutes: <name>, <Sat Oct 31>, <2:30 p.m.>`, with a
    **Book it** link and a link to cancel the reminder. One email per reminder, then it's done.
  - A "Reminder" tag on the calendar tile. Status page: "Your reminders" next to your watches; the
    admin also sees everyone's.
  - Limit of 10 reminders per person.
  - If a later scrape moves the registration time, the reminder follows it. If the session
    disappears, the reminder is dropped with no email.

## Decided by me (change only with reason)

- **Reminders and watches are independent.** A session can have both; neither affects the other.
- **What can get a reminder:** a stored session with `registration.residents` set and more than
  2 minutes in the future at the time you press the button. Otherwise `409` with a plain message
  ("Registration has already opened" / "Online registration time isn't known for this session").
  The dialog only shows the button when this holds.
- **Fire time is read from the stored session at send time**, not copied at creation. That is how a
  moved registration time is followed with no extra bookkeeping.
- **Sender loop:** a goroutine in the existing `internal/watch` package's process (or a sibling
  package, builder's choice) ticks every **15 seconds**. Each tick it loads pending reminders, reads
  each session's current Residents time, and sends those whose fire time (residents − 2 min) has
  passed.
  - If registration has already opened when the tick sees it (e.g. the service was down), the
    reminder is dropped with no email (anti-goal: no late email). Logged.
  - If the session is gone from the store or has lost its Residents time, the reminder is dropped,
    no email.
  - The reminder is marked sent **before** the email goes out, so a crash can't send twice.
  - A failed send is logged, not retried (same as spot watching).
- **Cancel link** works like the spot-watch Stop link: a per-email token (32 random bytes, SHA-256
  stored), GET shows a confirm page, POST (CSRF-checked) deletes. Since a reminder is single-use the
  link mainly matters if the email is early; it is still included because the owner asked for it.
- **Limits:** 10 pending reminders per user (constant, not config). No global cap: reminders cost no
  outside requests.
- **Cleanup:** sent reminders are kept for 7 days for the status page ("sent") then purged; deleting
  a user removes their reminders and tokens.
- **Admin:** reuses `ADMIN_EMAILS` / `IsAdmin`. One extra admin endpoint lists everyone's reminders.

### Settled after the builder check (my calls; these win over anything above)

- **A briefly missing session doesn't kill a reminder.** Each reminder stores `residents_at`, the
  last Residents time seen; it is set at creation and refreshed whenever a tick finds the session.
  If the session is missing from the store on a tick, the reminder is skipped that tick, not
  dropped. It is dropped (no email) once its stored `residents_at` has passed. A missing session is
  never emailed, because its time can't be confirmed.
- **Session IDs** hash the event, date, start time and facility. A moved registration time keeps
  the ID and the reminder follows it. A moved *start* time makes a new ID, so the old reminder
  lapses as "gone". That's accepted.
- **Timing boundaries.**
  - `POST` needs Residents registration to be more than 2 minutes 15 seconds away, i.e. 2 minutes
    plus one tick, so every accepted reminder gets at least one tick inside its window.
  - The sender sends from `residents − 2m` up to `residents`. At or after `residents` it drops the
    reminder without sending.
- **Failed sends.** The reminder is still marked sent before sending (no double send). A failed
  send records `send_error`. The status page shows "Sent" or "Couldn't send". Failed sends are not
  retried.
- **Booking link.** The email uses `booking_url`. If that's empty, the email leaves out the
  button and points to Markham's registration site
  (`https://cityofmarkham.perfectmind.com`).
- **Address.** The user's current email is read at send time.
- **Calendar tag refresh.** `index.html` loads `/api/reminders` together with `/api/watches` and
  reloads both after a set or cancel. There's no new polling.

## How it fits

- New table `reminders` (id, user_id, session_id, created, sent_at nullable) with unique
  (user_id, session_id), plus `reminder_cancel_tokens` (hash, reminder_id).
- Endpoints, all behind `RequireConfirmed` except the cancel page, POSTs CSRF-checked:
  - `GET /api/reminders` → the user's reminders with session name, start and fire time
  - `POST /api/reminders` `{session_id}` → 201, or 200 if already set
  - `POST /api/reminders/delete` `{session_id}` → 204
  - `GET /api/reminders/all` (admin)
  - `GET/POST /reminder/cancel?t=…`
- UI: `index.html` dialog and tile tag; `status.html` "Your reminders" card and admin "All
  reminders" card, refreshed with the existing 30s watch poll.
- No new config. README gets a "Registration reminders" section and API rows.

## Left to the builder

- Exact schemas, JSON shapes, file layout, and whether the sender shares the watch checker's loop.
- Email wording beyond the subject, as plain text plus simple HTML like the spot-watch emails.
- Tests with a fake clock and mailer covering:
  - sends once at residents − 2 min, not before
  - follows a moved registration time
  - drops with no email when registration has already opened or the session is gone
  - no double send after a restart mid-tick
  - the 10 limit, 409 cases, admin gating, cancel-token single use, CSRF on the POSTs
