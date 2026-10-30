# Spot watching: email me when a full session opens

## Why

Popular drop-ins (Badminton especially) fill up. Spots reopen when someone cancels, and whoever
books first gets them. The owner wants signed-in people to be able to watch a full session and get
an email **within about a minute** of a spot opening, so they can book it before someone else does.
This is the first feature behind the accounts gate (spec `2026-10-30-accounts-design.md`).

## Goals

- A signed-in, confirmed user can watch any upcoming **full** session and stop watching it.
- The service checks every watched session about **once a minute**. When a watched session goes from
  full to having a spot, each watcher gets one email. If it fills again and reopens later, they get
  another. Watching ends when the session starts.
- The calendar shows which sessions you watch. The status page lists your watches, and the admin sees
  everyone's.

## Non-goals

- Booking for the user (auto-register is a later project).
- Watching sessions that aren't full, or watching a whole activity or time slot.
- SMS, push or any channel other than email.
- An admin UI beyond the read-only "All watches" table.

## Anti-goals

- Hammering the city's PerfectMind site. Checks must stay small and bounded no matter how many
  people sign up.
- Spamming a watcher: no email for a session that just stays open, and no email after it has started.
- Showing anyone's watches or email address to anyone but them and the admin.
- Anything already public changing or needing sign-in.

## Decided by the owner

- Email arrives within about 1 minute of a spot opening.
- Keep watching after an email, and email again on every reopening, until the session starts.
- Status page shows each signed-in user their own watches. Only the admin sees everyone's.
- Admin is `vlmailing@gmail.com`.
- The approved mockups:
  - The session details dialog for a full session shows one of three states:
    - Signed out: "Sign in to get an email when a spot opens".
    - Signed in: an "Email me when a spot opens" button with "We check every minute until the
      session starts".
    - Watching: a "Watching" banner and a "Stop watching" button.
  - A "Watching" tag on the calendar tile.
  - The email has a "Book it now" link and a "Stop watching" link.
  - The status page has "Sessions you're watching" (session, now, last email, Stop) and a
    status line: last check, sessions watched, people. Admins also see "All watches" (who, session,
    now, emails sent).

## Decided by me (change only with reason)

- **Who can watch:** gated with `RequireConfirmed`, the existing one-line gate.
- **What can be watched:** a session that is upcoming (start in the future) and full
  (`spots_left == 0` or spots text "Full") in the stored schedule at the time you press the button.
  Otherwise `409` with a plain message. Once watched, it stays watched even if it opens.
- **Limits:**
  - At most **10 active watches per user**.
  - At most **60 distinct watched sessions** across all users. A new watch on an unwatched session
    beyond that gets `503` "Spot watching is at capacity right now".
  - Both are constants, not config.
- **Which channel the checker uses:** the stored session's first category names its PerfectMind
  calendar. The checker groups active watches by (calendar, day) and fetches each group once per
  cycle with `Classes(cal, dayStart, dayEnd)`, matching rows to watches by `perfectmind.SessionID`.
  It uses its **own `perfectmind.Client`** instance, so it doesn't share state with the full
  scraper. That's about one or two small requests per watched (calendar, day) per minute.
- **Interval:** `WATCH_INTERVAL`, default `1m`, minimum `30s`. A cycle that overruns makes the
  next one start right away, never two at once. With no active watches, a cycle makes no
  requests.
- **Open/closed state** is per session, kept in the database:
  - Fields: `last_spots` (int or null), `last_checked`, `last_error`.
  - On the first check after a watch is created, the session is treated as full.
  - An email is sent to every watcher when a check moves the session from **full or unknown-after-full**
    to `spots_left > 0`. If a check fails or the row is missing from the response, nothing changes.
    The session stays in its last state and the error is recorded.
- **Sessions not found:** if the session is missing from a successful response on 3 checks in a
  row, it's treated as cancelled. Its watches end with no email.
- **Refreshing stored data:** when the checker sees a new spots value, it also updates that session's
  stored `spots`, `spots_left` and `scraped_at`. The calendar then shows fresher numbers for
  watched sessions.
- **End of watching:** when the session's start time passes, its watches are deleted, along with
  any watches whose user was deleted.
- **Email:**
  - Sent through the existing Mailer, in the background.
  - Subject: `A spot opened: <name>, <Sat Oct 31>, <7:00 p.m.>`.
  - The body gives the name, centre, date and time, the spots now and the check time.
  - "Book it now" links to the session's `booking_url`.
  - "Stop watching" is a **one-click link with a per-watch token**: 32 random bytes, only its
    SHA-256 stored. It deletes that watch without needing sign-in and shows a small confirmation
    page.
  - A failed send is logged, not retried.
