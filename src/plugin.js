import videojs from 'video.js';
import { version as VERSION } from '../package.json';

const Plugin = videojs.getPlugin('plugin');

class SyncPlugin extends Plugin {

  constructor(player, options) {
    super(player);

    if (!options.connection) {
      throw new Error('A WebSocket or Socket.IO connection object is required');
    }

    this.connection = options.connection;
    this.connectionType = this.connection.constructor.name;
    this.supportedTypes = ['WebSocket', 'io'];
    this.syncInterval = options.syncInterval || 5000;

    if (!this.supportedTypes.includes(this.connectionType)) {
      throw new Error(`Unsupported connection type: ${this.connectionType}. Supported types are: ${this.supportedTypes.join(', ')}`);
    }

    this.player.ready(() => {
      this.player.addClass('vjs-sync-plugin');
      this.setupConnectionHandlers();
      this.addEventListeners();
    });
  }

  setupConnectionHandlers() {
    const events = {
      connect: () => console.log('Connected to sync server'),
      disconnect: () => console.log('Disconnected from sync server'),
      error: (error) => console.error(`${this.connectionType} Error:`, error),
      message: (data) => this.handleIncomingMessage(data)
    };

    if (this.connectionType === 'WebSocket') {
      this.setupWebSocketEvents(events);
    } else if (this.connectionType === 'io') {
      this.setupSocketIOEvents(events);
    }
  }

  setupWebSocketEvents(events) {
    this.connection.onopen = events.connect;
    this.connection.onclose = events.disconnect;
    this.connection.onerror = events.error;
    this.connection.onmessage = (event) => events.message(JSON.parse(event.data));
  }

  setupSocketIOEvents(events) {
    this.connection.on('connect', events.connect);
    this.connection.on('disconnect', events.disconnect);
    this.connection.on('error', events.error);
    this.connection.on('sync_event', events.message);
  }

  handleIncomingMessage(data) {
    const callback = this.eventCallbacks[data.type];
    if (callback) {
      callback(data);
    } else {
      console.warn(`Unknown message type: ${data.type}`);
    }
  }

  addEventListeners() {
    const playerEvents = ['play', 'pause', 'seek'];

    playerEvents.forEach(event => {
      this.player.on(event, () => {
        this.sendMessage(event, { currentTime: this.player.currentTime() });
      });
    });

    this.eventCallbacks = {
      play: this.handlePlay.bind(this),
      pause: this.handlePause.bind(this),
      stop: this.handleStop.bind(this),
      seeked: this.handleSeeked.bind(this),
      sync: this.handleSync.bind(this)
    };

    // Emit sync event periodically
    setInterval(() => {
      this.sendMessage('sync', {
        isPlaying: !this.player.paused(),
        currentTime: this.player.currentTime()
      });
    }, this.syncInterval);
  }

  handlePlay(data) {
    this.player.currentTime(data.currentTime);
    this.player.play();
    this.trigger('syncplay');
  }

  handlePause(data) {
    this.player.currentTime(data.currentTime);
    this.player.pause();
    this.trigger('syncpause');
  }

  handleStop(data) {
    this.player.currentTime(0);
    this.player.pause();
    this.trigger('syncstop');
  }

  handleSeeked(data) {
    this.player.currentTime(data.currentTime);
    this.trigger('syncseeked');
  }

  handleSync(data) {
    this.player.currentTime(data.currentTime);
    if (data.isPlaying && this.player.paused()) {
      this.player.play();
    } else if (!data.isPlaying && !this.player.paused()) {
      this.player.pause();
    }
    this.trigger('syncevent');
  }

  sendMessage(type, data) {
    const message = { type, ...data };
    if (this.connectionType === 'WebSocket') {
      if (this.connection.readyState === WebSocket.OPEN) {
        this.connection.send(JSON.stringify(message));
      }
    } else {
      this.connection.emit('sync_event', message);
    }
  }
}

SyncPlugin.prototype.allowedEvents_ = {
  syncplay: 'syncplay',
  syncpause: 'syncpause',
  syncstop: 'syncstop',
  syncseeked: 'syncseeked',
  syncevent: 'syncevent'
};

for (const event in SyncPlugin.prototype.allowedEvents_) {
  SyncPlugin.prototype['on' + event] = null;
}

SyncPlugin.VERSION = VERSION;

videojs.registerPlugin('syncPlugin', SyncPlugin);

export default SyncPlugin;