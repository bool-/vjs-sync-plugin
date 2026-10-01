// Keeps a media element in step with a room on a sync server.
//
// The server owns the room's timeline. The player sends commands (play,
// pause, seek) and follows the state the server broadcasts back; it never
// reports its own position as truth, so a buffering viewer cannot drag the
// room. After a play or seek the room waits: every player buffers at the
// new spot and says it is ready, then all of them start together (or the
// server gives up waiting on stragglers). Drift is corrected with small
// playback-rate changes, and only large drift gets a hard seek. Locked rooms
// (a scheduled showing) take no commands at all.

import Clock from './clock.js';
import {positionAt, startsIn, applyCommand, correctionFor} from './timeline.js';
import {connect} from './transport.js';
import {loadSource, DEFAULT_HLS_URL} from './source.js';

// HTMLMediaElement.HAVE_FUTURE_DATA: enough buffered to keep playing.
const HAVE_FUTURE_DATA = 3;

// A seeked event this close to the last seek we made is ours, not the viewer's.
const OWN_SEEK_SLOP = 0.5;

// Longest seek or start-up time we will lead by, in seconds.
const MAX_SEEK_LEAD = 2;
const MAX_PLAY_LEAD = 0.5;

// Close enough to the waiting spot to call ourselves ready, in seconds.
const READY_SLOP = 0.1;

export const DEFAULTS = {
  room: 'default',
  tickInterval: 250,
  pingInterval: 5000,
  pingBurst: 5,
  seekThreshold: 0.5,
  // Rate changes this small keep pitch and go unnoticed, so correct early.
  driftTolerance: 0.04,
  settleTolerance: 0.015,
  maxRateAdjust: 0.05,
  rateGain: 0.5,
  // Native controls fire a seeked per drag step; send only the last one.
  seekDebounce: 150,
  hlsUrl: DEFAULT_HLS_URL,
  // (media, src, hlsUrl) => Promise<release>. Swap it when a player library
  // owns the element, e.g. with video.js: async (m, src) => { player.src(src); return () => {}; }
  loadSource
};

const isState = (m) =>
  typeof m.paused === 'boolean' && Number.isFinite(m.position) && Number.isFinite(m.at);

/**
 * Sync a media element to a room.
 *
 * Events (CustomEvent, data on `detail`): `state` ({state}) on every room
 * state; `blocked` when autoplay is refused and the viewer has to click
 * play; `syncerror` ({message}) when the server rejects something.
 */
export class MediaSync extends EventTarget {

  /**
   * @param {HTMLMediaElement} media
   * @param {Object} options
   * @param {string} [options.url] Sync server WebSocket URL.
   * @param {Object} [options.transport] Use this instead of connecting to `url`.
   * @param {string} [options.room='default']
   */
  constructor(media, options) {
    super();
    this.media = media;
    this.options = {...DEFAULTS, ...options};

    if (!this.options.transport && !this.options.url) {
      throw new Error('MediaSync needs a server url or a transport');
    }

    this.ownsTransport = !this.options.transport;
    this.transport = this.options.transport || connect(this.options.url);
    this.clock = new Clock();
    this.state = null;
    this.sourceKey = null;
    this.releaseSource = null;
    this.ownSeekTarget = null;
    this.seekStartedAt = null;
    // How long this player's seeks take, in seconds. The room keeps moving
    // while the player fetches the new spot, so seeks during playback aim
    // this far ahead.
    this.seekLatency = 0;
    // Same idea for play(): how long from the call to frames moving. A
    // synced start calls play() this much early.
    this.playLatency = 0;
    this.playStartedAt = null;
    this.pausedTarget = null;
    this.pendingSeek = null;
    this.readyRev = null;
    this.startTimer = null;
    this.startTimerAt = null;
    this.endReported = false;
    this.blocked = false;
    this.wantsPlaying = false;
    this.rate = 1;
    this.drift = null;
    this.timeouts = new Set();

    this.handlers = {
      play: () => this.handleLocalPlay(),
      playing: () => this.recordPlayTime(),
      pause: () => this.handleLocalPause(),
      seeked: () => this.handleLocalSeeked()
    };
    for (const [type, fn] of Object.entries(this.handlers)) {
      media.addEventListener(type, fn);
    }

    this.unsubscribers = [
      this.transport.onOpen(() => this.handleOpen()),
      this.transport.onMessage((message) => this.handleMessage(message))
    ];

    this.tickTimer = setInterval(() => this.reconcile(), this.options.tickInterval);
    this.pingTimer = setInterval(() => this.ping(), this.options.pingInterval);
  }