- **Admin:**
  - The `ADMIN_EMAILS` env var holds a comma-separated list. Entries are normalised like sign-up
    emails.
  - Admin status needs a confirmed account whose email is in the list.
  - Admin gets one extra endpoint, the all-watches list. Nothing else.
- **Calendar data:** `GET /api/watches` (confirmed user) returns the user's watched session IDs with
  state, and the page uses it to tag tiles and pick the dialog state. Signed-out visitors make no
  watch requests.
- **Status page:**
  - The checker line (last cycle time, watched sessions, people watching) is shown to signed-in
    users only.
  - The public `/api/status` JSON doesn't change.
- **Accepted risk carried over from accounts:** someone could pre-register a victim's address and,
  if the victim clicks an unrequested confirm link, sign in as them and create watches that email
  the victim. Several things limit the damage:
  - At most 10 watches.
  - Emails only go out when a spot really opens.
  - Every email has a one-click Stop watching link.
  - The victim can reset their password.

  Accepted for now and recorded. It still has to be fixed before auto-register.

### Settled after the builder check (my calls)

- **Finding the calendar.** The checker looks up PerfectMind calendars once at startup and keeps
  them in memory. It looks them up again only when it meets a category name it doesn't know, and
  at most once every 10 minutes.
- **Which category to check.** A session can sit under several categories, stored in alphabetical
  order. The checker tries each of a session's categories in turn and stops at the first response
  that contains the session.
- **Missing sessions.** A session counts as missing only if every one of its categories returned
  rows and none of them contained it. An error, or an empty response, counts as a failed check:
  no state change, no miss, error recorded.
- **Day range.** The checker fetches with `from` = the start of the day and `to` = the start of the
  next day. It always filters rows by `SessionID`, so any extra day in the response doesn't matter.
- **Spots states.**
  - Only a parsed `spots_left > 0` counts as open, and `0` counts as full.
  - A nil value (such as "Waitlist" or "Registration closed") keeps the previous state and never
    sends an email.
  - An email goes out only on a move from `last_spots == 0` to `> 0`. A new watch starts at
    `last_spots = 0`.
  - Only the checker sends emails. A full scrape that changes the stored spots never triggers one.
- **Stored spots.** The new store method `UpdateSessionSpots` edits the session's stored data in
  place. A later full scrape may overwrite those numbers with its own, which is fine.
- **Request budget, replacing the "60 sessions" cap.** The global cap is **20 distinct
  (calendar, day) groups**, which is about 40 small requests a minute at most.
  - A new watch that would need a 21st group gets `503` "Spot watching is at capacity right now".
  - The checker uses its own `perfectmind.Client` with no delay and runs the groups one after
    another.
- **Stop link.** The link just deletes the watch. A second click says "already stopped".
- **Admin flag.** `/api/auth/me` gains `is_admin`, which the status page uses to decide whether to
  show "All watches".
- **Checker line on the status page.** `GET /api/watches` also returns the checker summary: last
  cycle time, sessions watched and people watching. The public `/api/status` stays unchanged.
- **Cleanup.** Watches for sessions that have started are deleted by the checker itself, not by
  the scraper's `Prune`.

## How it fits

- New tables `watches` (id, user_id, session_id, created, stop_token_hash, emails_sent,
  last_emailed) with a unique (user_id, session_id), and `watch_state` (session_id, last_spots,
  last_checked, misses, last_error).
- New package `internal/watch`: the checker loop, plus HTTP handlers registered through the
  existing auth `Register` style.
- Endpoints:
  - `POST /api/watches` `{session_id}`
  - `POST /api/watches/delete` `{session_id}`
  - `GET /api/watches`
  - `GET /api/watches/all` (admin)
  - `GET /watch/stop?t=…` (page)
  - All POSTs use the existing CSRF check.
- Config: `WATCH_INTERVAL` and `ADMIN_EMAILS`. README and the vNas run command are updated. The
  deploy adds `-e ADMIN_EMAILS=vlmailing@gmail.com`.

## Left to the builder

- Exact schemas, JSON shapes and file layout within the above.
- Email wording beyond the mockup, as plain text plus simple HTML like the accounts emails.
- Tests: the checker gets a fake source and clock. Tests cover:
  - the full→open email
  - no email when the session stays open
  - an email again after it refills and reopens
  - check errors not emailing
  - the miss counter
  - expiry at start
  - limits
  - admin gating
  - stop-token single-use
  - CSRF on the new POSTs
