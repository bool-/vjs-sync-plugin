import {sync} from '/client/sync.js';

const room = new URLSearchParams(location.search).get('room') || 'party';
const video = document.getElementById('video');
const $ = (id) => document.getElementById(id);

for (const link of $('rooms').querySelectorAll('a')) {
  if (new URL(link.href).searchParams.get('room') === room) {
    link.setAttribute('aria-current', 'page');
  }
}

const url = `${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/ws`;
const player = sync(video, {url, room});

// Handy from the console: mediaSync.stats()
window.mediaSync = player;

// Party rooms get the native controls. A scheduled room is a drive-in:
// no play, pause or scrubbing, just sound and fullscreen.
player.addEventListener('state', ({detail: {state}}) => {
  video.controls = !state.locked;
  $('locked-bar').hidden = !state.locked;
  $('blurb').textContent = state.locked ?
    'Showings run on a timetable. Everyone sees the same frame; nobody can pause it.' :
    'Open this page in two tabs. Play, pause and seek in either one; the other follows.';
});

player.addEventListener('blocked', () => {
  $('blocked').hidden = false;
});

$('join').addEventListener('click', () => {
  $('blocked').hidden = true;
  video.play();
});

$('mute').addEventListener('click', () => {
  video.muted = !video.muted;
  $('mute').textContent = video.muted ? 'Unmute' : 'Mute';
});

$('fullscreen').addEventListener('click', () => video.requestFullscreen());

const clock = (seconds) => {
  const s = Math.max(0, Math.round(seconds));

  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
};

const fixed = (n, digits, unit = '') => (n === null ? '–' : `${n.toFixed(digits)}${unit}`);

setInterval(() => {
  const s = player.stats();

  $('s-room').textContent = room;
  if (s.state) {
    const showing = s.state.locked && s.startsIn > 0;
    const buffering = !s.state.locked && s.state.waiting;

    $('s-state').textContent = showing ? 'waiting for showtime' :
      buffering ? 'waiting for everyone to buffer' :
        s.state.paused ? 'paused' : 'playing';
    // Hold the overlay back for a beat so a quick sync does not flash it.
    $('countdown').hidden = !showing && !(buffering && s.startsIn < 4.5);
    $('countdown-label').textContent = showing ? 'Next showing in' : 'Waiting for everyone to buffer…';
    $('countdown-time').textContent = showing ? clock(s.startsIn) : '';
  }
  $('s-expected').textContent = s.expected === null ? '–' : clock(s.expected);
  $('s-drift').textContent = fixed(s.drift === null ? null : s.drift * 1000, 0, ' ms');
  $('s-rate').textContent = fixed(s.rate, 2, '×');
  $('s-clock').textContent = s.offset === null ? '–' : `${fixed(s.offset, 0, ' ms')} / ${fixed(s.rtt, 0, ' ms')}`;
}, 250);
