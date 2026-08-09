// Agent core: the v1 command vocabulary, executed against a page.
//
// These run in the content script, where the DOM is. The host adapter supplies
// anything that needs extension privileges -- tabs, cookies, screenshots -- so
// this file stays the same across Chrome, Firefox, and a WebView.

var Hub = self.Hub || (self.Hub = {});

Hub.commands = (function () {
  const protocol = Hub.protocol;

  function fail(code, message) {
    const err = new Error(message);
    err.hubCode = code;
    return err;
  }

  /** waitFor polls until a condition holds or the deadline passes. */
  function until(check, timeoutMs, description) {
    const deadline = Date.now() + (timeoutMs || 10_000);
    return new Promise((resolve, reject) => {
      const tick = () => {
        let value;
        try {
          value = check();
        } catch (err) {
          reject(fail(protocol.codes.internal, err.message));
          return;
        }
        if (value) {
          resolve(value);
          return;
        }
        if (Date.now() > deadline) {
          reject(fail(protocol.codes.timeout, `timed out waiting for ${description}`));
          return;
        }
        setTimeout(tick, 50);
      };
      tick();
    });
  }

  function must(selector) {
    if (!selector) throw fail(protocol.codes.badParams, 'a selector is required');
    const el = document.querySelector(selector);
    if (!el) throw fail(protocol.codes.notFound, `no element matches ${selector}`);
    return el;
  }

  // Dispatching real events rather than calling .click() matters: a page that
  // listens for mousedown, or checks isTrusted-adjacent details like button and
  // coordinates, behaves differently otherwise. This is BYOB -- the point is to
  // look like the human whose browser this is.
  function clickLike(el) {
    const rect = el.getBoundingClientRect();
    const init = {
      bubbles: true,
      cancelable: true,
      view: window,
      clientX: rect.left + rect.width / 2,
      clientY: rect.top + rect.height / 2,
      button: 0,
    };
    el.dispatchEvent(new PointerEvent('pointerdown', init));
    el.dispatchEvent(new MouseEvent('mousedown', init));
    el.focus?.();
    el.dispatchEvent(new PointerEvent('pointerup', init));
    el.dispatchEvent(new MouseEvent('mouseup', init));
    el.dispatchEvent(new MouseEvent('click', init));
  }

  // Setting .value directly is invisible to React and Vue, which track the
  // native setter. Going through the prototype descriptor is what makes a typed
  // value actually register with a framework-driven form.
  function setValue(el, value) {
    const prototype = el instanceof HTMLTextAreaElement
      ? HTMLTextAreaElement.prototype
      : HTMLInputElement.prototype;
    const setter = Object.getOwnPropertyDescriptor(prototype, 'value')?.set;
    if (setter) setter.call(el, value);
    else el.value = value;
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
  }

  function describe(el) {
    return {
      tag: el.tagName.toLowerCase(),
      id: el.id || undefined,
      text: (el.textContent || '').trim().slice(0, 200) || undefined,
    };
  }

  const handlers = {
    async navigate(params) {
      if (!params.url) throw fail(protocol.codes.badParams, 'navigate needs a url');
      location.href = params.url;
      return { url: params.url };
    },

    async waitFor(params) {
      if (params.selector) {
        const el = await until(() => document.querySelector(params.selector), params.timeoutMs, params.selector);
        return { found: true, element: describe(el) };
      }
      if (params.text) {
        await until(
          () => (document.body?.innerText || '').includes(params.text),
          params.timeoutMs,
          `text ${JSON.stringify(params.text)}`,
        );
        return { found: true };
      }
      if (params.readyState) {
        await until(() => document.readyState === params.readyState, params.timeoutMs, `readyState ${params.readyState}`);
        return { readyState: document.readyState };
      }
      throw fail(protocol.codes.badParams, 'waitFor needs a selector, text, or readyState');
    },

    async click(params) {
      const el = must(params.selector);
      el.scrollIntoView({ block: 'center', behavior: 'instant' });
      clickLike(el);
      return { clicked: describe(el) };
    },

    async type(params) {
      const el = must(params.selector);
      el.focus?.();
      if (params.clear) setValue(el, '');
      setValue(el, params.text ?? '');
      if (params.submit && el.form) el.form.requestSubmit?.();
      return { typed: (params.text ?? '').length };
    },

    async scroll(params) {
      if (params.selector) {
        must(params.selector).scrollIntoView({ block: params.block || 'center', behavior: 'instant' });
        return { scrolledTo: params.selector };
      }
      const top = params.bottom ? document.body.scrollHeight : (params.y ?? 0);
      window.scrollTo({ top, left: params.x ?? 0, behavior: 'instant' });
      return { y: window.scrollY };
    },

    async select(params) {
      const el = must(params.selector);
      if (!(el instanceof HTMLSelectElement)) {
        throw fail(protocol.codes.badParams, `${params.selector} is not a <select>`);
      }
      el.value = params.value;
      el.dispatchEvent(new Event('change', { bubbles: true }));
      return { value: el.value };
    },

    // extract is the reason most callers never need eval: a declarative
    // selector-to-field mapping covers the common scrape without the agent
    // having to accept arbitrary code.
    async extract(params) {
      const scope = params.selector ? document.querySelectorAll(params.selector) : [document];
      const fields = params.fields || {};
      const rows = [];

      for (const root of scope) {
        const row = {};
        for (const [name, spec] of Object.entries(fields)) {
          const sel = typeof spec === 'string' ? spec : spec.selector;
          const attr = typeof spec === 'string' ? null : spec.attribute;
          const target = sel ? root.querySelector(sel) : root;
          if (!target) {
            row[name] = null;
            continue;
          }
          row[name] = attr ? target.getAttribute(attr) : (target.textContent || '').trim();
        }
        rows.push(row);
      }
      return { rows, count: rows.length };
    },

    async snapshotDom() {
      // Returned inline as well as pushed as an event, so a caller who asked
      // directly gets an answer without waiting for the mirror to catch up.
      return Hub.domsnap.snapshot(document, 'main');
    },

    async back() {
      history.back();
      return { ok: true };
    },

    async forward() {
      history.forward();
      return { ok: true };
    },

    async reload() {
      location.reload();
      return { ok: true };
    },
  };

  /**
   * run executes a page-scoped command.
   * Anything not here needs extension privileges and is handled by the host.
   */
  async function run(op, params) {
    const handler = handlers[op];
    if (!handler) throw fail(protocol.codes.unsupported, `this agent cannot ${op} in the page`);
    return handler(params || {});
  }

  function supports(op) {
    return Object.prototype.hasOwnProperty.call(handlers, op);
  }

  return { run, supports, fail, handlers };
})();
