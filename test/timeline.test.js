import {test} from 'node:test';
import assert from 'node:assert/strict';
import {positionAt, startsIn, applyCommand, correctionFor} from '../client/timeline.js';
import Clock from '../client/clock.js';
import {DEFAULTS} from '../client/sync.js';
import {isHls, normalizeSource} from '../client/source.js';

test('positionAt holds while paused and advances in real time while playing', () => {
  assert.equal(positionAt({paused: true, position: 5, at: 0}, 9000), 5);
  assert.equal(positionAt({paused: false, position: 5, at: 1000}, 3500), 7.5);
});

test('a playing room holds at its position until its start time', () => {
  const upcoming = {paused: false, position: 0, at: 10000};

  assert.equal(positionAt(upcoming, 4000), 0);
  assert.equal(startsIn(upcoming, 4000), 6);
  assert.equal(startsIn(upcoming, 12000), 0);
  assert.equal(startsIn({paused: true, position: 0, at: 10000}, 4000), 0, 'paused rooms are not counting down');
});

test('applyCommand: play, pause and seek', () => {
  const playing = {paused: false, position: 10, at: 0, locked: false};

  assert.deepEqual(applyCommand(playing, {action: 'pause', position: 12}, 2000),
    {paused: true, position: 12, at: 2000, locked: false});
  assert.deepEqual(applyCommand(playing, {action: 'seek', position: 50}, 2000),
    {paused: false, position: 50, at: 2000, locked: false});
  assert.equal(applyCommand(playing, {action: 'pause'}, 3000).position, 13, 'no position: where the room is');
  assert.equal(applyCommand(playing, {action: 'seek', position: -4}, 0).position, 0);
  assert.equal(applyCommand(playing, {action: 'rewind'}, 0), playing);
});

test('correctionFor: ignore, nudge, settle, seek', () => {
  const o = DEFAULTS;

  assert.deepEqual(correctionFor(0.03, 1, o), {seek: false, rate: 1}, 'inside tolerance');
  assert.deepEqual(correctionFor(0.2, 1, o), {seek: false, rate: 0.95}, 'ahead: slow down, capped');
  assert.equal(correctionFor(-0.06, 1, o).rate, 1.03, 'behind: speed up in proportion');
  assert.equal(correctionFor(-0.12, 1, o).rate, 1.05, 'capped');
  assert.equal(correctionFor(0.03, 0.98, o).rate, 0.985, 'keeps correcting below the start tolerance');
  assert.equal(correctionFor(0.01, 0.98, o).rate, 1, 'settles');
  assert.deepEqual(correctionFor(-1.5, 1, o), {seek: true, rate: 1});
});

test('Clock trusts the fastest round trip', () => {
  const clock = new Clock(3);

  assert.equal(clock.ready, false);
  clock.addSample(1000, 6100, 1200); // rtt 200, offset 5000
  clock.addSample(2000, 7010, 2020); // rtt 20, offset 5000
  clock.addSample(3000, 8500, 3100); // rtt 100, offset 5450
  assert.equal(clock.rtt, 20);
  assert.equal(clock.offset, 5000);
  assert.equal(clock.now(10), 5010);
  clock.addSample(5, 0, 1); // clock went backwards: ignored
  assert.equal(clock.samples.length, 3);
  clock.addSample(4000, 9000, 4050); // the oldest sample (rtt 200) drops out
  assert.equal(clock.samples.length, 3);
  assert.equal(clock.rtt, 20);
});

test('source helpers spot HLS by type or extension', () => {
  assert.equal(isHls(normalizeSource('https://x/movie.mp4')), false);
  assert.equal(isHls(normalizeSource('https://x/start.m3u8?token=1')), true);
  assert.equal(isHls(normalizeSource({src: 'https://x/stream', type: 'application/x-mpegURL'})), true);
});
