# media-sync

Keep video players in step across browsers: a watch party where anyone can
play, pause and seek, or a drive-in where a showing runs on a timetable and
nobody can.

- **Client**: one ES module set, no dependencies, no build. Drives any
  `<video>` element. HLS plays natively where the browser can; elsewhere
  hls.js is imported on first use only.
- **Server**: one Go binary, standard library only (about 6 MB, a few MB of
  RAM). Serves the example and the client too.

## How it works

The server owns each room's timeline: `{paused, position, at}` means the
media is at `position` seconds at server time `at`, advancing in real time
from then unless paused, and holding at `position` until `at` arrives.

- **Commands, not reports.** Players send play, pause and seek. They never
  report their own position as truth, so a viewer who is buffering cannot
  drag everyone else back.
- **Everyone buffers, then everyone starts.** After a play or seek the room
  waits while each player seeks to the spot and reports `ready`. When the
  last one is ready the server sets `at` 300 ms out and every player starts
  on a timer at that moment. A player that is not ready within 5 seconds is
  not waited for; it catches up on its own.
- **Clock sync.** Ping/pong round trips; the fastest one sets the offset to
  the server's clock.
- **Smooth correction.** Drift over 40 ms is absorbed by nudging the playback
  rate (at most ±5%, pitch preserved), down to 15 ms. Drift over 0.5 s gets a
  hard seek aimed ahead by this player's measured seek time.
- **Start-up lead.** Each player measures how long `play()` takes to get
  frames moving and starts that much early.
- **Scheduled rooms** are locked: commands are refused, and the showing is
  announced one countdown ahead of each start.

In a two-browser test against the local server, players stay within about
30 ms of each other, land within a few ms of each other after a seek, and
pause on the same frame.

## Run the example

```sh
cd server && go build -o ../sync-server . && cd ..
./sync-server            # http://localhost:8080
```

Open `/?room=party` in two tabs and drive either one. `/?room=drive-in` is
the scheduled room. Flags:

| flag | default | |
|---|---|---|
| `-addr` | `:8080` | listen address |
| `-src` | oceans.mp4 sample | media URL every room plays (MP4 or `.m3u8`) |
| `-type` | | MIME type, if the URL does not make it obvious |
| `-scheduled-room` | `drive-in` | the timetabled room; empty for none |
| `-show-every` | `75s` | time between showings |
| `-countdown` | `20s` | how long before a showing it is announced |
| `-max-rooms` | `1000` | most rooms at once |
| `-web` | `.` | repository root to serve `example/` and `client/` from |

For the VPS: `GOOS=linux GOARCH=amd64 go build -ldflags="-s -w"`, copy the
binary, and put it behind the TLS proxy so browsers connect over `wss://`.

## Use the client

```js
import {sync} from './client/sync.js';

const player = sync(document.querySelector('video'), {
  url: 'wss://example.com/ws',
  room: 'movie-night'
});

player.addEventListener('blocked', () => showClickToJoin()); // autoplay refused
player.addEventListener('state', ({detail}) => render(detail.state));
player.stats(); // {offset, rtt, drift, rate, expected, startsIn, ...}
player.destroy();
```

Options (defaults in `client/sync.js`): `room`, `seekThreshold` (0.5 s),
`driftTolerance` (0.04 s), `settleTolerance` (0.015 s), `maxRateAdjust`
(0.05), `rateGain`, `tickInterval`, `pingInterval`, `seekDebounce`,
`hlsUrl`, `transport` (bring your own wire: see `client/transport.js`), and
`loadSource`.

When a player library owns the element (video.js, for instance), let it load
sources and sync the element underneath:

```js
sync(player.tech().el(), {url, room, loadSource: async(media, src) => {
  player.src(src);
  return () => {};
}});
```

## Protocol

JSON text frames on `/ws`.

| from | message |
|---|---|
| client | `{type: 'join', room}` (`[a-z0-9-]{1,32}`) |
| client | `{type: 'ping', t0}` → `{type: 'pong', t0, t}` |
| client | `{type: 'command', action: 'play'\|'pause'\|'seek', position}` |
| client | `{type: 'ready', rev}` |
| server | `{type: 'state', room, rev, paused, position, at, locked, waiting, src}` |
| server | `{type: 'error', message}` |

The server also refuses cross-origin sockets, caps messages at 1 KB, rate
limits each connection, drops clients that cannot keep up, and removes party
rooms when the last viewer leaves.

## Test

```sh
npm test              # client, Node's built-in runner
cd server && go test ./...
```

## License

MIT. Copyright (c) Anthony Snavely
