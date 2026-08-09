// The Operator Console.
//
// This file talks to /v1/... and nothing else. There is no private console API,
// which is the constraint that keeps the Control API honest: anything the
// console can do here, a scraper can do with a bearer token.
//
// It clones <template> elements rather than building HTML from strings. That is
// not stylistic -- page titles and URLs here come from whatever site an agent
// visited, and textContent on a cloned node cannot become markup no matter what
// the page was called.

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

const status = $('output[name="status"]');
const say = (msg) => { status.textContent = msg; };

// ------------------------------------------------------------------ fetch

async function api(path, { method = 'GET', body, raw = false } = {}) {
  const res = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });

  if (res.status === 401) {
    showLogin();
    throw new Error('signed out');
  }
  if (res.status === 204) return null;

  const text = await res.text();
  if (!res.ok) {
    let message = text;
    try { message = JSON.parse(text).error ?? text; } catch { /* not JSON */ }
    throw new Error(message || `${res.status} ${res.statusText}`);
  }
  if (raw) return text;
  return text ? JSON.parse(text) : null;
}

// --------------------------------------------------------------- rendering

// row clones a <template data-row="name"> and returns its single element.
function row(name) {
  const tpl = $(`template[data-row="${name}"]`);
  return tpl.content.firstElementChild.cloneNode(true);
}

const cell = (root, name) => $(`[data-cell="${name}"]`, root);

function setText(root, name, value) {
  const el = cell(root, name);
  if (el) el.textContent = value ?? '';
  return el;
}

function fill(tbody, rows, build) {
  tbody.replaceChildren();
  for (const item of rows) tbody.append(build(item));
}

const relative = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });

function ago(iso) {
  if (!iso) return '—';
  const then = new Date(iso);
  if (Number.isNaN(then.getTime()) || then.getFullYear() < 2000) return '—';
  const seconds = Math.round((then - Date.now()) / 1000);
  const units = [['day', 86400], ['hour', 3600], ['minute', 60], ['second', 1]];
  for (const [unit, size] of units) {
    if (Math.abs(seconds) >= size || unit === 'second') {
      return relative.format(Math.round(seconds / size), unit);
    }
  }
  return '—';
}

