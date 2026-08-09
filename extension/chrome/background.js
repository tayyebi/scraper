// Chrome host adapter (MV3 service worker).
//
// Classic worker rather than a module, because importScripts is what lets the
// shared agent core load without a bundler -- and this project has no build
// step anywhere.

importScripts(
  'shared/protocol.js',
  'shared/channel.js',
  'shared/capabilities.js',
  'shared/netlog.js',
  'shared/agent.js',
);

// MV3 evicts an idle service worker after roughly 30 seconds. WebSocket traffic
// resets that timer (Chrome 116+), which is why the channel pings -- but if the
// socket is already down there is no traffic, and the worker dies before it can
// reconnect. This alarm is the backstop: it wakes the worker, which reconnects,
// which restores the traffic that keeps it alive.
const KEEPALIVE_ALARM = 'hub-keepalive';
const KEEPALIVE_MINUTES = 0.5;

const host = {
  storageGet: (keys) => chrome.storage.local.get(keys),
  storageSet: (values) => chrome.storage.local.set(values),

  describe() {
    const ua = navigator.userAgent;
    const match = ua.match(/Chrome\/([\d.]+)/);
    return {
      browser: 'chrome',
      browserVersion: match ? match[1] : '',
      platform: navigator.platform || '',
      userAgent: ua,
      agentVersion: chrome.runtime.getManifest().version,
    };
  },

  capabilities() {
    // Reported honestly: fetch-patch is what is on by default, and the hub
    // tells callers exactly what that mode cannot see. Claiming full fidelity
    // here would produce request logs that look complete and are not.
    return Hub.capabilities.build({
      capture: state.captureMode,
      eval: state.evalEnabled,
      screenshot: true,
      cookies: true,
      attach: true,
      openTabs: true,
      mirror: true,
    });
  },

  async openTab({ url, active }) {
    const tab = await chrome.tabs.create({ url: url || 'about:blank', active: Boolean(active) });
    await waitForTabLoad(tab.id);
    const loaded = await chrome.tabs.get(tab.id);
    return { tabId: tab.id, url: loaded.url || url || '', title: loaded.title || '' };
  },

  async closeTab({ tabId, origin }) {
    // An attached tab belonged to a person who was using it. Closing their
    // window because an automation finished is rude, so the agent detaches
    // instead and leaves the tab where it was.
    if (origin === 'attached') {
      await agent.detach(tabId);
      await notifyTab(tabId, { kind: 'detach' });
      return;
    }
    try {
      await chrome.tabs.remove(tabId);
    } catch {
      // Already gone. The end state is what was asked for.
    }
  },

  async callTab(tabId, message) {
    try {
      const res = await chrome.tabs.sendMessage(tabId, message);
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
    const tab = await chrome.tabs.get(tabId);
    const dataUrl = await chrome.tabs.captureVisibleTab(tab.windowId, { format: 'png' });
    return { dataUrl, format: 'png' };
  },

  async cookies({ url, tabId }) {
    let target = url;
    if (!target) {
      const tab = await chrome.tabs.get(tabId);
      target = tab.url;
    }
    const cookies = await chrome.cookies.getAll({ url: target });
    return { cookies };
  },

  onStatus(status, detail) {
    state.status = status;
    state.detail = detail || '';
    chrome.action.setBadgeText({ text: status === 'online' ? '●' : '' });
    chrome.action.setBadgeBackgroundColor({ color: '#2e7d32' });
    broadcast();
  },
};

const state = {
  status: 'offline',
  detail: '',
  captureMode: Hub.capabilities.CAPTURE.fetchPatch,
  evalEnabled: false,
};

const agent = new Hub.Agent(host);

// -------------------------------------------------------------- lifecycle

async function boot() {
  const settings = await chrome.storage.local.get(['captureMode', 'evalEnabled']);
  state.captureMode = settings.captureMode || Hub.capabilities.CAPTURE.fetchPatch;
  state.evalEnabled = Boolean(settings.evalEnabled);

  const ready = await agent.load();
  if (ready) agent.connect();

  chrome.alarms.create(KEEPALIVE_ALARM, { periodInMinutes: KEEPALIVE_MINUTES });
}

chrome.runtime.onInstalled.addListener(boot);
chrome.runtime.onStartup.addListener(boot);
boot();

chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name !== KEEPALIVE_ALARM) return;
  if (!agent.connected) agent.connect();
});

function waitForTabLoad(tabId, timeoutMs = 30_000) {
  return new Promise((resolve) => {
    const done = () => {
      chrome.tabs.onUpdated.removeListener(listener);
      clearTimeout(timer);
      resolve();
    };
    const listener = (id, info) => {
      if (id === tabId && info.status === 'complete') done();
    };
    const timer = setTimeout(done, timeoutMs);
    chrome.tabs.onUpdated.addListener(listener);
  });
}

