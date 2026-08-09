// Agent core: capability reporting.
//
// An agent declares what it can actually do, and the hub surfaces that to
// callers verbatim. The rule here is the one that makes the whole scheme worth
// having: never claim a capability that is not real. A capture mode that
// overstates itself produces a request log which looks complete and is not,
// and that is worse than no log, because nobody goes looking for what is
// missing.

var Hub = self.Hub || (self.Hub = {});

Hub.capabilities = (function () {
  // Must match core.CaptureMode in Go.
  const CAPTURE = {
    none: 'none',
    fetchPatch: 'fetch-patch',
    filterResponse: 'filter-response',
    debugger: 'debugger',
  };

  // The v1 vocabulary, matching core.KnownOps.
  const ALL_OPS = [
    'navigate', 'waitFor', 'click', 'type', 'scroll', 'select', 'eval',
    'snapshotDom', 'extract', 'screenshot', 'cookies', 'back', 'forward',
    'reload', 'close',
  ];

  /**
   * build assembles the capability report sent at enrollment.
   *
   * @param {object} host
   * @param {string} host.capture        the capture mode actually in force
   * @param {boolean} [host.eval]        whether eval is enabled (opt-in)
   * @param {boolean} [host.screenshot]
   * @param {boolean} [host.cookies]
   * @param {boolean} [host.attach]
   * @param {boolean} [host.openTabs]
   * @param {boolean} [host.mirror]
   */
  function build(host) {
    const ops = ALL_OPS.filter((op) => {
      switch (op) {
        // eval is remote code execution as far as an extension store is
        // concerned, so it is never on unless the operator turned it on.
        case 'eval': return Boolean(host.eval);
        case 'screenshot': return Boolean(host.screenshot);
        case 'cookies': return Boolean(host.cookies);
        case 'close': return Boolean(host.openTabs || host.attach);
        default: return true;
      }
    });

    return {
      capture: host.capture || CAPTURE.none,
      eval: Boolean(host.eval),
      screenshot: Boolean(host.screenshot),
      cookies: Boolean(host.cookies),
      attach: Boolean(host.attach),
      openTabs: Boolean(host.openTabs),
      mirror: host.mirror !== false,
      ops,
    };
  }

  return { CAPTURE, ALL_OPS, build };
})();