function bytes(n) {
  if (!n) return '—';
  const units = ['B', 'kB', 'MB', 'GB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n < 10 && i > 0 ? n.toFixed(1) : Math.round(n)} ${units[i]}`;
}

// ------------------------------------------------------------------ agents

async function loadAgents() {
  const { agents } = await api('/v1/agents');
  const tbody = $('tbody[data-rows="agents"]');

  fill(tbody, agents, (a) => {
    const tr = row('agent');
    setText(tr, 'name', a.name || 'unnamed agent');
    setText(tr, 'id', a.id);
    setText(tr, 'browser', [a.browser, a.browserVersion].filter(Boolean).join(' ') || '—');
    setText(tr, 'labels', Object.entries(a.labels ?? {}).map(([k, v]) => `${k}=${v}`).join(' ') || '—');

    const st = setText(tr, 'status', a.status);
    st.dataset.status = a.status;

    // The capture mode and its gaps travel together. An agent that cannot see
    // navigations says so here, before anyone trusts its request log.
    const mode = setText(tr, 'mode', a.capabilities?.capture ?? 'none');
    mode.dataset.fidelity = a.fullFidelityCapture ? 'full' : 'partial';
    const gaps = cell(tr, 'gaps');
    gaps.replaceChildren();
    for (const gap of a.captureGaps ?? []) {
      const li = document.createElement('li');
      li.textContent = gap;
      gaps.append(li);
    }

    const seen = cell(tr, 'lastSeen');
    seen.textContent = ago(a.lastSeenAt);
    if (a.lastSeenAt) seen.dateTime = a.lastSeenAt;

    $('form[name="open-session"]', tr).addEventListener('submit', async (e) => {
      e.preventDefault();
      const url = new FormData(e.target).get('url');
      await guard(e.target, async () => {
        const sess = await api(`/v1/agents/${a.id}/sessions`, { method: 'POST', body: { url, active: true } });
        say(`Opened ${sess.id}`);
        await loadSessions();
        show('sessions');
      });
    });

    $('form[name="revoke-agent"]', tr).addEventListener('submit', async (e) => {
      e.preventDefault();
      if (!confirm(`Revoke ${a.name || a.id}? Its credential stops working immediately.`)) return;
      await guard(e.target, async () => {
        await api(`/v1/agents/${a.id}`, { method: 'DELETE' });
        say('Agent revoked');
        await loadAgents();
      });
    });

    return tr;
  });
}

$('form[name="enroll"]').addEventListener('submit', async (e) => {
  e.preventDefault();
  const raw = new FormData(e.target).get('labels') ?? '';
  const labels = {};
  for (const pair of String(raw).split(',')) {
    const [k, v] = pair.split('=').map((s) => s.trim());
    if (k && v) labels[k] = v;
  }

  await guard(e.target, async () => {
    const out = await api('/v1/enrollments', { method: 'POST', body: { labels, ttlSeconds: 3600 } });
    const dialog = $('dialog[data-dialog="token"]');
    $('output[name="token"]', dialog).textContent = out.token;
    const expires = $('time[name="expires"]', dialog);
    expires.textContent = new Date(out.expiresAt).toLocaleString();
    expires.dateTime = out.expiresAt;
    dialog.showModal();
  });
});

// ---------------------------------------------------------------- sessions

async function loadSessions() {
  const { sessions } = await api('/v1/sessions');
  const tbody = $('tbody[data-rows="sessions"]');

  fill(tbody, sessions, (s) => {
    const tr = row('session');
    setText(tr, 'id', s.id);
    setText(tr, 'title', s.title || '—');
    setText(tr, 'url', s.url || '');

    const origin = setText(tr, 'origin', s.origin);
    origin.dataset.origin = s.origin;
    const state = setText(tr, 'state', s.state);
    state.dataset.state = s.state;

    $('[data-action="inspect"]', tr).addEventListener('click', () => inspect(s));
    $('[data-action="close"]', tr).addEventListener('click', async () => {
      const warning = s.origin === 'attached'
        ? 'This tab was handed over by a person who was using it. Close it anyway?'
        : 'Close this session?';
      if (!confirm(warning)) return;
      await api(`/v1/sessions/${s.id}`, { method: 'DELETE' });
      say('Session closed');
      await loadSessions();
    });

    return tr;
  });
}

// --------------------------------------------------------------- inspector

let stream = null;
let current = null;

function inspect(session) {
  current = session;
  const dialog = $('dialog[data-dialog="inspect"]');

  $('output[name="session-title"]').textContent = session.title || session.id;
  $('output[name="session-url"]').textContent = session.url || '';
  $('[data-action="har"]').href = `/v1/sessions/${session.id}/har?bodies=1`;
  $('[data-action="har"]').download = `${session.id}.har`;
  $('output[name="result"]').textContent = '';
  $('output[name="dom"]').textContent = '';

  dialog.showModal();
  showTab('steer');
  listen(session.id);

  dialog.addEventListener('close', () => {
    if (stream) { stream.close(); stream = null; }
    current = null;
  }, { once: true });
}

function listen(sessionID) {
  if (stream) stream.close();
  const list = $('ol[data-rows="events"]');
  list.replaceChildren();

  // EventSource speaks SSE and nothing else, which is why the API offers both
  // SSE and NDJSON: this is the browser's half.
  stream = new EventSource(`/v1/sessions/${sessionID}/events`, { withCredentials: true });
  stream.onmessage = (msg) => {
    let event;
    try { event = JSON.parse(msg.data); } catch { return; }

    const li = row('event');
    const at = cell(li, 'at');
    at.textContent = new Date(event.at).toLocaleTimeString();
    at.dateTime = event.at;
    setText(li, 'type', event.type);
    setText(li, 'body', event.body ? JSON.stringify(event.body).slice(0, 200) : '');
    list.prepend(li);

    while (list.childElementCount > 300) list.lastElementChild.remove();
  };
  stream.onerror = () => say('Event stream interrupted; the browser will retry.');
}

$('form[name="command"]').addEventListener('submit', async (e) => {
  e.preventDefault();
  if (!current) return;

  const data = new FormData(e.target);
  let params;
  try {
    params = JSON.parse(String(data.get('params') || '{}'));
  } catch (err) {
    $('output[name="result"]').textContent = `Parameters are not valid JSON: ${err.message}`;
    return;
  }

  await guard(e.target, async () => {
    try {
      const cmd = await api(`/v1/sessions/${current.id}/commands?wait=30s`, {
        method: 'POST',
        body: { op: data.get('op'), params },
      });
      $('output[name="result"]').textContent = JSON.stringify(cmd, null, 2);
    } catch (err) {
      $('output[name="result"]').textContent = err.message;
    }
  });
});

async function loadDOM(fresh) {
  if (!current) return;
  const out = $('output[name="dom"]');
  out.textContent = 'Reading…';
  try {
    const html = await api(`/v1/sessions/${current.id}/dom?format=html${fresh ? '&fresh=1' : ''}`, { raw: true });
    out.textContent = html;
    setText(document, 'seq', fresh ? 'Re-snapshotted just now.' : '');
  } catch (err) {
    out.textContent = err.message;
  }
}

$('[data-action="dom-refresh"]').addEventListener('click', () => loadDOM(false));
$('[data-action="dom-fresh"]').addEventListener('click', () => loadDOM(true));
$('[data-action="requests-refresh"]').addEventListener('click', loadRequests);

async function loadRequests() {
  if (!current) return;
  const { exchanges, captureGaps } = await api(`/v1/sessions/${current.id}/requests`);

  const gapList = $('ul[data-rows="capture-gaps"]');
  gapList.replaceChildren();
  gapList.hidden = !captureGaps?.length;
  for (const gap of captureGaps ?? []) {
    const li = document.createElement('li');
    li.textContent = gap;
    gapList.append(li);
  }

  fill($('tbody[data-rows="requests"]'), exchanges, (x) => {
    const tr = row('request');
    setText(tr, 'method', x.method);
    setText(tr, 'url', x.url);
    setText(tr, 'mime', x.mimeType || '—');
    setText(tr, 'size', bytes(x.responseBodySize));

    const st = setText(tr, 'status', x.status || '—');
    if (x.status) st.dataset.statusClass = String(Math.floor(x.status / 100));

    const duration = cell(tr, 'duration');
    duration.textContent = `${x.durationMs ?? 0} ms`;

    const headers = cell(tr, 'headers');
    headers.replaceChildren();
    for (const [k, v] of Object.entries(x.responseHeaders ?? {})) {
      const dt = document.createElement('dt');
      dt.textContent = k;
      const dd = document.createElement('dd');
      dd.textContent = v;
      headers.append(dt, dd);
    }

    const artifact = cell(tr, 'artifact');
    artifact.replaceChildren();
    if (x.responseBodyDigest) {
      const a = document.createElement('a');
      a.href = `/v1/artifacts/${x.responseBodyDigest}`;
      a.textContent = `body ${x.responseBodyDigest.slice(0, 12)}…`;
      a.download = '';
      artifact.append(a);
    }

    return tr;
  });
}

// -------------------------------------------------------------------- keys

async function loadKeys() {
  const { keys } = await api('/v1/apikeys');
  fill($('tbody[data-rows="keys"]'), keys, (k) => {
    const tr = row('key');
    setText(tr, 'name', k.name || '—');
    setText(tr, 'scope', k.scope);
    cell(tr, 'created').textContent = ago(k.createdAt);
    cell(tr, 'lastUsed').textContent = k.lastUsedAt ? ago(k.lastUsedAt) : 'never';

    const form = $('form[name="revoke-key"]', tr);
    if (k.revokedAt) {
      form.replaceChildren(document.createTextNode('revoked'));
    } else {
      form.addEventListener('submit', async (e) => {
        e.preventDefault();
        if (!confirm(`Revoke ${k.name || k.id}?`)) return;
        await api(`/v1/apikeys/${k.id}`, { method: 'DELETE' });
        say('Key revoked');
        await loadKeys();
      });
    }
    return tr;
  });
}

$('form[name="mint-key"]').addEventListener('submit', async (e) => {
  e.preventDefault();
  const data = new FormData(e.target);
  await guard(e.target, async () => {
    const out = await api('/v1/apikeys', {
      method: 'POST',
      body: { name: data.get('name'), scope: data.get('scope') },
    });
    const dialog = $('dialog[data-dialog="key"]');
    $('output[name="key"]', dialog).textContent = out.token;
    dialog.showModal();
    await loadKeys();
  });
});

// ------------------------------------------------------------- navigation

function show(id) {
  for (const section of ['agents', 'sessions', 'keys']) {
    $(`#${section}`).hidden = section !== id;
  }
  for (const link of $$('header nav a')) {
    // aria-current is both the state and the styling hook.
    if (link.getAttribute('href') === `#${id}`) link.setAttribute('aria-current', 'page');
    else link.removeAttribute('aria-current');
  }
  if (id === 'sessions') loadSessions().catch((e) => say(e.message));
  if (id === 'keys') loadKeys().catch((e) => say(e.message));
  if (id === 'agents') loadAgents().catch((e) => say(e.message));
}

function showTab(id) {
  for (const tab of ['steer', 'dom', 'requests', 'events']) {
    $(`#${tab}`).hidden = tab !== id;
  }
  for (const link of $$('dialog nav a')) {
    if (link.getAttribute('href') === `#${id}`) link.setAttribute('aria-current', 'page');
    else link.removeAttribute('aria-current');
  }
  if (id === 'requests') loadRequests().catch((e) => say(e.message));
}

for (const link of $$('header nav a')) {
  link.addEventListener('click', (e) => { e.preventDefault(); show(link.getAttribute('href').slice(1)); });
}
for (const link of $$('dialog nav a')) {
  link.addEventListener('click', (e) => { e.preventDefault(); showTab(link.getAttribute('href').slice(1)); });
}

// guard disables a form while its request is in flight, so a double-click
// cannot mint two enrollment tokens or open two tabs.
async function guard(form, fn) {
  const button = $('button[type="submit"]', form);
  if (button) button.disabled = true;
  try {
    await fn();
  } catch (err) {
    say(err.message);
  } finally {
    if (button) button.disabled = false;
  }
}

// ------------------------------------------------------------------ login

function showLogin(loginEnabled = true) {
  $('main[data-view="login"]').hidden = false;
  $('main[data-view="console"]').hidden = true;
  $('form[name="logout"]').hidden = true;
  $('aside[data-note="disabled"]').hidden = loginEnabled;
  $('form[name="login"]').hidden = !loginEnabled;
}

function showConsole(user) {
  $('main[data-view="login"]').hidden = true;
  $('main[data-view="console"]').hidden = false;
  const logout = $('form[name="logout"]');
  logout.hidden = false;
  $('output[name="user"]', logout).textContent = user;
}

$('form[name="login"]').addEventListener('submit', async (e) => {
  e.preventDefault();
  const data = new FormData(e.target);
  const error = $('output[name="error"]');
  error.textContent = '';

  const res = await fetch('/console/login', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ user: data.get('user'), password: data.get('password') }),
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    error.textContent = body.error ?? 'Sign in failed';
    return;
  }
  await start();
});

$('form[name="logout"]').addEventListener('submit', async (e) => {
  e.preventDefault();
  await fetch('/console/logout', { method: 'POST', credentials: 'same-origin' });
  location.reload();
});

// ------------------------------------------------------------------- boot

async function start() {
  const session = await (await fetch('/console/session', { credentials: 'same-origin' })).json();
  if (!session.authenticated) {
    showLogin(session.loginEnabled);
    return;
  }
  showConsole(session.user);

  const meta = await api('/v1/meta');
  $('output[name="version"]').textContent = meta.version;
  $('output[name="online"]').textContent = meta.agentsOnline;

  // The command vocabulary comes from the hub, so the console cannot drift out
  // of step with what this build actually supports.
  const ops = $('select[name="op"]');
  ops.replaceChildren();
  for (const op of meta.commands) {
    const option = document.createElement('option');
    option.value = op;
    option.textContent = op;
    ops.append(option);
  }

  show('agents');
  setInterval(() => {
    if (!$('#agents').hidden) loadAgents().catch(() => {});
  }, 5000);
}

start().catch((err) => say(err.message));
