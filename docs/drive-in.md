# Drive-in

Status: design. Milestone: **Drive-in v1**. Builds on
[the Plex relay](plex-relay.md).

An admin schedules a showing: a movie, an episode, or a run of episodes back
to back, starting at a set time. Viewers open the drive-in page, see what's
on next and a countdown, and at showtime everyone watches the same frame. No
one can pause it. Late arrivals join wherever the showing has got to, like
pulling into a real drive-in.

## Scope

v1 includes:

- One screen (one scheduled room)
- One-off showings
- Movies, single episodes, and runs of consecutive episodes
- An admin page to search Plex, schedule, edit and cancel
- Viewers and admins sign in through streams

Not in v1: recurring schedules, several screens, voting on what to show,
chat (streams has its own).

## Who can do what

Both use the streams login, through the nginx `auth_request` handoff in
[plex-relay.md](plex-relay.md#auth-handoff). The streams auth endpoint
answers with three headers:

| header | meaning |
|---|---|
| `X-Streams-User` | stable user ID |
| `X-Streams-Name` | display name |
| `X-Streams-Admin` | `1` for streams admins (Discord admin role), otherwise absent |

- **Viewers:** anyone signed in to streams can open `/drive-in` and join.
- **Admins:** only `X-Streams-Admin: 1` can reach `/drive-in/admin` and
  the admin API.

## Showings

```json
{
  "id": "sh_7f3k2",
  "title": "Charade",
  "startsAt": 1791327600000,
  "items": [
    {"media": "plex:8812", "title": "Charade", "duration": 6834.2}
  ],
  "intermission": 60,
  "status": "scheduled",
  "createdBy": "123456789",
  "updatedAt": 1791240000000
}
```

| field | notes |
|---|---|
| `startsAt` | UTC ms. Times exist only as UTC on the server |
| `items` | 1 to 12 entries; an episode run is several items in order. Durations are copied from Plex when scheduling, so the timetable is fixed and doesn't depend on Plex being up |
| `intermission` | seconds between items, 0 to 600, default 60. Ignored for a single item |
| `status` | `scheduled` → `live` → `done`, or `cancelled` |
| end time | `startsAt` + sum of durations + intermission × (items − 1). Computed, never stored |

### Storage

- A JSON file at `~/media-sync/showings.json`.
- Writes go to a temp file, then `fsync`, then rename, so a crash never
  leaves a half-written schedule.
- Kept in memory with a mutex; every change is written immediately.
- Showings that ended more than 30 days ago are pruned when the file is
  written.

This needs no database and no dependencies.

## The timeline

The drive-in room is driven by the schedule and the wall clock, not by
commands. For server time `t`:

- **Before the lobby opens** (more than 30 min before the next showing):
  the room is idle and the page shows the next showing.
- **Lobby** (up to 30 min before): poster, title, start time in the
  viewer's local time, and a countdown. Room state is `{paused: false,
  position: 0, at: startsAt}`, the same "hold until `at`" countdown the
  room uses today.
- **Item k playing:** room state is `{media: items[k], position: 0, at:
  startsAt + offset(k)}`, so a late joiner lands at `t − at` into item k.
- **Intermission:** an intermission card with the next item and a
  countdown. The next item's state is broadcast with `at` in the future,
  so clients hold at 0 and load the next source while they wait.
- **After the end:** "That's all, folks" and the next showing, if any.

The server re-works the state from `(schedule, t)` every second and
broadcasts only when it changes. Nothing is remembered between ticks, so
after a restart the server picks up exactly where the showing should be.

### Warm-up

The relay starts the Plex transcode for item k 120 s before it begins (at
the lobby or intermission start) and fetches the first segments, so
showtime doesn't wait on Plex.

## Admin page

`/drive-in/admin` is a single page served by media-sync, for admins only.

**Schedule a showing:**

1. Search the allowed Plex libraries. Results show the poster, title, year
   and runtime.
2. Choose:
   - a **movie**, or
   - a **show** → season → tick a run of episodes. It has to be
     consecutive; gaps are refused, so a run is never accidentally
     scrambled.
3. Pick the date and time with `<input type="datetime-local">` in the
   admin's own local time. The zone is shown next to it, e.g. "EDT
   (America/New_York)". The browser converts it to UTC ms before sending.
4. Preview the runtime, the end time (local) and any clash with another
   showing, before saving.

**Upcoming list:** time (local), title, runtime and status. Each showing
can be edited (title, time, intermission; items only while it's
`scheduled`) or cancelled. Cancelling a live showing asks for confirmation
first, then ends it for everyone.

**Now playing:** current item, position, viewer count, the quality levels
in use, and the Plex session state from the relay.

## Admin API

All JSON, under `/drive-in/api/admin/`:

| method | path | |
|---|---|---|
| GET | `search?q=` | allowed libraries only |
| GET | `shows/{key}/seasons`, `seasons/{key}/episodes` | |
| GET | `showings` | upcoming, live, and done in the last 7 days |
| POST | `showings` | create |
| PATCH | `showings/{id}` | edit |
| POST | `showings/{id}/cancel` | cancel |

## Checks

### Request checks (admin API)

| check | rule |
|---|---|
| admin | `X-Streams-Admin: 1`, trusted only from loopback (nginx), otherwise 403 |
| CSRF | admin calls are authenticated by the streams cookie through nginx, so every write also requires `Origin` to be the streams host and an `X-Requested-With: drive-in` header. A plain cross-site form can send neither |
| body | `Content-Type: application/json`, at most 16 KB, unknown fields rejected |
| rate | 30 writes per minute per admin |

### Showing validation (create and edit)

| check | rule |
|---|---|
| future | `startsAt` at least 2 min from now (warm-up), and no more than 90 days ahead |
| items | 1–12 items; each resolves through the relay's library allowlist; each has a known duration over 60 s |
| run | episodes in a run belong to one show and are consecutive in order (index or air order; gaps refused) |
| clash | the new showing's window, including a 10 min buffer, must not overlap another `scheduled` or `live` showing |
| length | total runtime 12 h or less |
| live edits | a `live` showing can only be cancelled or have its title changed; its time and items are fixed |
| intermission | 0–600 s |

### Runtime checks

- **Source down at showtime:** if an item's media can't be resolved
  during warm-up, admins see an alert on the admin page and viewers see
  "Technical difficulties". The timeline keeps running from the wall
  clock, so when Plex comes back, everyone joins at the right point
  instead of the showing starting late.
- **Schedule file:** unreadable or invalid JSON at startup means the
  server refuses to start. It never silently starts with an empty
  schedule.
- **Clock:** a showing whose `startsAt` is in the past when the server
  starts begins mid-way, which is correct for a drive-in. It is never
  restarted from the top.
- **Watchdog:** if the schedule tick stops, `WATCHDOG=1` stops too, so
  systemd restarts the service. See the deployment section of
  [plex-relay.md](plex-relay.md#deployment).

### Tests

- **Timeline:** table-driven tests of `(schedule, t)` → state across the
  lobby, each item, intermissions, the end, and restarts mid-item, with a
  fake clock.
- **Validation:** one test per rule above, including DST: 2026-11-01
  01:30 occurs twice in America/New_York, and the UTC the browser sends is
  what gets stored.
- **Storage:** a write is killed mid-way, then reloaded, and the previous
  schedule is intact. A corrupt file makes startup fail.
- **Auth:** non-admin, missing header, wrong `Origin` and missing
  `X-Requested-With` each get 403. A request arriving directly, not from
  loopback, is refused.

### Acceptance

| # | check | pass |
|---|---|---|
| D1 | Schedule a movie 5 min out from the admin page, in local time | viewers in 2 time zones see the right local start; playback begins within 1 s of showtime |
| D2 | Join 20 min late | lands within 0.5 s of the room position |
| D3 | 3-episode run, 60 s intermission | intermission cards show; the next episode starts on time with no manual action |
| D4 | Restart media-sync during episode 2 | viewers resume episode 2 at the right point |
| D5 | Non-admin opens `/drive-in/admin` and calls the API | 403 on both |
| D6 | Overlapping showing | refused, with the clash named |
| D7 | Stop Plex 1 min before showtime, start it 3 min after | "Technical difficulties", then everyone joins at minute 2 |

## Streams changes (planned, not done yet)

All three are small, and they are the whole streams side of this:

1. **Auth endpoint:** `GET /internal/auth/media-sync`. It reads the
   streams session and answers 204 with `X-Streams-User`,
   `X-Streams-Name` and `X-Streams-Admin`, or 401. It is called only by
   nginx `auth_request` and is marked `internal` in nginx, so browsers
   can't call it.
2. **nginx:**
   - `/drive-in`, `/drive-in/api/` and `/sync/ws` go through
     `auth_request` and then to media-sync, with `auth_request_set` for
     the three headers. Client-sent copies are cleared.
   - `/media/` goes to media-sync without `auth_request`; the grant is
     the authorization.
   - Add tests in `tests/test_nginx_auth.py`.
3. **Nav link** to "Drive-in" in the streams shell.

Streams keeps its own `/sync` Socket.IO namespace until the watch page
moves over to `sync.js`. That move is a later, separate change.

## What's needed to build it

| need | from |
|---|---|
| Plex token, written to `~/media-sync/plex-token` (mode 600) on the box by you | you |
| Which Plex libraries the drive-in may show (listed from the server once the token is there) | you |
| Running the sudo steps once: systemd unit and nginx locations (a script, provided for review) | you |
| Go-ahead on the three streams changes above, when it's time | you |
| Everything else: relay, drive-in, admin page, tests, deploy script | me |

## Build order

1. Plex relay steps 1–4 ([plex-relay.md](plex-relay.md#build-order)).
   The #4 spike runs on the box against the real token.
2. Schedule model, storage and the timeline function, with fake-clock
   tests.
3. The drive-in room driven by the schedule, plus warm-up.
4. Admin API and checks, then the admin page.
5. Viewer page: lobby, intermission card, "Technical difficulties", end
   card.
6. Deploy script and sudo script for review. Streams changes once
   approved.
7. Acceptance D1–D7.
