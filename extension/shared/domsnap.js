// Agent core: DOM snapshot.
//
// A snapshot is a photograph of a document. It is the thing a mirror starts
// from, and the thing a caller falls back to when the mirror has drifted.
//
// Node ids are assigned here and remembered in a WeakMap, so the mutation
// stream can reference the same nodes by the same numbers. That shared registry
// is the reason snapshot and mirror live next to each other: a mutation that
// referenced a node the snapshot never numbered would be unappliable.

var Hub = self.Hub || (self.Hub = {});

Hub.domsnap = (function () {
  // nodeIds must be a WeakMap: a strong map would keep every node a page ever
  // created alive for as long as the tab is open.
  const nodeIds = new WeakMap();
  let nextId = 0;

  function idOf(node) {
    let id = nodeIds.get(node);
    if (id === undefined) {
      id = ++nextId;
      nodeIds.set(node, id);
    }
    return id;
  }

  function knownId(node) {
    return nodeIds.get(node);
  }

  // Attributes that are never useful and often enormous. Dropping them keeps a
  // snapshot of a real page from being mostly base64.
  const SKIP_ATTR_PREFIX = ['data-react', 'data-v-', 'ng-'];
  const MAX_ATTR_LENGTH = 8 * 1024;

  function attrsOf(el) {
    const out = [];
    for (const attr of el.attributes) {
      const name = attr.name;
      if (SKIP_ATTR_PREFIX.some((p) => name.startsWith(p))) continue;
      let value = attr.value;
      if (value.length > MAX_ATTR_LENGTH) {
        // Truncate rather than drop: an over-long src is still worth knowing
        // about, and the marker says the value is not the whole story.
        value = value.slice(0, MAX_ATTR_LENGTH) + '…[truncated]';
      }
      out.push({ n: name, v: value });
    }
    return out;
  }

  const MAX_TEXT_LENGTH = 64 * 1024;

  function textOf(node) {
    const data = node.data || '';
    return data.length > MAX_TEXT_LENGTH ? data.slice(0, MAX_TEXT_LENGTH) : data;
  }

  /**
   * serialize converts a node and its subtree into the wire shape.
   * Returns null for nodes that carry nothing worth mirroring.
   */
  function serialize(node, depth = 0) {
    // A pathological page can nest deeply enough to blow the stack. Stopping is
    // better than crashing the content script and losing the session.
    if (depth > 500) return null;

    switch (node.nodeType) {
      case Node.ELEMENT_NODE: {
        const el = /** @type {Element} */ (node);
        const out = {
          id: idOf(el),
          t: 1,
          n: el.tagName.toLowerCase(),
          a: attrsOf(el),
        };

        // An open shadow root is part of the rendered document, so it belongs
        // in the mirror. A closed one is genuinely inaccessible -- that is what
        // "closed" means -- and is documented as a limitation rather than
        // worked around, because working around it would mean defeating a
        // boundary the page deliberately set.
        if (el.shadowRoot) {
          const shadow = serializeChildren(el.shadowRoot, depth + 1);
          out.sr = { id: idOf(el.shadowRoot), t: 11, c: shadow };
        }

        // A frame's document lives in its own snapshot, sent separately, and is
        // referenced here so the two can be stitched together.
        if (el.tagName === 'IFRAME' || el.tagName === 'FRAME') {
          out.f = el.getAttribute('name') || el.getAttribute('id') || String(out.id);
        }

        out.c = serializeChildren(el, depth + 1);
        return out;
      }

      case Node.TEXT_NODE: {
        const data = textOf(node);
        // Whitespace-only text between block elements is layout, not content.
        if (!data.trim() && !node.parentElement?.closest('pre, textarea')) return null;
        return { id: idOf(node), t: 3, v: data };
      }

      case Node.COMMENT_NODE:
        return { id: idOf(node), t: 8, v: textOf(node) };

      case Node.DOCUMENT_TYPE_NODE:
        return { id: idOf(node), t: 10, n: node.name || 'html' };

      case Node.DOCUMENT_NODE:
      case Node.DOCUMENT_FRAGMENT_NODE:
        return { id: idOf(node), t: node.nodeType, c: serializeChildren(node, depth + 1) };

      default:
        return null;
    }
  }

  function serializeChildren(parent, depth) {
    const out = [];
    for (const child of parent.childNodes) {
      const node = serialize(child, depth);
      if (node) out.push(node);
    }
    return out;
  }

  /**
   * snapshot serializes a whole document.
   * @returns {{frameId: string, url: string, title: string, root: object}}
   */
  function snapshot(doc = document, frameId = 'main') {
    const root = serialize(doc.documentElement || doc, 0);
    return {
      frameId,
      url: doc.location ? doc.location.href : '',
      title: doc.title || '',
      root,
    };
  }

  return { snapshot, serialize, serializeChildren, idOf, knownId };
})();
