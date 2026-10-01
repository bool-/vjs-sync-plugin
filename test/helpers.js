// Stand-ins for a media element and a transport, driven by hand.

export class FakeMedia extends EventTarget {
  constructor() {
    super();
    this.paused = true;
    this.seeking = false;
    this.ended = false;
    this.readyState = 4;
    this.duration = 100;
    this.playbackRate = 1;
    this.playCalls = 0;
    this.seeks = [];
    this.playResult = () => Promise.resolve();
    this._currentTime = 0;
  }

  get currentTime() {
    return this._currentTime;
  }

  // Seeks land at once; seeked fires when the test calls finishSeek().
  set currentTime(value) {
    this._currentTime = value;
    this.seeks.push(value);
  }

  finishSeek() {
    this.dispatchEvent(new Event('seeked'));
  }

  // What a viewer dragging the scrubber does.
  userSeek(value) {
    this._currentTime = value;
    this.finishSeek();
  }

  // Like the real thing: paused flips at once, the play event follows, and
  // a refused play flips it back.
  play() {
    this.playCalls++;
    const wasPaused = this.paused;

    this.paused = false;
    return this.playResult().then(() => {
      if (wasPaused) {
        this.dispatchEvent(new Event('play'));
      }
    }, (error) => {
      this.paused = true;
      throw error;
    });
  }

  pause() {
    if (!this.paused) {
      this.paused = true;
      this.dispatchEvent(new Event('pause'));
    }
  }

  canPlayType() {
    return '';
  }

  removeAttribute() {}
}

export const fakeTransport = ({open = true} = {}) => {
  const messageFns = new Set();
  const openFns = new Set();

  return {
    sent: [],
    open,
    send(message) {
      if (!this.open) {
        return false;
      }
      this.sent.push(message);
      return true;
    },
    onMessage(fn) {
      messageFns.add(fn);
      return () => messageFns.delete(fn);
    },
    onOpen(fn) {
      openFns.add(fn);
      if (this.open) {
        fn();
      }
      return () => openFns.delete(fn);
    },
    close() {},
    deliver(message) {
      messageFns.forEach((fn) => fn(message));
    },
    commands() {
      return this.sent.filter((m) => m.type === 'command');
    },
    listenerCount() {
      return messageFns.size + openFns.size;
    }
  };
};

// Let promise callbacks (play() results) run.
export const flush = () => new Promise((resolve) => setImmediate(resolve));
