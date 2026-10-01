import {test, beforeEach, afterEach, mock} from 'node:test';
import assert from 'node:assert/strict';
import {sync} from '../client/sync.js';
import {FakeMedia, fakeTransport, flush} from './helpers.js';

const T0 = 1_000_000;
let syncs = [];

beforeEach(() => {
  mock.timers.enable({apis: ['setInterval', 'setTimeout', 'Date'], now: T0});
});

afterEach(() => {
  syncs.forEach((s) => s.destroy());
  syncs = [];
  mock.timers.reset();
});

// A synced fake player whose clock already matches the server's.
const setup = ({room = 'party', transport = fakeTransport()} = {}) => {
  const media = new FakeMedia();
  const loaded = [];
  const s = sync(media, {
    room,
    transport,
    loadSource: async (m, src) => {
      loaded.push(src);
      return () => {};
    }
  });

  syncs.push(s);
  transport.deliver({type: 'pong', t0: Date.now(), t: Date.now()});
  return {media, transport, s, loaded};
};

const state = (fields) => ({type: 'state', rev: 1, paused: true, position: 0, at: Date.now(), locked: false, ...fields});

test('needs a url or a transport', () => {
  assert.throws(() => sync(new FakeMedia(), {}), /url or a transport/);
});

test('joins the room and bursts pings when the connection opens', () => {
  const {transport} = setup({room: 'movie-night'});

  assert.deepEqual(transport.sent[0], {type: 'join', room: 'movie-night'});
  mock.timers.tick(1000);
  assert.equal(transport.sent.filter((m) => m.type === 'ping').length, 5);
});

test('measures the clock from pongs', () => {
  const {transport, s} = setup();

  transport.deliver({type: 'pong', t0: Date.now() - 40, t: Date.now() + 4980});
  assert.equal(s.stats().rtt, 0, 'the setup sample is still the fastest');
  assert.equal(s.stats().offset, 0);
});

test('loads the room source once', () => {
  const {transport, loaded} = setup();

  transport.deliver(state({src: {src: 'a.m3u8'}}));
  transport.deliver(state({rev: 2, src: {src: 'a.m3u8'}}));
  transport.deliver(state({rev: 3, src: {src: 'b.mp4'}}));
  assert.deepEqual(loaded, [{src: 'a.m3u8'}, {src: 'b.mp4'}]);
});

test('joining a playing room seeks to where it is and plays', async() => {
  const {media, transport} = setup();

  transport.deliver(state({paused: false, position: 30, at: Date.now() - 2000}));
  assert.deepEqual(media.seeks, [32]);
  assert.equal(media.playCalls, 1);
  await flush();
  assert.equal(media.paused, false);
  assert.deepEqual(transport.commands(), [], 'following the room sends nothing back');
});

test('small drift bends the playback rate, large drift seeks', async() => {
  const {media, transport, s} = setup();

  transport.deliver(state({paused: false, position: 0}));
  await flush();

  media._currentTime = 0.3; // 0.3s ahead
  s.reconcile();
  assert.equal(media.playbackRate, 0.95);

  media._currentTime = 0.01;
  s.reconcile();
  assert.equal(media.playbackRate, 1, 'back in step');

  mock.timers.tick(10000);
  media._currentTime = 4;
  s.reconcile();
  assert.equal(media.seeks.at(-1), 10);
  assert.equal(media.playbackRate, 1);
});

test('leaves a buffering player alone', async() => {
  const {media, transport, s} = setup();

  transport.deliver(state({paused: false, position: 0}));
  await flush();
  const seeks = media.seeks.length;

  media.readyState = 2;
  mock.timers.tick(5000);
  s.reconcile();
  assert.equal(media.seeks.length, seeks);
  assert.equal(media.playbackRate, 1);
});

test('the viewer pausing sends one pause; the room pausing sends nothing', async() => {
  const {media, transport} = setup();

  transport.deliver(state({paused: false, position: 10}));
  await flush();
  media.finishSeek();
  mock.timers.tick(1000);
  media._currentTime = 11;
  media.pause();
  assert.deepEqual(transport.commands(), [{type: 'command', action: 'pause', position: 11}]);

  // The server's answer arrives, and then someone else resumes.
  transport.deliver(state({rev: 2, paused: true, position: 11}));
  transport.deliver(state({rev: 3, paused: false, position: 11}));
  await flush();
  transport.deliver(state({rev: 4, paused: true, position: 11}));
  assert.equal(media.paused, true);
  assert.equal(transport.commands().length, 1);
});

