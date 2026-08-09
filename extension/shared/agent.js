// Agent core: the orchestrator.
//
// This is the piece a host adapter drives. It owns enrollment, the channel, and
// command routing; the adapter supplies a small object describing how *this*
// browser opens tabs, reaches a page, and stores things.
//
// The split is the reason a WebView host or a second browser is a few hundred
// lines rather than a fork.

var Hub = self.Hub || (self.Hub = {});

Hub.Agent = (function () {
  const protocol = Hub.protocol;

  class Agent {
    /**
     * @param {object} host the platform adapter
     * @param {function} host.storageGet    (keys) => Promise<object>
     * @param {function} host.storageSet    (values) => Promise<void>
     * @param {function} host.openTab       ({url, active}) => Promise<{tabId, url, title}>
     * @param {function} host.closeTab      ({tabId, origin}) => Promise<void>
     * @param {function} host.callTab       (tabId, message) => Promise<any>
     * @param {function} host.capabilities  () => object
     * @param {function} [host.screenshot]  (tabId) => Promise<{dataUrl}>
     * @param {function} [host.cookies]     (params) => Promise<any>
     * @param {function} [host.describe]    () => {browser, browserVersion, platform, userAgent}
     * @param {function} [host.onStatus]    (state, detail) => void
     */
    constructor(host) {
      this.host = host;
      this.channel = null;
      this.hubURL = '';
      this.credential = '';
      this.agentId = '';

      // sessionId -> tabId, and the reverse. Two maps because both directions
      // are hot: commands arrive addressed by session, and tab events arrive
      // addressed by tab.
      this.sessions = new Map();
      this.tabs = new Map();

      // A short log the popup shows, so the person whose browser this is can
      // see what was done in it. Consent is granted once at enrollment; this is
      // what keeps that grant honest afterwards.
      this.log = [];
    }

    // ------------------------------------------------------------ lifecycle

    async load() {
      const stored = await this.host.storageGet(['hubURL', 'credential', 'agentId', 'sessions']);
      this.hubURL = stored.hubURL || '';
      this.credential = stored.credential || '';
      this.agentId = stored.agentId || '';
      for (const [sessionId, tabId] of Object.entries(stored.sessions || {})) {
        this.sessions.set(sessionId, tabId);
        this.tabs.set(tabId, sessionId);
      }
      return Boolean(this.hubURL && this.credential);
    }

    async persistSessions() {
      await this.host.storageSet({ sessions: Object.fromEntries(this.sessions) });
    }

    /**
     * enroll spends a one-time token for this agent's own credential.
     *
     * The token is never stored. It is worthless after this call, and keeping
     * it would only create something to steal.
     */
    async enroll(hubURL, token, name) {
      const base = String(hubURL).replace(/\/+$/, '');
      const describe = this.host.describe ? this.host.describe() : {};

      const res = await fetch(`${base}/agent/v1/enroll`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          token,
          name: name || describe.browser || 'browser agent',
          capabilities: this.host.capabilities(),
          ...describe,
        }),
      });

      const text = await res.text();
      if (!res.ok) {
        let message = text;
        try { message = JSON.parse(text).error ?? text; } catch { /* not JSON */ }
        throw new Error(message || `enrollment failed (${res.status})`);
      }

      const out = JSON.parse(text);
      this.hubURL = base;
      this.credential = out.credential;
      this.agentId = out.agentId;
      await this.host.storageSet({ hubURL: base, credential: out.credential, agentId: out.agentId });
      this.note('enrolled', `as ${out.agentId}`);
      return out;
    }

    /** forget removes every trace of this hub. The popup's kill switch. */
    async forget() {
      this.disconnect('the operator disconnected this agent');
      this.hubURL = '';
      this.credential = '';
      this.agentId = '';
      this.sessions.clear();
      this.tabs.clear();
      await this.host.storageSet({ hubURL: '', credential: '', agentId: '', sessions: {} });
      this.note('disconnected', 'all hub credentials erased');
    }

    connect() {
      if (!this.hubURL || !this.credential) return false;
      if (this.channel) this.channel.stop('reconnecting');

      this.channel = new Hub.Channel({
        hubURL: this.hubURL,
        credential: this.credential,
        onCommand: (env) => this.dispatch(env),
        onStatus: (state, detail) => {
          if (this.host.onStatus) this.host.onStatus(state, detail);
          if (state === 'rejected') this.note('rejected', detail);
        },
      });
      this.channel.connect();
      return true;
    }

    disconnect(reason) {
      if (this.channel) {
        this.channel.stop(reason);
        this.channel = null;
      }
    }

    get connected() {
      return Boolean(this.channel && this.channel.connected);
    }

    // -------------------------------------------------------------- routing

    /** dispatch routes one command, returning its result body. */
    async dispatch(env) {
      const params = env.body || {};
      this.note(env.op, env.sid || '');

      switch (env.op) {
        case 'openTab': {
          const tab = await this.host.openTab({ url: params.url, active: params.active });
          this.bind(params.sessionId, tab.tabId);
          await this.persistSessions();
          return { tabId: tab.tabId, url: tab.url, title: tab.title };
        }

        case 'closeTab': {
          const tabId = this.sessions.get(env.sid);
          if (tabId === undefined) return { closed: false, reason: 'no such session' };
          await this.host.closeTab({ tabId, origin: params.origin });
          this.unbind(env.sid);
          await this.persistSessions();
          return { closed: true };
        }

        case 'screenshot': {
          if (!this.host.screenshot) {
            throw this.fail(protocol.codes.unsupported, 'this agent cannot take screenshots');
          }
          return this.host.screenshot(this.tabFor(env.sid));
        }

        case 'cookies': {
          if (!this.host.cookies) {
            throw this.fail(protocol.codes.unsupported, 'this agent cannot read cookies');
          }
          return this.host.cookies({ ...params, tabId: this.tabFor(env.sid) });
        }

        case 'close': {
          const tabId = this.tabFor(env.sid);
          await this.host.closeTab({ tabId, origin: params.origin });
          this.unbind(env.sid);
          await this.persistSessions();
          return { closed: true };
        }

        default: {
          // Everything else is page work, so it goes to the content script.
          const tabId = this.tabFor(env.sid);
          return this.host.callTab(tabId, { kind: 'command', op: env.op, params });
        }
      }
    }

    tabFor(sessionId) {
      const tabId = this.sessions.get(sessionId);
      if (tabId === undefined) {
        throw this.fail(protocol.codes.noSuchTab, `session ${sessionId} is not attached to a tab`);
      }
      return tabId;
    }

    bind(sessionId, tabId) {
      if (!sessionId) return;
      // A tab can only serve one session; rebinding drops the old one rather
      // than leaving two sessions pointing at the same page.
      const previous = this.tabs.get(tabId);
      if (previous && previous !== sessionId) this.sessions.delete(previous);
      this.sessions.set(sessionId, tabId);
      this.tabs.set(tabId, sessionId);
    }

    unbind(sessionId) {
      const tabId = this.sessions.get(sessionId);
      this.sessions.delete(sessionId);
      if (tabId !== undefined) this.tabs.delete(tabId);
    }

    sessionFor(tabId) {
      return this.tabs.get(tabId);
    }

    // --------------------------------------------------------------- events

    emit(sessionId, op, body, seq) {
      if (!this.channel) return false;
      return this.channel.emit(sessionId, op, body, seq);
    }

    /** attach puts a tab the user already had open under control. */
    async attach(tabId, info) {
      const sessionId = `s_${randomId()}`;
      this.bind(sessionId, tabId);
      await this.persistSessions();
      this.emit(sessionId, 'session.attached', {
        sessionId,
        tabId,
        url: info?.url || '',
        title: info?.title || '',
      });
      this.note('attached', info?.url || String(tabId));
      return sessionId;
    }

    async detach(tabId) {
      const sessionId = this.sessionFor(tabId);
      if (!sessionId) return;
      this.emit(sessionId, 'session.closed', { sessionId, tabId });
      this.unbind(sessionId);
      await this.persistSessions();
      this.note('detached', String(tabId));
    }

    fail(code, message) {
      const err = new Error(message);
      err.hubCode = code;
      return err;
    }

    note(what, detail) {
      this.log.unshift({ at: Date.now(), what, detail: String(detail || '') });
      if (this.log.length > 100) this.log.pop();
    }
  }

  // A session id proposed by an agent is provisional: the hub mints the real
  // one for managed tabs. For attached tabs the agent proposes, because it is
  // the only side that knows the tab exists.
  function randomId() {
    const bytes = crypto.getRandomValues(new Uint8Array(16));
    return [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('').toUpperCase().slice(0, 26);
  }

  return Agent;
})();
