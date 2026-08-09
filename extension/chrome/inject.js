// Chrome MAIN-world capture.
//
// This runs in the page's own JavaScript context, which is the only place a
// fetch/XHR patch can see anything. It is also the reason this file is as small
// and as careful as it is: it shares a global with hostile code.
//
// What this mode cannot see, and what the hub therefore reports as a capture
// gap: document navigations, subresource loads (img, script, css, media),
// requests issued by service workers, WebSocket and EventSource traffic, and
// anything from a page that captured a reference to fetch before this ran. Full
// fidelity needs chrome.debugger, which is opt-in because it shows the "being
// debugged" infobar.

(() => {
  // Bodies above this are not read. Buffering a video into a page's memory to
  // capture it is how a tab gets killed.
  const MAX_BODY = 4 * 1024 * 1024;

  const nativeFetch = window.fetch;
  const NativeXHR = window.XMLHttpRequest;

  function report(record) {
    // postMessage rather than a direct call: the content script lives in an
    // isolated world, and this is the only channel between them.
    window.postMessage({ __hubAgent: true, kind: 'exchange', record }, '*');
  }

  function toBase64(buffer) {
    const bytes = new Uint8Array(buffer);
    let binary = '';
    // Chunked because String.fromCharCode(...bigArray) blows the argument
    // limit somewhere around a hundred thousand elements.
    const CHUNK = 8192;
    for (let i = 0; i < bytes.length; i += CHUNK) {
      binary += String.fromCharCode.apply(null, bytes.subarray(i, i + CHUNK));
    }
    return btoa(binary);
  }

  function headersToObject(headers) {
    const out = {};
    if (!headers) return out;
    try {
      headers.forEach((value, key) => { out[key.toLowerCase()] = value; });
    } catch {
      // Not a Headers instance.
    }
    return out;
  }

  // --------------------------------------------------------------- fetch

  window.fetch = async function patchedFetch(input, init) {
    const started = Date.now();
    const request = input instanceof Request ? input : new Request(input, init);
    const url = request.url;
    const method = request.method;

    let response;
    try {
      response = await nativeFetch.call(this, input, init);
    } catch (err) {
      report({
        url,
        method,
        status: 0,
        statusText: String(err && err.message),
        startedAt: started,
        durationMs: Date.now() - started,
        resourceType: 'fetch',
      });
      throw err;
    }

    // The response body can only be read once. Cloning is what lets the page
    // have its response *and* lets the capture read the bytes -- without it,
    // capturing would break every page it observed.
    const copy = response.clone();
    const record = {
      url,
      method,
      status: response.status,
      statusText: response.statusText,
      mimeType: response.headers.get('content-type') || '',
      requestHeaders: headersToObject(request.headers),
      responseHeaders: headersToObject(response.headers),
      startedAt: started,
      durationMs: Date.now() - started,
      resourceType: 'fetch',
      fromCache: response.type === 'basic' && response.status === 304,
    };

    // Read the copy in the background: awaiting it here would make every
    // patched fetch wait for the whole body before the page gets its response.
    copy.arrayBuffer().then((buffer) => {
      record.responseBodySize = buffer.byteLength;
      if (buffer.byteLength <= MAX_BODY) {
        record.bodyBase64 = toBase64(buffer);
      } else {
        record.truncated = true;
      }
      report(record);
    }).catch(() => report(record));

    return response;
  };

  // ----------------------------------------------------------------- XHR

  function PatchedXHR() {
    const xhr = new NativeXHR();
    let method = 'GET';
    let url = '';
    let started = 0;
    const requestHeaders = {};

    const open = xhr.open;
    xhr.open = function patchedOpen(m, u, ...rest) {
      method = m;
      url = u;
      return open.call(this, m, u, ...rest);
    };

    const setRequestHeader = xhr.setRequestHeader;
    xhr.setRequestHeader = function patchedSetHeader(name, value) {
      requestHeaders[String(name).toLowerCase()] = String(value);
      return setRequestHeader.call(this, name, value);
    };

    const send = xhr.send;
    xhr.send = function patchedSend(body) {
      started = Date.now();
      return send.call(this, body);
    };

    xhr.addEventListener('loadend', () => {
      const record = {
        url: String(url),
        method: String(method).toUpperCase(),
        status: xhr.status,
        statusText: xhr.statusText,
        mimeType: xhr.getResponseHeader('content-type') || '',
        requestHeaders,
        responseHeaders: parseRawHeaders(xhr.getAllResponseHeaders()),
        startedAt: started || Date.now(),
        durationMs: Date.now() - (started || Date.now()),
        resourceType: 'xhr',
      };

      try {
        // responseType decides what is readable. Anything else would throw,
        // and a capture that throws inside a page's own handler would break
        // the page.
        if (xhr.responseType === '' || xhr.responseType === 'text') {
          const text = xhr.responseText || '';
          record.responseBodySize = text.length;
          if (text.length <= MAX_BODY) {
            record.bodyBase64 = toBase64(new TextEncoder().encode(text).buffer);
          } else {
            record.truncated = true;
          }
        } else if (xhr.responseType === 'arraybuffer' && xhr.response) {
          record.responseBodySize = xhr.response.byteLength;
          if (xhr.response.byteLength <= MAX_BODY) {
            record.bodyBase64 = toBase64(xhr.response);
          } else {
            record.truncated = true;
          }
        }
      } catch {
        // Body unreadable in this mode. The exchange is still worth reporting.
      }

      report(record);
    });

    return xhr;
  }

  PatchedXHR.prototype = NativeXHR.prototype;
  PatchedXHR.UNSENT = 0;
  PatchedXHR.OPENED = 1;
  PatchedXHR.HEADERS_RECEIVED = 2;
  PatchedXHR.LOADING = 3;
  PatchedXHR.DONE = 4;
  window.XMLHttpRequest = PatchedXHR;

  function parseRawHeaders(raw) {
    const out = {};
    for (const line of String(raw || '').trim().split(/[\r\n]+/)) {
      const idx = line.indexOf(':');
      if (idx < 0) continue;
      out[line.slice(0, idx).trim().toLowerCase()] = line.slice(idx + 1).trim();
    }
    return out;
  }
})();
