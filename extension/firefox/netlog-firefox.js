// Firefox network capture: full fidelity, via filterResponseData.
//
// This is why the Firefox agent reports a different capture mode than the
// Chrome one. filterResponseData gives the extension the actual response
// stream, for every request the browser makes -- document navigations,
// subresources, service-worker fetches, all of it -- with no debugger banner
// and no extra permission prompt.
//
// Chrome has no equivalent short of chrome.debugger, which is why its default
// mode is a MAIN-world fetch patch and why the hub reports that mode's gaps to
// callers. The difference is real, so the two agents declare it honestly rather
// than pretending to be the same.

var Hub = self.Hub || (self.Hub = {});

Hub.firefoxCapture = (function () {
  // Bodies above this are counted but not kept. Buffering an arbitrary download
  // into the extension's memory would eventually take the browser with it.
  const MAX_BODY = 8 * 1024 * 1024;

  // requestId -> partial record. Firefox gives the same id across the request
  // lifecycle, which is what lets headers, body and timing be stitched together.
  const inFlight = new Map();

  function install({ isTracked, onExchange }) {
    const api = typeof browser !== 'undefined' ? browser : chrome;

    api.webRequest.onBeforeSendHeaders.addListener(
      (details) => {
        if (!isTracked(details.tabId)) return {};
        record(details).requestHeaders = flatten(details.requestHeaders);
        return {};
      },
      { urls: ['<all_urls>'] },
      ['requestHeaders'],
    );

    api.webRequest.onHeadersReceived.addListener(
      (details) => {
        if (!isTracked(details.tabId)) return {};

        const entry = record(details);
        entry.status = details.statusCode;
        entry.statusText = details.statusLine || '';
        entry.responseHeaders = flatten(details.responseHeaders);
        entry.mimeType = header(details.responseHeaders, 'content-type');
        entry.fromCache = Boolean(details.fromCache);

        capture(api, details.requestId, entry, onExchange);
        return {};
      },
      { urls: ['<all_urls>'] },
      ['responseHeaders', 'blocking'],
    );

    api.webRequest.onErrorOccurred.addListener(
      (details) => {
        if (!isTracked(details.tabId)) return;
        const entry = inFlight.get(details.requestId);
        inFlight.delete(details.requestId);
        if (!entry) return;
        entry.status = 0;
        entry.statusText = details.error || 'request failed';
        entry.durationMs = Date.now() - entry.startedAt;
        onExchange(details.tabId, entry, null);
      },
      { urls: ['<all_urls>'] },
    );

    function record(details) {
      let entry = inFlight.get(details.requestId);
      if (!entry) {
        entry = {
          requestId: String(details.requestId),
          url: details.url,
          method: details.method,
          resourceType: details.type,
          startedAt: Date.now(),
          tabId: details.tabId,
        };
        inFlight.set(details.requestId, entry);
      }
      return entry;
    }
  }

  // capture attaches a stream filter and reassembles the body.
  //
  // The filter must write every chunk straight back out. A filter that buffers
  // and forgets to forward does not observe a page -- it breaks it.
  function capture(api, requestId, entry, onExchange) {
    let filter;
    try {
      filter = api.webRequest.filterResponseData(requestId);
    } catch {
      // Some requests cannot be filtered (already cached, or an internal
      // scheme). The exchange is still worth reporting without a body.
      inFlight.delete(requestId);
      entry.durationMs = Date.now() - entry.startedAt;
      onExchange(entry.tabId, entry, null);
      return;
    }

    const chunks = [];
    let total = 0;
    let truncated = false;

    filter.ondata = (event) => {
      // Pass through first, unconditionally. The page's copy is not optional.
      filter.write(event.data);

      total += event.data.byteLength;
      if (total <= MAX_BODY) {
        chunks.push(new Uint8Array(event.data));
      } else {
        truncated = true;
      }
    };

    filter.onstop = () => {
      filter.disconnect();
      inFlight.delete(requestId);

      entry.durationMs = Date.now() - entry.startedAt;
      entry.responseBodySize = total;
      entry.truncated = truncated;
      onExchange(entry.tabId, entry, truncated ? null : concat(chunks, total));
    };

    filter.onerror = () => {
      inFlight.delete(requestId);
      entry.durationMs = Date.now() - entry.startedAt;
      entry.statusText = filter.error || entry.statusText;
      onExchange(entry.tabId, entry, null);
    };
  }

  function concat(chunks, total) {
    const out = new Uint8Array(total);
    let offset = 0;
    for (const chunk of chunks) {
      out.set(chunk, offset);
      offset += chunk.length;
    }
    return out;
  }

  function flatten(headers) {
    const out = {};
    for (const h of headers || []) out[String(h.name).toLowerCase()] = String(h.value ?? '');
    return out;
  }

  function header(headers, name) {
    for (const h of headers || []) {
      if (String(h.name).toLowerCase() === name) return String(h.value ?? '');
    }
    return '';
  }

  return { install, MAX_BODY };
})();