async function notifyTab(tabId, message) {
  try {
    await chrome.tabs.sendMessage(tabId, message);
  } catch {
    // The tab is gone or has no content script. Nothing to tell.
  }
}

// ------------------------------------------------------- tab bookkeeping

chrome.tabs.onRemoved.addListener(async (tabId) => {
  const sessionId = agent.sessionFor(tabId);
  if (!sessionId) return;
  agent.emit(sessionId, 'session.closed', { sessionId, tabId });
  agent.unbind(sessionId);
  await agent.persistSessions();
});

chrome.tabs.onUpdated.addListener((tabId, info, tab) => {
  const sessionId = agent.sessionFor(tabId);
  if (!sessionId) return;
  if (info.status === 'complete' || info.url) {
    agent.emit(sessionId, 'navigated', { url: tab.url || '', title: tab.title || '' });
  }
});

// --------------------------------------------------- messages from pages

chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
  // Every branch answers asynchronously, so the listener returns true to keep
  // the message channel open.
  handleMessage(message, sender).then(sendResponse).catch((err) => {
    sendResponse({ error: { code: err.hubCode || 'internal', message: err.message } });
  });
  return true;
});

async function handleMessage(message, sender) {
  const tabId = sender.tab ? sender.tab.id : undefined;

  switch (message.kind) {
    // ----- from a content script -----
    case 'session-of': {
      return { sessionId: tabId === undefined ? null : agent.sessionFor(tabId) || null };
    }

    case 'mirror': {
      const sessionId = agent.sessionFor(tabId);
      if (!sessionId) return { ok: false };
      agent.emit(sessionId, message.op, message.body, message.seq);
      return { ok: true };
    }

    case 'exchange': {
      const sessionId = agent.sessionFor(tabId);
      if (!sessionId) return { ok: false };
      await recordExchange(sessionId, message.record);
      return { ok: true };
    }

    // The Stop button on the in-page indicator. The person whose browser this
    // is must be able to end control from the page itself, not only from a
    // popup they have to know to open.
    case 'detach-self': {
      if (tabId === undefined) return { ok: false };
      await agent.detach(tabId);
      return { ok: true };
    }

    // ----- from the popup -----
    case 'status': {
      return {
        status: state.status,
        detail: state.detail,
        hubURL: agent.hubURL,
        agentId: agent.agentId,
        enrolled: Boolean(agent.credential),
        captureMode: state.captureMode,
        evalEnabled: state.evalEnabled,
        sessions: [...agent.sessions.entries()].map(([id, tab]) => ({ sessionId: id, tabId: tab })),
        log: agent.log.slice(0, 40),
      };
    }

    case 'enroll': {
      await agent.enroll(message.hubURL, message.token, message.name);
      agent.connect();
      return { ok: true, agentId: agent.agentId };
    }

    case 'disconnect': {
      await agent.forget();
      chrome.action.setBadgeText({ text: '' });
      return { ok: true };
    }

    case 'attach': {
      const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
      if (!tab) throw new Error('no active tab');
      const sessionId = await agent.attach(tab.id, tab);
      await notifyTab(tab.id, { kind: 'attached', sessionId });
      return { ok: true, sessionId };
    }

    case 'detach': {
      await agent.detach(message.tabId);
      await notifyTab(message.tabId, { kind: 'detach' });
      return { ok: true };
    }

    case 'settings': {
      if (message.captureMode) state.captureMode = message.captureMode;
      if (typeof message.evalEnabled === 'boolean') state.evalEnabled = message.evalEnabled;
      await chrome.storage.local.set({ captureMode: state.captureMode, evalEnabled: state.evalEnabled });
      return { ok: true };
    }

    default:
      return { ok: false };
  }
}

// recordExchange uploads bodies out of band and then reports the exchange.
//
// The upload happens here rather than in the content script because the service
// worker holds the credential; a page-adjacent context should never see it.
async function recordExchange(sessionId, record) {
  let responseDigest = '';
  let truncated = false;

  if (record.bodyBase64) {
    const bytes = base64ToBytes(record.bodyBase64);
    const stored = await Hub.netlog.upload(agent.hubURL, agent.credential, bytes);
    if (stored) {
      responseDigest = stored.digest;
      truncated = stored.truncated;
    }
  }

  agent.emit(sessionId, 'exchange', Hub.netlog.exchange({
    ...record,
    responseBodyDigest: responseDigest,
    truncated: truncated || record.truncated,
  }));
}

function base64ToBytes(b64) {
  const binary = atob(b64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

function broadcast() {
  // The popup may not be open; a failed send here is normal and not an error.
  chrome.runtime.sendMessage({ kind: 'status-changed' }).catch(() => {});
}
