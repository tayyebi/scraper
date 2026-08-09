// Firefox host adapter.
//
// Structurally the same as the Chrome one -- both drive the shared agent core --
// and different in exactly the two places the platforms differ: capture is
// full-fidelity here via filterResponseData, and the background page is
// persistent, so none of Chrome's service-worker eviction machinery is needed.

const api = typeof browser !== 'undefined' ? browser : chrome;

const host = {
  storageGet: (keys) => api.storage.local.get(keys),
  storageSet: (values) => api.storage.local.set(values),

  describe() {
    const ua = navigator.userAgent;
    const match = ua.match(/Firefox\/([\d.]+)/);
    const android = /Android/i.test(ua);
    return {
      browser: android ? 'firefox-android' : 'firefox',
      browserVersion: match ? match[1] : '',
      platform: android ? 'android' : (navigator.platform || ''),
      userAgent: ua,
      agentVersion: api.runtime.getManifest().version,
    };
  },

  capabilities() {
    return Hub.capabilities.build({
      capture: Hub.capabilities.CAPTURE.filterResponse,
      eval: state.evalEnabled,
      // captureVisibleTab does not exist on Firefox for Android, so the claim
      // follows what this build can actually do rather than what desktop can.
      screenshot: Boolean(api.tabs.captureVisibleTab),
      cookies: true,
      attach: true,
      openTabs: true,
      mirror: true,
    });
  },

  async openTab({ url, active }) {
    const tab = await api.tabs.create({ url: url || 'about:blank', active: Boolean(active) });
    await waitForTabLoad(tab.id);
    const loaded = await api.tabs.get(tab.id);
    return { tabId: tab.id, url: loaded.url || url || '', title: loaded.title || '' };
  },

  async closeTab({ tabId, origin }) {
    // An attached tab was somebody's, and stays theirs.
    if (origin === 'attached') {
      await agent.detach(tabId);
      await notifyTab(tabId, { kind: 'detach' });
      return;
    }
    try {
      await api.tabs.remove(tabId);
    } catch {
      // Already closed.
    }
  },

  async callTab(tabId, message) {
    try {
      const res = await api.tabs.sendMessage(tabId, message);
      if (res && res.error) {
        const err = new Error(res.error.message);
        err.hubCode = res.error.code;
        throw err;
      }
      return res ? res.body : {};
    } catch (err) {
      if (err.hubCode) throw err;
      const wrapped = new Error(`the page did not answer: ${err.message}`);
      wrapped.hubCode = Hub.protocol.codes.detached;
      throw wrapped;
    }
  },

  async screenshot(tabId) {
    if (!api.tabs.captureVisibleTab) {
      const err = new Error('Firefox for Android cannot capture tab images');
      err.hubCode = Hub.protocol.codes.unsupported;
      throw err;
    }
    const tab = await api.tabs.get(tabId);
    const dataUrl = await api.tabs.captureVisibleTab(tab.windowId, { format: 'png' });
    return { dataUrl, format: 'png' };
  },

  async cookies({ url, tabId }) {
    let target = url;
    if (!target) {
      const tab = await api.tabs.get(tabId);
      target = tab.url;
    }
    const cookies = await api.cookies.getAll({ url: target });
    return { cookies };
  },

  onStatus(status, detail) {
    state.status = status;
    state.detail = detail || '';
    // Firefox for Android has no badge, so this is guarded rather than assumed.
    if (api.browserAction && api.browserAction.setBadgeText) {
      api.browserAction.setBadgeText({ text: status === 'online' ? '●' : '' });
    }
    api.runtime.sendMessage({ kind: 'status-changed' }).catch(() => {});
  },
};

const state = {
  status: 'offline',
  detail: '',
  evalEnabled: false,
};

const agent = new Hub.Agent(host);

// ------------------------------------------------------------- capture

