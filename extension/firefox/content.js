// Firefox content script.
//
// Same job as the Chrome one, minus the MAIN-world bridge: capture happens in
// the background page here, through filterResponseData, so nothing needs to be
// injected into the page's own context. That is one fewer place where extension
// code shares a global with a hostile page.

(() => {
  const api = typeof browser !== 'undefined' ? browser : chrome;
  const isTop = window.top === window;

  let sessionId = null;
  let mirror = null;
  let indicator = null;

  // A controlled tab always says so. Enrollment is the one consent gesture and
  // commands run unattended afterwards, so this is what keeps that grant
  // honest -- and it carries its own stop button.
  function showIndicator() {
    if (indicator || !isTop || !document.documentElement) return;

    indicator = document.createElement('div');
    indicator.setAttribute('role', 'status');
    indicator.setAttribute('aria-live', 'polite');
    indicator.textContent = 'This tab is under automation control';
    indicator.style.cssText = [
      'position:fixed', 'top:0', 'left:0', 'right:0',
      'z-index:2147483647', 'padding:4px 10px',
      'font:12px/1.5 system-ui,sans-serif', 'color:#fff', 'background:#b26a00',
      'text-align:center', 'pointer-events:none',
      'box-shadow:0 1px 4px rgba(0,0,0,.35)',
    ].join(';');

    const stop = document.createElement('button');
    stop.textContent = 'Stop';
    stop.style.cssText = 'margin-left:10px;pointer-events:auto;font:inherit;cursor:pointer';
    stop.addEventListener('click', () => {
      api.runtime.sendMessage({ kind: 'detach-self' }).catch(() => {});
      teardown();
    });
    indicator.append(stop);

    document.documentElement.append(indicator);
  }

  function hideIndicator() {
    if (indicator) {
      indicator.remove();
      indicator = null;
    }
  }

  function startMirror() {
    if (mirror || !sessionId) return;
    mirror = new Hub.Mirror({
      sessionId,
      frameId: isTop ? 'main' : (window.name || String(Math.random()).slice(2, 10)),
      send: (op, body, seq) => {
        api.runtime.sendMessage({ kind: 'mirror', op, body, seq }).catch(() => {});
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

  api.runtime.onMessage.addListener(async (message) => {
    switch (message.kind) {
      case 'command':
        try {
          return { body: await Hub.commands.run(message.op, message.params) };
        } catch (err) {
          return { error: { code: err.hubCode || 'internal', message: err.message } };
        }

      case 'attached':
        sessionId = message.sessionId;
        showIndicator();
        startMirror();
        return { ok: true };

      case 'detach':
        teardown();
        return { ok: true };

      default:
        return undefined;
    }
  });

  api.runtime.sendMessage({ kind: 'session-of' })
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
      // Background not ready; it will send 'attached' when it is.
    });
})();
