// The agent popup.
//
// Deliberately plain: it reads state from the service worker and renders it.
// Every action here is one the person whose browser this is might need in a
// hurry -- attach, detach, disconnect -- so none of them are more than a click
// deep.

const $ = (sel) => document.querySelector(sel);

const send = (message) => chrome.runtime.sendMessage(message);

function row(name) {
  return document.querySelector(`template[data-row="${name}"]`).content.firstElementChild.cloneNode(true);
}

async function refresh() {
  let state;
  try {
    state = await send({ kind: 'status' });
  } catch {
    return;
  }
  if (!state) return;

  const status = $('output[name="status"]');
  status.textContent = state.status;
  status.dataset.status = state.status;
  $('output[name="detail"]').textContent = state.detail || '';

  $('section[data-view="enroll"]').hidden = state.enrolled;
  $('section[data-view="connected"]').hidden = !state.enrolled;
  if (!state.enrolled) return;

  $('output[name="hub"]').textContent = state.hubURL;
  $('output[name="agent"]').textContent = state.agentId;
  $('select[name="captureMode"]').value = state.captureMode;
  $('input[name="evalEnabled"]').checked = state.evalEnabled;

  const sessions = $('ul[data-rows="sessions"]');
  sessions.replaceChildren();
  if (!state.sessions.length) {
    const li = document.createElement('li');
    li.textContent = 'No tabs are under control.';
    sessions.append(li);
  }
  for (const session of state.sessions) {
    const li = row('session');
    li.querySelector('span').textContent = `tab ${session.tabId}`;
    li.querySelector('button').addEventListener('click', async () => {
      await send({ kind: 'detach', tabId: session.tabId });
      refresh();
    });
    sessions.append(li);
  }

  const log = $('ol[data-rows="log"]');
  log.replaceChildren();
  for (const entry of state.log) {
    const li = row('log');
    const time = li.querySelector('time');
    time.textContent = new Date(entry.at).toLocaleTimeString();
    time.dateTime = new Date(entry.at).toISOString();
    li.querySelector('strong').textContent = entry.what;
    li.querySelector('code').textContent = entry.detail;
    log.append(li);
  }
}

$('form[name="enroll"]').addEventListener('submit', async (e) => {
  e.preventDefault();
  const data = new FormData(e.target);
  const error = $('output[name="error"]');
  error.textContent = '';

  const button = e.target.querySelector('button[type="submit"]');
  button.disabled = true;
  try {
    const res = await send({
      kind: 'enroll',
      hubURL: data.get('hubURL'),
      token: String(data.get('token')).trim(),
      name: data.get('name'),
    });
    if (res && res.error) throw new Error(res.error.message);
    await refresh();
  } catch (err) {
    error.textContent = err.message;
  } finally {
    button.disabled = false;
  }
});

$('button[name="attach"]').addEventListener('click', async () => {
  await send({ kind: 'attach' });
  refresh();
});

$('button[name="disconnect"]').addEventListener('click', async () => {
  if (!confirm('Disconnect from the hub and erase this browser\'s credential?')) return;
  await send({ kind: 'disconnect' });
  refresh();
});

$('form[name="settings"]').addEventListener('change', async () => {
  await send({
    kind: 'settings',
    captureMode: $('select[name="captureMode"]').value,
    evalEnabled: $('input[name="evalEnabled"]').checked,
  });
  refresh();
});

chrome.runtime.onMessage.addListener((message) => {
  if (message && message.kind === 'status-changed') refresh();
});

refresh();
setInterval(refresh, 2000);
