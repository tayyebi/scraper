// Agent core: network capture bookkeeping.
//
// This file does not intercept anything itself -- interception is
// platform-specific and lives in the host adapter (a MAIN-world fetch patch in
// Chrome, filterResponseData in Firefox). What lives here is the part that is
// the same everywhere: turning a captured exchange into the hub's shape, and
// getting the bytes to the hub without putting them on the command channel.

var Hub = self.Hub || (self.Hub = {});

Hub.netlog = (function () {
  // Bodies above this are not captured. A capture that quietly holds a 200 MB
  // video in memory is a capture that gets the tab killed.
  const MAX_BODY_BYTES = 8 * 1024 * 1024;

  /** sha256Hex hashes bytes with the platform's own crypto. */
  async function sha256Hex(bytes) {
    const digest = await crypto.subtle.digest('SHA-256', bytes);
    return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, '0')).join('');
  }

  function headerObject(headers) {
    const out = {};
    if (!headers) return out;
    if (typeof headers.forEach === 'function' && !Array.isArray(headers)) {
      headers.forEach((value, key) => { out[String(key).toLowerCase()] = String(value); });
      return out;
    }
    for (const [key, value] of Object.entries(headers)) {
      out[String(key).toLowerCase()] = String(value);
    }
    return out;
  }

  /**
   * upload stores captured bytes as an artifact and returns its digest.
   *
   * Bulk bytes deliberately travel over HTTP rather than the command channel:
   * a large body on the channel would head-of-line block every command sharing
   * it. Going over HTTP also makes uploads parallel, retryable, and skippable.
   *
   * @returns {Promise<{digest: string, size: number, truncated: boolean}|null>}
   */
  async function upload(hubURL, credential, bytes) {
    if (!bytes || !bytes.byteLength) return null;

    let truncated = false;
    let payload = bytes;
    if (payload.byteLength > MAX_BODY_BYTES) {
      payload = payload.slice(0, MAX_BODY_BYTES);
      truncated = true;
    }

    const digest = await sha256Hex(payload);
    const base = String(hubURL).replace(/\/+$/, '');
    const url = `${base}/agent/v1/artifacts/${digest}`;
    const auth = { Authorization: `Bearer ${credential}` };

    try {
      // Ask first. The hub is content-addressed, so if it already holds these
      // bytes the upload is pure waste -- and on a page that fetches the same
      // JSON on every scroll, that is most uploads.
      const head = await fetch(url, { method: 'HEAD', headers: auth });
      if (head.ok) return { digest, size: payload.byteLength, truncated };

      const put = await fetch(url, { method: 'PUT', headers: auth, body: payload });
      if (!put.ok && put.status !== 409) {
        console.warn('hub: artifact upload failed', put.status);
        return null;
      }
      return { digest, size: payload.byteLength, truncated };
    } catch (err) {
      // A failed upload must not lose the exchange. The row is still worth
      // recording without a body.
      console.warn('hub: artifact upload error', err);
      return null;
    }
  }

  /**
   * exchange builds the hub's request-log shape.
   *
   * Header names are lowercased so the same request logged by Chrome and by
   * Firefox produces the same row.
   */
  function exchange(record) {
    return {
      requestId: record.requestId || '',
      method: (record.method || 'GET').toUpperCase(),
      url: record.url || '',
      status: record.status || 0,
      statusText: record.statusText || '',
      mimeType: record.mimeType || '',
      resourceType: record.resourceType || '',
      initiator: record.initiator || '',
      requestHeaders: headerObject(record.requestHeaders),
      responseHeaders: headerObject(record.responseHeaders),
      requestBodyDigest: record.requestBodyDigest || '',
      responseBodyDigest: record.responseBodyDigest || '',
      requestBodySize: record.requestBodySize || 0,
      responseBodySize: record.responseBodySize || 0,
      fromCache: Boolean(record.fromCache),
      truncated: Boolean(record.truncated),
      startedAt: new Date(record.startedAt || Date.now()).toISOString(),
      durationMs: Math.max(0, Math.round(record.durationMs || 0)),
    };
  }

  return { MAX_BODY_BYTES, sha256Hex, headerObject, upload, exchange };
})();
