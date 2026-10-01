// JSON over a WebSocket that reconnects with exponential backoff.
//
// A transport is anything with this shape, so tests and other wires can
// stand in for it:
//
//   send(message)  -> boolean        false when offline; the message is dropped
//   onMessage(fn)  -> unsubscribe
//   onOpen(fn)     -> unsubscribe    fn also runs at once if already open
//   close()

const listeners = () => {
  const set = new Set();

  return {
    add: (fn) => {
      set.add(fn);
      return () => set.delete(fn);
    },
    emit: (value) => set.forEach((fn) => fn(value))
  };
};

/**
 * @param  {string} url ws:// or wss:// URL of the sync server.
 * @param  {Object} [options]
 * @param  {number} [options.minDelay=500] First reconnect delay, ms.
 * @param  {number} [options.maxDelay=10000] Longest reconnect delay, ms.
 * @param  {Function} [options.WebSocket] Constructor; defaults to the global.
 * @return {Object} A transport.
 */
export const connect = (url, {minDelay = 500, maxDelay = 10000, WebSocket = globalThis.WebSocket} = {}) => {
  const messages = listeners();
  const opens = listeners();
  let socket;
  let open = false;
  let closed = false;
  let delay = minDelay;
  let timer;

  const dial = () => {
    socket = new WebSocket(url);
    socket.onopen = () => {
      open = true;
      delay = minDelay;
      opens.emit();
    };
    socket.onmessage = (event) => {
      let message;

      try {
        message = JSON.parse(event.data);
      } catch {
        return;
      }
      messages.emit(message);
    };
    socket.onclose = () => {
      open = false;
      if (!closed) {
        timer = setTimeout(dial, delay);
        delay = Math.min(delay * 2, maxDelay);
      }
    };
  };

  dial();

  return {
    send(message) {
      if (!open) {
        return false;
      }
      socket.send(JSON.stringify(message));
      return true;
    },
    onMessage: messages.add,
    onOpen(fn) {
      const off = opens.add(fn);

      if (open) {
        fn();
      }
      return off;
    },
    close() {
      closed = true;
      clearTimeout(timer);
      socket.close();
    }
  };
};