test('pressing play sends play, then holds until the room starts everyone together', async() => {
  const {media, transport, s} = setup();

  transport.deliver(state({paused: true, position: 20}));
  media.finishSeek();
  await media.play();
  assert.deepEqual(transport.commands(), [{type: 'command', action: 'play', position: 20}]);
  assert.equal(media.paused, true, 'held while the room waits');
  assert.equal(s.stats().state.waiting, true);

  // The server waits for everyone, then starts the room 300ms out.
  transport.deliver(state({rev: 2, paused: false, position: 20, at: Date.now() + 5000, waiting: true}));
  assert.deepEqual(transport.sent.filter((m) => m.type === 'ready'), [{type: 'ready', rev: 2}]);
  transport.deliver(state({rev: 3, paused: false, position: 20, at: Date.now() + 300}));
  assert.equal(media.paused, true);
  mock.timers.tick(300);
  s.reconcile();
  await flush();
  assert.equal(media.paused, false);
  assert.equal(media.playCalls, 2, 'one press by the viewer, one synced start');
});

test('ready is only reported once buffered at the waiting spot', () => {
  const {media, transport, s} = setup();
  const readies = () => transport.sent.filter((m) => m.type === 'ready');

  media.readyState = 1;
  transport.deliver(state({rev: 4, paused: false, position: 50, at: Date.now() + 5000, waiting: true}));
  assert.deepEqual(media.seeks, [50]);
  media.finishSeek();
  s.reconcile();
  assert.equal(readies().length, 0, 'not buffered yet');
  media.readyState = 4;
  s.reconcile();
  s.reconcile();
  assert.deepEqual(readies(), [{type: 'ready', rev: 4}]);
});

test('seeks by the viewer are debounced; our own seeks are not commands', async() => {
  const {media, transport} = setup();

  transport.deliver(state({paused: false, position: 5}));
  await flush();
  media.finishSeek(); // our seek to 5
  assert.deepEqual(transport.commands(), []);

  media.userSeek(40);
  media.userSeek(55);
  media.userSeek(60);
  mock.timers.tick(150);
  assert.deepEqual(transport.commands(), [{type: 'command', action: 'seek', position: 60}]);
});

test('a scheduled room counts down, then plays, and refuses the viewer', async() => {
  const {media, transport, s} = setup({room: 'drive-in'});

  transport.deliver(state({locked: true, paused: false, position: 0, at: Date.now() + 20000}));
  assert.equal(media.playCalls, 0);
  assert.equal(s.stats().startsIn, 20);
  assert.equal(s.stats().expected, 0);

  await media.play(); // the viewer tries to start early
  s.reconcile();
  assert.equal(media.paused, true);

  mock.timers.tick(21000);
  s.reconcile();
  await flush();
  assert.equal(media.paused, false);

  media.pause();
  media.userSeek(80);
  mock.timers.tick(500);
  assert.deepEqual(transport.commands(), [], 'nothing is sent from a locked room');
  assert.ok(media.seeks.at(-1) < 2, 'and the player is put back');
});

test('a party room stops at the end with one pause', async() => {
  const {media, transport, s} = setup();

  transport.deliver(state({paused: false, position: 99}));
  await flush();
  mock.timers.tick(2000);
  s.reconcile();
  s.reconcile();
  assert.deepEqual(transport.commands(), [{type: 'command', action: 'pause', position: 100}]);
  assert.equal(media.paused, true);
});

test('autoplay refusal is reported once and not retried until the viewer clicks', async() => {
  const {media, transport, s} = setup();
  const blocked = [];

  s.addEventListener('blocked', () => blocked.push(true));
  media.playResult = () => Promise.reject(Object.assign(new Error('no'), {name: 'NotAllowedError'}));
  transport.deliver(state({paused: false, position: 0}));
  await flush();
  s.reconcile();
  s.reconcile();
  await flush();
  assert.equal(blocked.length, 1);
  assert.equal(media.playCalls, 1);
  assert.equal(s.stats().blocked, true);

  media.playResult = () => Promise.resolve();
  await media.play(); // the click
  assert.equal(s.stats().blocked, false);
});