  later(fn, ms) {
    const id = setTimeout(() => {
      this.timeouts.delete(id);
      fn();
    }, ms);

    this.timeouts.add(id);
    return id;
  }

  emit(type, detail) {
    this.dispatchEvent(new CustomEvent(type, {detail}));
  }

  // Join the room and measure the clock with a quick burst of pings. Runs
  // on every (re)connect.
  handleOpen() {
    this.transport.send({type: 'join', room: this.options.room});
    for (let i = 0; i < this.options.pingBurst; i++) {
      this.later(() => this.ping(), i * 150);
    }
  }

  ping() {
    this.transport.send({type: 'ping', t0: Date.now()});
  }

  handleMessage(message) {
    if (!message || typeof message !== 'object') {
      return;
    }
    if (message.type === 'pong') {
      if (Number.isFinite(message.t0) && Number.isFinite(message.t)) {
        this.clock.addSample(message.t0, message.t, Date.now());
      }
    } else if (message.type === 'state') {
      if (isState(message)) {
        this.setRoomState({
          paused: message.paused,
          position: message.position,
          at: message.at,
          locked: message.locked === true,
          waiting: message.waiting === true,
          rev: message.rev,
          src: message.src || null
        });
      }
    } else if (message.type === 'error') {
      this.emit('syncerror', {message: message.message});
    }
  }

  // Adopt a room state, from the server or optimistically from a command.
  setRoomState(state) {
    const previous = this.state;

    this.state = state;
    if (!previous || previous.paused !== state.paused || previous.position !== state.position) {
      this.endReported = false;
    }

    const key = state.src ? JSON.stringify(state.src) : null;

    if (key && key !== this.sourceKey) {
      this.sourceKey = key;
      this.switchSource(state.src);
    }

    this.emit('state', {state});
    this.reconcile();
  }

  async switchSource(src) {
    if (this.releaseSource) {
      this.releaseSource();
      this.releaseSource = null;
    }
    try {
      const release = await this.options.loadSource(this.media, src, this.options.hlsUrl);

      if (this.sourceKey === JSON.stringify(src)) {
        this.releaseSource = release;
      } else {
        release();
      }
    } catch (error) {
      this.emit('syncerror', {message: `could not load source: ${error.message}`});
    }
  }

  // Where the room is right now in seconds, or null before the first state
  // and clock sample arrive.
  expectedPosition() {
    if (!this.state || !this.clock.ready) {
      return null;
    }
    return positionAt(this.state, this.clock.now());
  }

