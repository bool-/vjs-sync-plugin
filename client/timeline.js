/**
 * Pure playback-timeline math. Server times are epoch milliseconds on the
 * server's clock; media positions are seconds.
 *
 * A room's state is `{paused, position, at}`: the media is at `position` at
 * server time `at`, and if it is not paused it advances in real time from
 * then. Until `at` it holds at `position`, which is how a room waits for
 * everyone to buffer, and how a scheduled showing counts down.
 */

/**
 * Where the room's media should be at a given server time.
 *
 * @param  {Object} state
 *         A room state.
 *
 * @param  {number} serverNow
 *         The current time on the server's clock, in ms.
 *
 * @return {number}
 *         The expected media position, in seconds.
 */
export const positionAt = (state, serverNow) => {
  if (state.paused || serverNow < state.at) {
    return state.position;
  }
  return state.position + (serverNow - state.at) / 1000;
};

/**
 * Seconds until a playing room starts moving; 0 once it has.
 *
 * @param  {Object} state
 *         A room state.
 *
 * @param  {number} serverNow
 *         The current time on the server's clock, in ms.
 *
 * @return {number}
 *         Seconds to go.
 */
export const startsIn = (state, serverNow) =>
  !state.paused && serverNow < state.at ? (state.at - serverNow) / 1000 : 0;

/**
 * Apply a play, pause or seek command to a room state. The server runs the
 * same rules; clients use this to update optimistically while the
 * authoritative state is on its way.
 *
 * @param  {Object} state
 *         The current room state.
 *
 * @param  {Object} command
 *         `{action, position}`; position defaults to the current position.
 *
 * @param  {number} serverNow
 *         The current time on the server's clock, in ms.
 *
 * @return {Object}
 *         The new room state.
 */
export const applyCommand = (state, command, serverNow) => {
  const position = Number.isFinite(command.position) ?
    Math.max(0, command.position) :
    positionAt(state, serverNow);
  let paused;

  if (command.action === 'play') {
    paused = false;
  } else if (command.action === 'pause') {
    paused = true;
  } else if (command.action === 'seek') {
    paused = state.paused;
  } else {
    return state;
  }

  return Object.assign({}, state, {paused, position, at: serverNow});
};

/**
 * Decide how to correct a player that has drifted from the room.
 *
 * Small drift is absorbed by nudging the playback rate, which is invisible;
 * large drift gets a hard seek. A rate correction keeps running until the
 * drift falls under `settleTolerance`, which is tighter than the tolerance
 * that starts it, so the rate does not flap at the boundary.
 *
 * @param  {number} drift
 *         Seconds the player is ahead of the room (negative when behind).
 *
 * @param  {number} currentRate
 *         The playback rate currently applied.
 *
 * @param  {Object} options
 *         seekThreshold, driftTolerance, settleTolerance, maxRateAdjust, rateGain.
 *
 * @return {Object}
 *         `{seek: boolean, rate: number}`.
 */
export const correctionFor = (drift, currentRate, options) => {
  const size = Math.abs(drift);

  if (size > options.seekThreshold) {
    return {seek: true, rate: 1};
  }

  const tolerance = currentRate === 1 ? options.driftTolerance : options.settleTolerance;

  if (size <= tolerance) {
    return {seek: false, rate: 1};
  }

  const adjust = Math.min(options.maxRateAdjust, size * options.rateGain);

  return {seek: false, rate: drift > 0 ? 1 - adjust : 1 + adjust};
};
