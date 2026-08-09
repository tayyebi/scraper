// Agent core: the mutation stream.
//
// The hazard this file exists to manage: mutation volume on a real page is
// enormous. A single React render can produce thousands of records, and an
// animation produces them every frame, forever. Sending each one as it happens
// would flood the channel and, on a busy page, outrun it.
//
// So mutations are coalesced into batches, batches are capped, and when the cap
// is exceeded the agent gives up on deltas and demands a re-snapshot instead.
// Overflowing into a re-snapshot is bounded work; queueing without limit is
// not.

var Hub = self.Hub || (self.Hub = {});

Hub.Mirror = (function () {
  const domsnap = Hub.domsnap;

  // Coalescing window. Long enough to collapse a render burst into one batch,
  // short enough that a caller polling the DOM sees current data.
  const FLUSH_MS = 100;

  // Beyond this many ops in one flush, a fresh snapshot is smaller than the
  // deltas describing how to get there.
  const MAX_OPS_PER_FLUSH = 2_000;

  class Mirror {
    /**
     * @param {object} options
     * @param {string} options.sessionId
     * @param {function} options.send   (op, body, seq) => void
     * @param {Document} [options.doc]
     * @param {string} [options.frameId]
     */
    constructor(options) {
      this.sessionId = options.sessionId;
      this.send = options.send;
      this.doc = options.doc || document;
      this.frameId = options.frameId || 'main';

      this.seq = 0;
      this.pending = [];
      this.flushTimer = null;
      this.observer = null;
      this.running = false;
    }

    /** start takes a snapshot and begins streaming deltas from it. */
    start() {
      if (this.running) return;
      this.running = true;
      this.snapshot();

      this.observer = new MutationObserver((records) => this.collect(records));
      this.observer.observe(this.doc, {
        subtree: true,
        childList: true,
        attributes: true,
        characterData: true,
      });
    }

    stop() {
      this.running = false;
      if (this.observer) {
        this.observer.disconnect();
        this.observer = null;
      }
      this.clearFlush();
      this.pending = [];
    }

    /**
     * snapshot sends a full document and resets the sequence.
     *
     * Everything downstream keys off seq: the hub applies batch n only after
     * batch n-1, and a gap forces it back here. Resetting to 1 with the
     * snapshot is what makes the two sides agree on where the stream begins.
     */
    snapshot() {
      this.pending = [];
      this.clearFlush();
      this.seq = 1;
      const snap = domsnap.snapshot(this.doc, this.frameId);
      this.send('mirror.snapshot', snap, this.seq);
    }

    collect(records) {
      for (const record of records) {
        switch (record.type) {
          case 'attributes': {
            const id = domsnap.knownId(record.target);
            if (id === undefined) break;
            const value = record.target.getAttribute(record.attributeName);
            this.pending.push({
              op: 'attr',
              f: this.frameId,
              id,
              n: record.attributeName,
              // null means remove. The hub distinguishes a removed attribute
              // from one set to the empty string, and so must this.
              v: value === null ? null : value,
            });
            break;
          }

          case 'characterData': {
            const id = domsnap.knownId(record.target);
            if (id === undefined) break;
            this.pending.push({ op: 'text', f: this.frameId, id, v: record.target.data || '' });
            break;
          }

          case 'childList': {
            for (const removed of record.removedNodes) {
              const id = domsnap.knownId(removed);
              // A node the hub never saw does not need removing there.
              if (id !== undefined) this.pending.push({ op: 'remove', f: this.frameId, id });
            }
            for (const added of record.addedNodes) {
              const parentId = domsnap.knownId(record.target);
              if (parentId === undefined) break;
              const node = domsnap.serialize(added, 0);
              if (!node) break;
              const ref = record.nextSibling ? domsnap.knownId(record.nextSibling) : undefined;
              this.pending.push({
                op: 'insert',
                f: this.frameId,
                parent: parentId,
                ref: ref === undefined ? 0 : ref,
                node,
              });
            }
            break;
          }

          default:
            break;
        }
      }

      if (this.pending.length >= MAX_OPS_PER_FLUSH) {
        // Overflow. Sending a snapshot is bounded work; sending ten thousand
        // deltas is not, and the hub would only apply them to rebuild a
        // document we can describe directly.
        this.snapshot();
        return;
      }
      this.scheduleFlush();
    }

    scheduleFlush() {
      if (this.flushTimer || !this.pending.length) return;
      this.flushTimer = setTimeout(() => {
        this.flushTimer = null;
        this.flush();
      }, FLUSH_MS);
    }

    clearFlush() {
      if (this.flushTimer) {
        clearTimeout(this.flushTimer);
        this.flushTimer = null;
      }
    }

    flush() {
      if (!this.pending.length) return;
      const ops = this.pending;
      this.pending = [];
      this.seq += 1;
      this.send('mirror.mutation', { ops }, this.seq);
    }
  }

  return Mirror;
})();