  // Bring the player in line with the room. Runs on a timer and on every
  // new state.
  reconcile() {
    const media = this.media;
    const state = this.state;
    const expected = this.expectedPosition();

    if (expected === null || media.seeking || this.pendingSeek !== null) {
      return;
    }

    const holding = startsIn(state, this.clock.now()) > this.playLatency;

    if (holding) {
      this.scheduleStart(state.at);
    }

    const duration = media.duration;
    const hasDuration = Number.isFinite(duration) && duration > 0;
    const pastEnd = hasDuration && expected >= duration;
    const target = Math.max(0, pastEnd ? duration : expected);

    // A party room plays out to the end and stops there for everyone, so
    // pressing play afterwards restarts it cleanly.
    if (pastEnd && !state.paused && !state.locked && !this.endReported) {
      this.endReported = true;
      this.sendCommand('pause', duration);
      return;
    }

    this.wantsPlaying = !state.paused && !holding && !pastEnd;

    if (!this.wantsPlaying) {
      this.drift = null;
      this.setRate(1);
      if (!media.paused) {
        media.pause();
      }
      // Stopped, so line up on the exact frame; seeking a paused player is
      // invisible. Once per target, in case the browser rounds the time.
      if (media.currentTime !== target && this.pausedTarget !== target) {
        this.pausedTarget = target;
        this.seekTo(target);
      } else {
        this.reportReady(target);
      }
      return;
    }
    this.pausedTarget = null;

    if (media.paused) {
      if (Math.abs(media.currentTime - target) > this.options.seekThreshold) {
        this.seekTo(target, true);
      }
      if (!this.blocked) {
        this.play();
      }
      return;
    }

    if (media.readyState < HAVE_FUTURE_DATA) {
      // Buffering; whatever drift it causes is dealt with once it resumes.
      return;
    }

    this.drift = media.currentTime - expected;

    const correction = correctionFor(this.drift, this.rate, this.options);

    if (correction.seek) {
      this.setRate(1);
      this.seekTo(target, true);
    } else {
      this.setRate(correction.rate);
    }
  }

  // The tick is too coarse to start on: wake up exactly when the room does,
  // so every player starts within a frame or two of the others.
  scheduleStart(at) {
    if (!Number.isFinite(at) || at === this.startTimerAt) {
      return;
    }
    if (this.startTimer !== null) {
      clearTimeout(this.startTimer);
      this.timeouts.delete(this.startTimer);
    }
    this.startTimerAt = at;
    this.startTimer = this.later(() => {
      this.startTimer = null;
      this.reconcile();
    }, Math.max(0, at - this.playLatency * 1000 - this.clock.now()));
  }

  // Tell a waiting room we are buffered at its spot, once per state.
  reportReady(target) {
    const media = this.media;

    if (!this.state.waiting || this.state.rev === null || this.readyRev === this.state.rev ||
        media.readyState < HAVE_FUTURE_DATA || Math.abs(media.currentTime - target) > READY_SLOP) {
      return;
    }
    if (this.transport.send({type: 'ready', rev: this.state.rev})) {
      this.readyRev = this.state.rev;
    }
  }

  play() {
    this.playStartedAt = Date.now();

    const result = this.media.play();

    if (result && typeof result.then === 'function') {
      result.then(() => {
        this.blocked = false;
      }, (error) => {
        if (error && error.name === 'NotAllowedError' && !this.blocked) {
          this.blocked = true;
          this.emit('blocked');
        }
      });
    }
  }

  // With `lead`, the room is playing: aim ahead by the measured seek time
  // and time this seek to refine the measurement.
  seekTo(position, lead = false) {
    const target = lead ? position + this.seekLatency : position;

    this.ownSeekTarget = target;
    this.seekStartedAt = lead ? Date.now() : null;
    this.media.currentTime = target;
  }

  recordPlayTime() {
    if (this.playStartedAt === null) {
      return;
    }

    const sample = Math.min(MAX_PLAY_LEAD, (Date.now() - this.playStartedAt) / 1000);

    this.playStartedAt = null;
    this.playLatency = this.playLatency ? 0.7 * this.playLatency + 0.3 * sample : sample;
  }

  recordSeekTime() {
    if (this.seekStartedAt === null) {
      return;
    }

    const sample = Math.min(MAX_SEEK_LEAD, (Date.now() - this.seekStartedAt) / 1000);

    this.seekStartedAt = null;
    this.seekLatency = this.seekLatency ? 0.7 * this.seekLatency + 0.3 * sample : sample;
  }