Hub.firefoxCapture.install({
  isTracked: (tabId) => tabId >= 0 && agent.sessionFor(tabId) !== undefined,
  onExchange: async (tabId, record, bytes) => {
    const sessionId = agent.sessionFor(tabId);
    if (!sessionId) return;

    let digest = '';
    if (bytes && bytes.byteLength) {
      const stored = await Hub.netlog.upload(agent.hubURL, agent.credential, bytes);
      if (stored) digest = stored.digest;
    }
    agent.emit(sessionId, 'exchange', Hub.netlog.exchange({
      ...record,
      responseBodyDigest: digest,
    }));
  },
});

// ----------------------------------------------------------- lifecycle

async function boot() {
  const settings = await api.storage.local.get(['evalEnabled']);
  state.evalEnabled = Boolean(settings.evalEnabled);

  const ready = await agent.load();
  if (ready) agent.connect();
}

boot();

function waitForTabLoad(tabId, timeoutMs = 30_000) {
  return new Promise((resolve) => {
    const done = () => {
      api.tabs.onUpdated.removeListener(listener);
      clearTimeout(timer);
      resolve();
    };
    const listener = (id, info) => {
      if (id === tabId && info.status === 'complete') done();
    };
    const timer = setTimeout(done, timeoutMs);
    api.tabs.onUpdated.addListener(listener);
  });
}

async function notifyTab(tabId, message) {
  try {
    await api.tabs.sendMessage(tabId, message);
  } catch {
    // No content script there.
  }
}

api.tabs.onRemoved.addListener(async (tabId) => {
  const sessionId = agent.sessionFor(tabId);
  if (!sessionId) return;
  agent.emit(sessionId, 'session.closed', { sessionId, tabId });
  agent.unbind(sessionId);
  await agent.persistSessions();
});

api.tabs.onUpdated.addListener((tabId, info, tab) => {
  const sessionId = agent.sessionFor(tabId);
  if (!sessionId) return;
  if (info.status === 'complete' || info.url) {
    agent.emit(sessionId, 'navigated', { url: tab.url || '', title: tab.title || '' });
  }
});

// ------------------------------------------------------------ messages

api.runtime.onMessage.addListener((message, sender) => handleMessage(message, sender)
  .catch((err) => ({ error: { code: err.hubCode || 'internal', message: err.message } })));

async function handleMessage(message, sender) {
  const tabId = sender.tab ? sender.tab.id : undefined;

  switch (message.kind) {
    case 'session-of':
      return { sessionId: tabId === undefined ? null : agent.sessionFor(tabId) || null };

    case 'mirror': {
      const sessionId = agent.sessionFor(tabId);
      if (!sessionId) return { ok: false };
      agent.emit(sessionId, message.op, message.body, message.seq);
      return { ok: true };
    }

    case 'detach-self': {
      if (tabId === undefined) return { ok: false };
      await agent.detach(tabId);
      return { ok: true };
    }

    case 'status':
      return {
        status: state.status,
        detail: state.detail,
        hubURL: agent.hubURL,
        agentId: agent.agentId,
        enrolled: Boolean(agent.credential),
        captureMode: Hub.capabilities.CAPTURE.filterResponse,
        evalEnabled: state.evalEnabled,
        sessions: [...agent.sessions.entries()].map(([id, tab]) => ({ sessionId: id, tabId: tab })),
        log: agent.log.slice(0, 40),
      };

    case 'enroll':
      await agent.enroll(message.hubURL, message.token, message.name);
      agent.connect();
      return { ok: true, agentId: agent.agentId };

    case 'disconnect':
      await agent.forget();
      return { ok: true };

    case 'attach': {
      const [tab] = await api.tabs.query({ active: true, currentWindow: true });
      if (!tab) throw new Error('no active tab');
      const sessionId = await agent.attach(tab.id, tab);
      await notifyTab(tab.id, { kind: 'attached', sessionId });
      return { ok: true, sessionId };
    }

    case 'detach':
      await agent.detach(message.tabId);
      await notifyTab(message.tabId, { kind: 'detach' });
      return { ok: true };

    case 'settings':
      if (typeof message.evalEnabled === 'boolean') state.evalEnabled = message.evalEnabled;
      await api.storage.local.set({ evalEnabled: state.evalEnabled });
      return { ok: true };

    default:
      return { ok: false };
  }
}
