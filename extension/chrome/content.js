// Chrome content script: the page-side half of the agent.
//
// Runs in the isolated world, so it can touch the DOM and talk to the service
// worker but cannot see the page's own JavaScript. inject.js runs in the MAIN
// world for the part that must.

(() => {
  const isTop = window.top === window;

  let sessionId = null;
  let mirror = null;
  let indicator = null;

  // ------------------------------------------------------------ indicator

  // A controlled tab always shows this. Enrollment is the one consent gesture,
  // and commands run unattended afterwards -- so the person whose browser this
  // is must always be able to see, at a glance, that this tab is one of them.
  // It is deliberately not dismissible from the page.
  function showIndicator() {
    if (indicator || !isTop || !document.documentElement) return;

    indicator = document.createElement('div');
    indicator.setAttribute('role', 'status');
    indicator.setAttribute('aria-live', 'polite');
    indicator.textContent = 'This tab is under automation control';
    indicator.style.cssText = [
      'position:fixed',
      'top:0',
      'left:0',
      'right:0',
      'z-index:2147483647',
      'padding:4px 10px',
      'font:12px/1.5 system-ui,sans-serif',
      'color:#fff',
      'background:#b26a00',
      'text-align:center',
      'pointer-events:none',
      'box-shadow:0 1px 4px rgba(0,0,0,.35)',
    ].join(';');

    const detach = document.createElement('button');
    detach.textContent = 'Stop';
    detach.style.cssText = 'margin-left:10px;pointer-events:auto;font:inherit;cursor:pointer';
    detach.addEventListener('click', () => {
      chrome.runtime.sendMessage({ kind: 'detach-self' }).catch(() => {});
      teardown();
    });
    indicator.append(detach);

    document.documentElement.append(indicator);
  }

  function hideIndicator() {
    if (indicator) {
      indicator.remove();
      indicator = null;
    }
  }

  // -------------------------------------------------------------- mirror

  function startMirror() {
    if (mirror || !sessionId) return;
    mirror = new Hub.Mirror({
      sessionId,
      frameId: isTop ? 'main' : (window.name || String(Math.random()).slice(2, 10)),
      send: (op, body, seq) => {
        chrome.runtime.sendMessage({ kind: 'mirror', op, body, seq }).catch(() => {});
      },
    });
    mirror.start();
  }

  function teardown() {
    sessionId = null;
    if (mirror) {
      mirror.stop();
      mirror = null;
    }
    hideIndicator();
  }

  // ------------------------------------------------------------ commands

  chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
    switch (message.kind) {
      case 'command':
        Hub.commands.run(message.op, message.params)
          .then((body) => sendResponse({ body }))
          .catch((err) => sendResponse({
            error: { code: err.hubCode || 'internal', message: err.message },
          }));
        return true;

      case 'attached':
        sessionId = message.sessionId;
        showIndicator();
        startMirror();
        sendResponse({ ok: true });
        return false;

      case 'detach':
        teardown();
        sendResponse({ ok: true });
        return false;

      default:
        return false;
    }
  });

  // ------------------------------------------- exchanges from the MAIN world

  window.addEventListener('message', (event) => {
    // Only accept messages this document sent to itself: any page can post to
    // this window, and a page that could forge exchanges could poison a
    // scraper's request log.
    if (event.source !== window) return;
    const data = event.data;
    if (!data || data.__hubAgent !== true || data.kind !== 'exchange') return;
    if (!sessionId) return;

    chrome.runtime.sendMessage({ kind: 'exchange', record: data.record }).catch(() => {});
  });

  // ---------------------------------------------------------------- boot

  // A content script may load into a tab that is already a session -- after a
  // navigation, or when the extension is reloaded -- so it asks rather than
  // waiting to be told.
  chrome.runtime.sendMessage({ kind: 'session-of' })
    .then((res) => {
      if (!res || !res.sessionId) return;
      sessionId = res.sessionId;
      showIndicator();
      if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', startMirror, { once: true });
      } else {
        startMirror();
      }
    })
    .catch(() => {
      // The service worker was asleep and has not woken yet. The background
      // will send 'attached' when it does.
    });
})();