  // Rounded to 0.01 so the rate is only touched when it really changes.
  setRate(rate) {
    const rounded = Math.round(rate * 100) / 100;

    if (rounded !== this.rate) {
      this.rate = rounded;
      this.media.playbackRate = rounded;
    }
  }

  // Send a command and apply it locally while the server's answer is in
  // flight. Offline or locked, nothing is sent, and the next reconcile puts
  // the player back where the room is.
  sendCommand(action, position) {
    if (!this.state || this.state.locked) {
      return;
    }

    const command = {type: 'command', action, position};

    if (!this.transport.send(command) || !this.clock.ready) {
      return;
    }

    const next = applyCommand(this.state, command, this.clock.now());

    // The server will make the room wait for everyone to buffer before it
    // plays; hold here until it says when to start.
    if (!next.paused) {
      next.waiting = true;
      next.at = Infinity;
      // Not a server state, so there is nothing to report ready for yet.
      next.rev = null;
    }
    this.setRoomState(next);
  }

  // The viewer pressed play. Our own play() calls only happen while the
  // room is already playing, so they never turn into a command here.
  handleLocalPlay() {
    this.blocked = false;
    if (this.state && this.state.paused) {
      this.sendCommand('play', this.media.currentTime);
    }
  }

  // The viewer paused. Pauses we make (room paused, countdown, end of
  // media) happen when the room does not want playback, so they are
  // ignored here.
  handleLocalPause() {
    if (this.wantsPlaying && !this.media.ended) {
      this.sendCommand('pause', this.media.currentTime);
    }
  }

  // The viewer seeked, unless the seek landed where we sent it. Drags are
  // debounced so only where the viewer let go is sent.
  handleLocalSeeked() {
    const position = this.media.currentTime;

    if (this.ownSeekTarget !== null) {
      const ours = Math.abs(position - this.ownSeekTarget) < OWN_SEEK_SLOP;

      this.ownSeekTarget = null;
      if (ours) {
        this.recordSeekTime();
        return;
      }
      this.seekStartedAt = null;
    }

    if (this.pendingSeek !== null) {
      clearTimeout(this.pendingSeek);
      this.timeouts.delete(this.pendingSeek);
    }
    this.pendingSeek = this.later(() => {
      this.pendingSeek = null;

      const latest = this.media.currentTime;
      const expected = this.expectedPosition();

      if (expected !== null && Math.abs(latest - expected) > this.options.seekThreshold) {
        this.sendCommand('seek', latest);
      }
    }, this.options.seekDebounce);
  }

  /**
   * @return {Object} offset and rtt (ms), drift (s, positive when ahead),
   * rate, the room's expected position and state, and whether autoplay is
   * blocked.
   */
  stats() {
    return {
      offset: this.clock.ready ? this.clock.offset : null,
      rtt: this.clock.rtt,
      drift: this.drift,
      rate: this.rate,
      seekLatency: this.seekLatency,
      playLatency: this.playLatency,
      expected: this.expectedPosition(),
      startsIn: this.state && this.clock.ready ? startsIn(this.state, this.clock.now()) : null,
      state: this.state,
      blocked: this.blocked
    };
  }

  // Stop syncing. A transport passed in by the caller stays open.
  destroy() {
    clearInterval(this.tickTimer);
    clearInterval(this.pingTimer);
    this.timeouts.forEach((id) => clearTimeout(id));
    this.timeouts.clear();
    this.unsubscribers.forEach((off) => off());
    for (const [type, fn] of Object.entries(this.handlers)) {
      this.media.removeEventListener(type, fn);
    }
    this.media.playbackRate = 1;
    if (this.releaseSource) {
      this.releaseSource();
    }
    if (this.ownsTransport) {
      this.transport.close();
    }
  }
}

/**
 * @param  {HTMLMediaElement} media
 * @param  {Object} options See MediaSync.
 * @return {MediaSync}
 */
export const sync = (media, options) => new MediaSync(media, options);
