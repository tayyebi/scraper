// Agent core: the wire protocol.
//
// This is the JavaScript half of internal/wire/envelope.go. The two are
// implemented separately and must agree exactly; docs/protocol.md is the
// contract, and Go's TestEnvelopeJSONFieldNames pins the field names so a
// rename there cannot silently break every agent in the field.
//
// Loaded as a classic script (importScripts in Chrome's service worker,
// background.scripts in Firefox) rather than an ES module, because those are
// the two loaders both platforms support without a bundler -- and this project
// has no build step.

var Hub = self.Hub || (self.Hub = {});

Hub.protocol = (function () {
  const PROTOCOL_VERSION = 1;

  // Envelope kinds.
  const CMD = 'cmd';
  const RES = 'res';
  const EVT = 'evt';
  const ERR = 'err';

  // Stable error codes. These match wire.Code* in Go.
  const codes = {
    unsupported: 'unsupported',
    noSuchTab: 'no_such_tab',
    timeout: 'timeout',
    navigation: 'navigation_failed',
    notFound: 'not_found',
    badParams: 'bad_params',
    denied: 'denied',
    internal: 'internal',
    detached: 'detached',
    mirrorGap: 'mirror_gap',
  };

  const now = () => Date.now();

  function result(id, sessionId, body) {
    return { v: PROTOCOL_VERSION, id, t: RES, ts: now(), sid: sessionId || undefined, body };
  }

  function failure(id, sessionId, code, message) {
    return {
      v: PROTOCOL_VERSION,
      id,
      t: ERR,
      ts: now(),
      sid: sessionId || undefined,
      err: { code: code || codes.internal, message: String(message || '') },
    };
  }

  function event(sessionId, op, body, seq) {
    const env = { v: PROTOCOL_VERSION, t: EVT, ts: now(), op, body };
    if (sessionId) env.sid = sessionId;
    if (seq) env.seq = seq;
    return env;
  }

  // valid mirrors Envelope.Validate in Go. An agent that sends an unroutable
  // envelope gets its channel closed, so it is cheaper to catch it here.
  function valid(env) {
    if (!env || env.v !== PROTOCOL_VERSION) return false;
    switch (env.t) {
      case CMD: return Boolean(env.id && env.op);
      case RES: return Boolean(env.id);
      case ERR: return Boolean(env.id && env.err && env.err.code);
      case EVT: return Boolean(env.op);
      default: return false;
    }
  }

  return { PROTOCOL_VERSION, CMD, RES, EVT, ERR, codes, result, failure, event, valid };
})();
