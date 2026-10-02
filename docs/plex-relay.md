# Plex relay

Status: design. Milestone: **Plex relay** (#3–#9).

Rooms play media from a Plex server. The sync server fetches it from Plex
once and serves it to every viewer in the room. Viewers never see a Plex
URL, a Plex hostname or the Plex token, and they can't make the relay call
anything on Plex except what this document allows.

## Goals

- One Plex transcode per room per quality level, however many viewers.
  Plex's upload is one stream per active level.
- Quality levels: 480p (1.5 Mbps), 720p (4 Mbps), 1080p (8 Mbps). Viewers
  in one room can watch at different levels and stay in sync.
- The owner token is used, held only by the server and contained by design:
  the token is sent only as a header, viewers can't supply a Plex path,
  responses are built by the relay rather than copied from Plex, and the
  libraries viewers can reach are allowlisted.
- Seeks stay fast: the relay fetches the first segments at the new position
  while the room waits for everyone to buffer.

Not goals: transcoding on the droplet; Plex accounts for viewers; Plex's own
watch-together features.

## Shape

```
browser ──ws──────────▶ sync server ── rooms, clocks, ready barrier (today)
   │                         │
   │ /media/{grant}/...      │ owns: resolver, Plex client, sessions, cache
   └────────────────────────▶ relay ──HTTPS, X-Plex-Token header──▶ Plex
                                                                  (Remote Access,
                                                                   *.plex.direct)
```

New Go packages under `server/`:

| package | job |
|---|---|
| `source` | `Source` interface and resolver; `url:` and `plex:` sources (#3) |
| `plex` | the only code that talks to Plex: a fixed set of typed calls, token as a header |
| `relay` | grants, path parsing, transcode sessions per (room, level), segment cache, playlists built by the relay |

## Media IDs and loading

Room state carries `media` instead of `src`:

```json
{"type": "state", "rev": 9, "media": {"id": "plex:8812", "title": "Charade", "duration": 6834.2, "levels": [480, 720, 1080]}, ...}
```

- `plex:<ratingKey>` is the only Plex form; the ratingKey must be digits.
- A party room loads media with `{type: "command", action: "load", media: "plex:8812"}`.
  The drive-in's schedule names its media in config.
- Before loading, the resolver fetches the item's metadata and **refuses
  anything outside `-plex-libraries`** (allowed library section IDs). That
  allowlist does most of what a restricted user would, without an extra
  account: viewers can only browse, see or load items from those libraries.
- `levels` lists only levels at or below the source's height. A 720p file
  never offers 1080p.

## Grants: how a viewer gets media URLs

The relay never serves media to someone who isn't in the room.

- On join, and again on every reconnect, the server sends that connection
  its own grant:
  `{type: "media", base: "/media/<grant>/"}`
- `grant = base64url(room | connID | expiry) + "." + HMAC-SHA256(key, same)`.
  The key is random per server process and never leaves memory, so a
  restart invalidates every grant and viewers get new ones when they
  reconnect.
- Expiry is 6 h; grants are refreshed while connected.
- When a connection leaves the room, its grant stops working at once: the
  relay checks the connection is still in the room on every request.

## Paths viewers can request

This is the whole surface. Anything else gets **404 with an empty body**.

```
GET|HEAD /media/{grant}/master.m3u8
GET|HEAD /media/{grant}/{level}/index.m3u8
GET|HEAD /media/{grant}/{level}/{seq}.ts        seq: 0–99999
GET|HEAD /media/{grant}/poster.jpg
GET      /api/search?q=...                      (needs a grant header; allowed libraries only)
```

- The paths are parsed by hand against this exact grammar. No
  `filepath.Clean`, no prefix matching, no catch-all.
- `level` must be one of the room's levels; `seq` must be in the current
  playlist.
- The relay builds Plex URLs **only** from its own state: session IDs it
  created and segment URIs it read from Plex's playlist. No string a viewer
  sends ever reaches a Plex URL. This prevents path traversal by design
  instead of by filtering.

## The Plex client (`plex` package)

These are the only calls the server makes to Plex:

| call | used for |
|---|---|
| `GET /identity` | preflight: reachable, and the server matches `-plex-server-id` |
| `GET /:/prefs` | preflight: WAN bitrate settings |
| `GET /library/metadata/{key}` | resolve: duration, height, codecs, library section, poster |
| `GET /hubs/search` | search, filtered to allowed libraries |
| `GET /video/:/transcode/universal/start.m3u8` | start a level's session |
| `GET` the playlist and segment URIs from that response | relay content |
| `GET /video/:/transcode/universal/ping` | keep a session alive |
| `GET /video/:/transcode/universal/stop` | end a session |
| `GET /photo/:/transcode` | poster, resized on Plex |

Rules the code must follow:

- The token goes **only** in the `X-Plex-Token` request header. Never in a
  URL, so it cannot turn up in Go error strings, redirects or logs.
- `CheckRedirect` refuses every redirect.
- Connect timeout 5 s, metadata calls 10 s, segments 20 s.
- Errors from this package go to the server log with the URL host and path
  but no query string, and **never** to a viewer. Viewers get `502` and a
  request ID.

## Transcode sessions

There is one session per (room, level), each with its own
`X-Plex-Client-Identifier` and session ID.

```
          first request for this level
idle ───────────────────────────────▶ starting ──playlist ok──▶ live
  ▲                                       │                      │ │
  │ idle 60s / room empty / unload        │ fail                 │ │ seek
  └────────────── stopping ◀──────────────┘◀─────────────────────┘ │
                     ▲                                             ▼
                     └──────────── restarting (new offset) ◀───────┘
```

- **Start:** `start.m3u8` with `protocol=hls`, `directPlay=0`,
  `directStream=1`, `videoResolution` and `maxVideoBitrate` set by the level,
  and `offset` set to the room position.
- **Keepalive:** ping every 20 s while anyone in the room uses the level.
- **Stop:** after 60 s with no requests, when the room empties, or when the
  room loads other media. Stop calls are best-effort and retried once.
- **Caps:** at most 3 sessions per room and 6 across the server (`-plex-max-sessions`).
  Beyond that, new levels are refused and the client stays on a level that
  is already running.
- **Seek or play command:**
  1. The room enters `waiting` as it does today.
  2. In the background, every live level restarts at the new offset and
     fetches its first 3 segments (prefetch).
  3. Viewer requests that arrive first wait on those same fetches, so
     there is no duplicate upstream call.
  4. The existing ready barrier and 5 s cap stay as they are.

### Timeline (confirmed in #4)

Plex serves a **full-length VOD playlist with on-demand segments**: an
83-minute film gives 997 × 5 s segments plus `#EXT-X-ENDLIST`, and a
segment far ahead is transcoded when requested. Segment timestamps are
movie time plus a constant 10 s, which hls.js normalises. So:

- The relay re-addresses Plex's own playlist, with no synthetic timeline.
  A forward seek is just a segment request.
- A session starts at `offset` and returns **empty 188-byte segments for
  anything before it**. A request below the session's first produced
  segment means the relay restarts that level at the requested offset.
  Undersized segments are a miss: retried, never cached.
- Segment length differs by level: 480p 8 s, 720p 5 s, 1080p 1 s. The
  levels never align, so hls.js automatic level switching stays off.

## Cache

- An in-memory LRU of segments keyed by (room, level, seq), with a byte
  budget (`-cache-mb`, default 512). Playlists are generated by the relay
  and never cached as upstream bytes.
- Fetches are deduplicated: one upstream request per segment, however many
  viewers ask at the same moment.
- A room's entries are dropped when its media changes or it closes.

## Client

- `sync.js` is unchanged except that `src` is replaced by `media` and the
  `media` grant message.
- hls.js loads `{base}master.m3u8` with `autoLevelEnabled = false`.
- **Choosing a level on join:**
  1. A choice saved in `localStorage` wins.
  2. Otherwise pick the highest level with a 1.5× margin over the bandwidth
     estimate (`navigator.connection.downlink`, if available).
  3. Otherwise 720p.
- A menu lists the room's levels. A change takes effect at once in hls.js;
  movie time is the same at every level, so sync just carries on.
- After 3 stalls in 60 s, offer "drop to {lower}?". Never switch silently.

## Configuration

| flag | default | |
|---|---|---|
| `-plex-url` | | Remote Access HTTPS URL (`https://*.plex.direct:32400`) |
| `-plex-token-file` | | file with the token; must be mode 0600 and not world-readable |
| `-plex-server-id` | | expected `machineIdentifier`, so the token is never sent to a different server |
| `-plex-libraries` | | comma-separated library section IDs viewers may use |
| `-levels` | `480:1500,720:4000,1080:8000` | height:kbps |
| `-plex-max-sessions` | `6` | Plex transcodes across all rooms |
| `-listen` | `127.0.0.1:8090` | loopback only; nginx is the way in |
| `-cache-mb` | `512` | segment cache budget (128 on the streams box) |

There is deliberately no token flag and no environment variable: a flag
would show up in `ps`, and an environment variable leaks into crash dumps
and child processes.

## Checks

### 1. Preflight (the server refuses to start)

| check | fails when |
|---|---|
| token file | missing, empty, or readable by group or others |
| HTTPS | the Plex URL is not `https`, or the certificate does not verify |
| server pin | `/identity` `machineIdentifier` ≠ `-plex-server-id`, so a token is never sent to a server that changed hands |
| token valid | the authenticated `/library/sections` call is not 200 |
| libraries | an ID in `-plex-libraries` doesn't exist on the server |
| levels | `-levels` doesn't parse, or isn't strictly ascending |

### 2. Preflight (warnings: the server starts and logs them)

| check | warns when |
|---|---|
| per-stream cap | the remote stream bitrate limit pref is below the top level's bitrate (pref ID to verify in #4) |
| total upload | the WAN total upload pref is below the sum of all levels |
| PMS version | older than the fix for CVE-2025-69414 (1.42.2.10156) |
| entitlement | a 1 s test transcode of an allowed item is refused (remote playback needs Plex Pass/Remote Watch Pass) |

### 3. On every request

- Method is `GET` or `HEAD`. The path matches the grammar exactly.
- Grant: HMAC compared in constant time, not expired, room exists, and the
  connection is still in it.
- `level` is in the room's levels. `seq` is in range for the current
  playlist.
- Per-grant rate limit: 50 requests/s with a burst of 100.
- **Response headers are built from scratch**: `Content-Type`,
  `Content-Length`, `Cache-Control: no-store` for playlists and
  `private, max-age=3600` for segments, plus `X-Request-Id`. Nothing from
  Plex is copied: no `Set-Cookie`, `Location` or `X-Plex-*`.
- Segment bodies: the upstream content type must be video, and the size is
  capped at 16 MB, or the response is a 502.
- Playlist bodies are written by the relay from parsed data. Upstream
  playlist bytes are never forwarded.

### 4. Runtime

- Plex health: `/identity` every 60 s. Three failures in a row mark the
  source down. Rooms get `{type: "error", message: "source unavailable"}`
  and loads are refused until Plex recovers.
- Session watchdog: any session in `starting` or `restarting` for more
  than 30 s is stopped and retried once, then reported.
- Orphan sweep every 5 min: compare Plex's `/transcode/sessions` with the
  relay's own and stop any session the relay started (identified by its
  client-ID prefix) that it no longer tracks.
- `GET /healthz`, on localhost only: Plex status, sessions per room, cache
  bytes, upstream error counts. It never includes the token or any Plex
  URL.

### 5. Leak tests (CI, blocking)

- **Hostile fake Plex.** An `httptest` Plex that puts the token in
  everything it can: playlist URIs, error bodies, `Location`, `Set-Cookie`,
  custom headers, metadata JSON. Drive every viewer path, and every error
  path (timeouts, 500s, oversized bodies), through the relay. Assert the
  token's bytes appear in **no** response body or header.
- **Header only.** The fake fails the test if the token ever arrives in a
  query string, or if any request goes to a path outside the call table.
- **No outbound path from viewer input.** A fuzz test (`go test -fuzz`)
  feeds the path parser arbitrary strings, including `..`, `%2e%2e`,
  `//`, `\`, NUL, overlong UTF-8 and huge numbers. It asserts that every
  accepted path round-trips to the grammar, and that the upstream URL is
  always one the relay built.
- **Source rules** (a Go test that reads the `relay` and `plex` source):
  - `X-Plex-Token=` never appears in non-test code.
  - Nothing writes `err.Error()` to an `http.ResponseWriter`.
  - `httputil.ReverseProxy` is never used, because forwarding raw
    responses is exactly what this design avoids.
- **Library allowlist.** Loading or searching an item from another library
  returns "not found", identical to an item that doesn't exist.
- **Grants.** Tampered, expired, other-room and departed-connection grants
  all get 404.

### 6. Acceptance (real Plex, before the milestone closes)

| # | check | pass |
|---|---|---|
| A1 | Two viewers, one at 720p and one at 1080p, in one room | within 50 ms of each other after 10 s |
| A2 | Seek to a cold position | room starts within 3 s at the median, 6 s worst over 10 seeks |
| A3 | Plex dashboard during A1 with 5 viewers | exactly 2 transcodes |
| A4 | Everyone leaves | Plex shows 0 relay sessions within 90 s |
| A5 | Browser HAR of a full session, searched for the token and the `plex.direct` host | 0 matches |
| A6 | Chrome throttled to 3 Mbps on join | starts at 720p or lower, no stalls in 2 min |
| A7 | Kill Plex in the middle of a movie | "source unavailable" within 3 min, no hung requests, recovers on its own |
| A8 | Restart the sync server mid-movie | viewers reconnect, get new grants, resume in sync |

## Deployment

**Decision: a separate Go service on the streams box, behind the streams
nginx vhost on the same domain. Not built into the streams Flask app.**

Why not build it into streams:

- Streams runs **exactly one** gunicorn worker, and the same process owns
  the ffmpeg scheduler (`deploy/streams.service`). Its systemd watchdog
  restarts the whole app when a job stalls. Putting a media relay in that
  process (blocking Plex fetches, a segment cache, hundreds of requests a
  minute during a movie) adds load to the one process whose hang already
  costs a restart. That restart would drop every IPTV channel and every
  watch party at once.
- This server is already written, tested and stdlib-only. Rewriting it in
  Python would trade a 6 MB binary for the GIL.
- Kept separate, a crash in either service leaves the other running.

Why not a separate domain: on the same origin, streams' login protects the
socket with no CORS and no second sign-in, and its existing nginx
`auth_request` pattern can be reused.

```
streams.fucking.lol (nginx, TLS)
├─ /                → streams (Flask, 127.0.0.1:5000)          unchanged
├─ /channels/       → nginx secure_link from disk               unchanged
├─ /sync/ws         → auth_request → streams says who the user is,
│                     then media-sync (Go, 127.0.0.1:8090)
└─ /media/          → media-sync. No auth_request; the grant in the path
                      is the authorization, so Chromecast keeps working
                      the same way streams' signed segments do.
```

### Auth handoff

- nginx runs `auth_request` against a small streams endpoint once per
  WebSocket connection, not per segment. The endpoint answers 204 with
  `X-Streams-User: <id>`, `X-Streams-Name: <display name>` and, for streams
  admins, `X-Streams-Admin: 1`, or 401. The drive-in admin page uses the
  admin flag ([drive-in.md](drive-in.md#who-can-do-what)).
- nginx forwards those headers with `auth_request_set` and **overwrites
  any client-sent copies**.
- media-sync binds `127.0.0.1` only. It trusts `X-Streams-*` only from
  loopback, and refuses a WebSocket without it.
- After that, media requests carry the per-connection grant described
  above. No cookies are involved, which is what lets Chromecast and
  segment fetches work.

### On the box

| thing | location |
|---|---|
| binary | `/home/bool/media-sync/media-sync` (built with `GOOS=linux GOARCH=amd64 go build -ldflags="-s -w"`, copied with scp) |
| Plex token | `/home/bool/media-sync/plex-token`, mode 0600 (preflight refuses anything wider) |
| unit | `/etc/systemd/system/media-sync.service`, committed in this repo as `deploy/media-sync.service` |
| nginx | two `location` blocks added to the streams vhost, with a matching test in streams' `tests/test_nginx_auth.py` |

Unit settings, following streams' conventions:

- `User=bool`, `Restart=always`, `StartLimitBurst=5` within 300 s.
- `Type=notify` with `WatchdogSec=30`. sd_notify is about 15 lines of
  stdlib over the `NOTIFY_SOCKET` datagram socket. The service sends
  `WATCHDOG=1` only while its accept loop and the Plex health check are
  both making progress.
- `MemoryMax=320M`. The host has 2 GB and streams uses about 680 MB, so
  set `-cache-mb 128` here and not the 512 default.

The unit and nginx changes need sudo once; after that, deploying means
copying the binary and running `systemctl restart media-sync`.

### Repository

When the streams work starts, this repo merges into the streams repo as
`media-sync/` (Go module, `client/`, docs), keeping its history, and
`vjs-sync-plugin` is archived with a pointer to the new location.

- Protocol changes and the streams pages that use them then ship in one
  PR, and streams' tests can run the real binary.
- On the box, deploying becomes `git pull`, `go build` in `media-sync/`,
  then `systemctl restart media-sync`.
- It stays a separate process with its own unit. The isolation comes
  from running two processes, not from having two repos.

### Watch out for

- **Egress is shared.** IPTV already leaves from this droplet. Check the
  droplet's monthly transfer graph before and after the first movie
  nights. One 10-viewer, 2-hour session is about 45 GB.
- **Streams' own `/sync` Socket.IO namespace** stays untouched until
  media-sync is live. After that, streams' watch page loads `sync.js` with
  the video.js `loadSource` adapter and the old namespace is removed.
  That is a separate streams change, done later.

## Plex facts (from #4)

- PMS 1.43.4, reachable at `https://…plex.direct:11226`; port 32400 is
  closed.
- The owner has lifetime Plex Pass, so remote playback through the relay
  is covered.
- `WanPerStreamMaxUploadRate`, `WanTotalMaxUploadRate` and
  `WanPerUserStreamCount` are all 0 (unlimited). These are the IDs the
  preflight reads.
- Three sessions per token in parallel work. Cold seeks with all three
  levels running took 2.2–5.7 s, so prefetch matters.
- Idle sessions are not reaped for at least 90 s; the relay stops them
  itself.
- The default profile transcodes audio to MP3. Request AAC through
  `X-Plex-Client-Profile-Extra`.
- Allowed libraries: 1 (Movies) and 2 (TV Shows). The 4K libraries are
  excluded.

## Build order

1. #3: media IDs and the `Source` interface, plus a `url:` source and the
   `media` grant message.
2. The `plex` package and the preflight checks, against the hostile fake.
3. ~~#4 spike~~: done. See Plex facts.
4. #5: relay path, grants, single-flight cache, leak tests.
5. #6: sessions per level, keepalive and idle stop, master playlist, client
   level choice.
6. #7: prefetch on seek.
7. #8: Direct Play and Direct Stream, using this same relay and these same
   checks.
8. #9: the deployment above (unit, nginx, auth handoff) and the acceptance table.
