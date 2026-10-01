/**
 * Estimates the offset between the local clock and the server's from
 * ping/pong round trips. The sample with the shortest round trip wins:
 * its midpoint assumption is the least wrong.
 */
export default class Clock {

  /**
   * @param {number} [maxSamples=8]
   *        How many recent samples to keep.
   */
  constructor(maxSamples = 8) {
    this.maxSamples = maxSamples;
    this.samples = [];
  }

  /**
   * Record one round trip.
   *
   * @param {number} sentAt
   *        Local time the ping was sent, in ms.
   *
   * @param {number} serverTime
   *        Server time stamped on the pong, in ms.
   *
   * @param {number} receivedAt
   *        Local time the pong arrived, in ms.
   */
  addSample(sentAt, serverTime, receivedAt) {
    const rtt = receivedAt - sentAt;

    if (!(rtt >= 0)) {
      return;
    }
    this.samples.push({rtt, offset: serverTime - (sentAt + receivedAt) / 2});
    if (this.samples.length > this.maxSamples) {
      this.samples.shift();
    }
  }

  /**
   * @return {boolean}
   *         Whether there is at least one sample.
   */
  get ready() {
    return this.samples.length > 0;
  }

  /**
   * @return {Object|null}
   *         The sample with the shortest round trip.
   */
  get best() {
    return this.samples.reduce((best, s) => (!best || s.rtt < best.rtt ? s : best), null);
  }

  /**
   * @return {number}
   *         Server time minus local time, in ms.
   */
  get offset() {
    return this.ready ? this.best.offset : 0;
  }

  /**
   * @return {number|null}
   *         The best round trip, in ms.
   */
  get rtt() {
    return this.ready ? this.best.rtt : null;
  }

  /**
   * @param  {number} [localNow=Date.now()]
   *         A local time, in ms.
   *
   * @return {number}
   *         The matching server time, in ms.
   */
  now(localNow = Date.now()) {
    return localNow + this.offset;
  }
}