test('offline commands are not applied locally', async() => {
  const transport = fakeTransport();
  const {media, s} = setup({transport});

  transport.deliver(state({paused: false, position: 0}));
  await flush();
  transport.open = false;
  media.pause();
  assert.equal(s.stats().state.paused, false);
  s.reconcile();
  assert.equal(media.playCalls, 2, 'the room is still playing, so the player resumes');
});

test('ignores malformed messages', () => {
  const {transport, s} = setup();

  transport.deliver(null);
  transport.deliver('hi');
  transport.deliver({type: 'state', paused: 'yes', position: 1, at: 1});
  transport.deliver({type: 'pong', t0: 'x', t: 1});
  assert.equal(s.stats().state, null);
});

test('destroy stops everything and lets go of the transport', () => {
  const {media, transport, s} = setup();

  transport.deliver(state({paused: false, position: 0}));
  media.playbackRate = 0.95;
  syncs = [];
  s.destroy();
  assert.equal(transport.listenerCount(), 0);
  assert.equal(media.playbackRate, 1);
  const sent = transport.sent.length;

  mock.timers.tick(60000);
  assert.equal(transport.sent.length, sent, 'no more pings');
});

test('seeks during playback lead by the measured seek time', async() => {
  const {media, transport, s} = setup();

  transport.deliver(state({paused: false, position: 0}));
  await flush();

  media._currentTime = 5; // 5s ahead: hard seek
  s.reconcile();
  assert.equal(media.seeks.at(-1), 0, 'no measurement yet, so no lead');
  mock.timers.tick(400); // the seek takes 0.4s
  media.finishSeek();
  assert.equal(s.stats().seekLatency, 0.4);

  media._currentTime = 20;
  s.reconcile();
  assert.equal(media.seeks.at(-1), 0.4 + 0.4, 'room is at 0.4s; aim 0.4s past it');
  mock.timers.tick(200);
  media.finishSeek();
  assert.ok(Math.abs(s.stats().seekLatency - 0.34) < 1e-9, 'smoothed toward the new sample');
});

test('seeks while the room is paused do not lead', () => {
  const {media, transport} = setup();

  transport.deliver(state({paused: true, position: 12}));
  assert.equal(media.seeks.at(-1), 12);
});

test('starts at the exact start time, not on the next tick', async() => {
  const {media, transport} = setup();

  transport.deliver(state({rev: 2, paused: false, position: 10, at: Date.now() + 310}));
  assert.equal(media.paused, true);
  mock.timers.tick(309);
  assert.equal(media.paused, true);
  mock.timers.tick(1); // 310ms: between the 250ms and 500ms ticks
  assert.equal(media.paused, false);
});

test('a synced start calls play() early by the measured start-up time', async() => {
  const {media, transport, s} = setup();

  // First start: measure how long play() takes to get frames moving.
  transport.deliver(state({rev: 2, paused: false, position: 0, at: Date.now() + 100}));
  mock.timers.tick(100);
  await flush();
  mock.timers.tick(80);
  media.dispatchEvent(new Event('playing'));
  assert.equal(s.stats().playLatency, 0.08);

  transport.deliver(state({rev: 3, paused: true, position: 5}));
  assert.equal(media.paused, true);
  transport.deliver(state({rev: 4, paused: false, position: 5, at: Date.now() + 1000}));
  mock.timers.tick(919);
  assert.equal(media.paused, true);
  mock.timers.tick(1);
  assert.equal(media.paused, false, 'started 80ms before the room');
});

test('a paused player lines up on the exact frame', () => {
  const {media, transport} = setup();

  transport.deliver(state({paused: true, position: 12.345}));
  assert.deepEqual(media.seeks, [12.345]);
  media._currentTime = 12.34; // the browser rounded to a frame
  media.finishSeek();
  transport.deliver(state({rev: 2, paused: true, position: 12.345}));
  assert.deepEqual(media.seeks, [12.345], 'no seek loop');
});
