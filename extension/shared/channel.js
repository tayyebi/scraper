// Agent core: the command channel.
//
// Reverse-connected: the agent dials the hub and the hub issues the requests.
// That direction is forced by reality -- agents run on laptops and phones
// behind NAT, and there is no address for a hub to dial.

var Hub = self.Hub || (self.Hub = {});

Hub.Channel = (function () {
  const protocol = Hub.protocol;

  // Chrome evicts an MV3 service worker after roughly 30 seconds idle. Since
  // Chrome 116 WebSocket traffic counts as activity and resets that timer, so
  // this ping is not only a liveness check -- it is what keeps the agent alive
  // at all. chrome.alarms is the backstop in background.js for the case where
  // the socket is already gone.
  const DEFAULT_PING_MS = 20_000;

  // Reconnect backoff. Capped low because an agent that takes ten minutes to
  // notice the hub came back is, to an operator, an agent that is broken.
  const BACKOFF_MIN_MS = 1_000;
  const BACKOFF_MAX_MS = 30_000;

  class Channel {
    /**
     * @param {object} options
     * @param {string} options.hubURL      base hub URL, e.g. https://hub.example
     * @param {string} options.credential  the agent credential from enrollment
     * @param {function} options.onCommand async (envelope) => body, called per cmd
     * @param {function} [options.onStatus] (state, detail) => void
     * @param {number}  [options.pingMs]
     */
    constructor(options) {
      this.hubURL = String(options.hubURL || '').replace(/\/+$/, '');
      this.credential = options.credential;
      this.onCommand = options.onCommand;
      this.onStatus = options.onStatus || (() => {});
      this.pingMs = options.pingMs || DEFAULT_PING_MS;

      this.socket = null;
      this.pingTimer = null;
      this.retryTimer = null;
      this.attempt = 0;
      this.stopped = false;
    }

    get connected() {
      return Boolean(this.socket && this.socket.readyState === WebSocket.OPEN);
    }

    url() {
      const base = this.hubURL.replace(/^http/, 'ws');
      return `${base}/agent/v1/channel`;
    }

    connect() {
      if (this.stopped || this.connected) return;
      this.clearRetry();

      let socket;
      try {
        // The credential travels in the subprotocol header because a browser's
        // WebSocket API cannot set Authorization. This is not a shortcut; it is
        // the only header-shaped channel the platform offers a client.
        socket = new WebSocket(this.url(), ['hub.v1', `hub.credential.${this.credential}`]);
      } catch (err) {
        this.onStatus('error', String(err));
        this.scheduleRetry();
        return;
      }

      this.socket = socket;
      this.onStatus('connecting');

      socket.onopen = () => {
        this.attempt = 0;
        this.onStatus('online');
        this.startPing();
      };

      socket.onmessage = (msg) => this.receive(msg.data);

      socket.onclose = (ev) => {
        this.stopPing();
        this.socket = null;
        // 1008 is policy violation: the hub rejected this credential, and
        // retrying with the same one will fail forever. Anything else is worth
        // retrying.
        if (ev.code === 1008) {
          this.onStatus('rejected', ev.reason || 'the hub rejected this credential');
          this.stopped = true;
          return;
        }
        this.onStatus('offline', ev.reason || `closed (${ev.code})`);
        this.scheduleRetry();
      };

      socket.onerror = () => {
        // onerror is always followed by onclose, which does the retry. Doing it
        // here as well would double the reconnect rate.
        this.onStatus('error');
      };
    }

    stop(reason) {
      this.stopped = true;
      this.clearRetry();
      this.stopPing();
      if (this.socket) {
        try { this.socket.close(1000, reason || 'agent stopping'); } catch { /* already gone */ }
        this.socket = null;
      }
    }

    /** send delivers an envelope, dropping it if the channel is down. */
    send(envelope) {
      if (!this.connected) return false;
      if (!protocol.valid(envelope)) {
        console.warn('hub: refusing to send an invalid envelope', envelope);
        return false;
      }
      try {
        this.socket.send(JSON.stringify(envelope));
        return true;
      } catch (err) {
        console.warn('hub: send failed', err);
        return false;
      }
    }

    emit(sessionId, op, body, seq) {
      return this.send(protocol.event(sessionId, op, body, seq));
    }

    async receive(data) {
      let env;
      try {
        env = JSON.parse(data);
      } catch {
        return;
      }
      if (!env || env.t !== protocol.CMD) return;

      try {
        const body = await this.onCommand(env);
        this.send(protocol.result(env.id, env.sid, body ?? {}));
      } catch (err) {
        const code = (err && err.hubCode) || protocol.codes.internal;
        this.send(protocol.failure(env.id, env.sid, code, (err && err.message) || err));
      }
    }

    startPing() {
      this.stopPing();
      this.pingTimer = setInterval(() => {
        if (!this.connected) return;
        // A WebSocket client cannot send a protocol-level ping frame from
        // JavaScript, so this is an application-level keepalive. It serves the
        // same two purposes: proving liveness, and being the traffic that keeps
        // the service worker from being evicted.
        this.emit('', 'ping', { at: Date.now() });
      }, this.pingMs);
    }

    stopPing() {
      if (this.pingTimer) {
        clearInterval(this.pingTimer);
        this.pingTimer = null;
      }
    }

    scheduleRetry() {
      if (this.stopped || this.retryTimer) return;
      this.attempt += 1;
      const base = Math.min(BACKOFF_MAX_MS, BACKOFF_MIN_MS * 2 ** (this.attempt - 1));
      // Jitter, so a hub restarting does not get every agent it serves back in
      // the same millisecond.
      const delay = base / 2 + Math.random() * (base / 2);
      this.retryTimer = setTimeout(() => {
        this.retryTimer = null;
        this.connect();
      }, delay);
    }

    clearRetry() {
      if (this.retryTimer) {
        clearTimeout(this.retryTimer);
        this.retryTimer = null;
      }
    }
  }

  return Channel;
})();
