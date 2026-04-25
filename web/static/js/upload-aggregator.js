// UploadAggregator — single source of truth for upload progress across N
// parallel clip uploads.
//
// Why this exists: each chunked PUT fires xhr.upload.onprogress with bytes
// uploaded *in the current request*. Those bytes are NOT committed until the
// PUT returns 308/200 with an acknowledged Range. On a failed chunk, the
// in-flight bytes have to be discarded; otherwise the UI claims more progress
// than GCS actually has, and the bar visibly regresses or freezes when we
// re-query GCS for the real offset.
//
// The aggregator separates state into:
//   - committed: bytes GCS has acknowledged (only goes up, except on resync
//     when GCS reports a smaller offset than we thought — rare but real)
//   - inflight:  bytes the current chunk's xhr.upload.onprogress has reported,
//     but not yet acked. Reset to 0 on chunk completion (becomes committed)
//     or chunk failure (discarded).
//
// Total displayed progress = sum(committed) + sum(inflight). On a single-tile
// view we can show per-file pct too, but the aggregate is what makes parallel
// uploads sane.
//
// This module is UMD-lite: usable as a CommonJS module under node:test AND
// as a browser global on `window.UploadAggregator`. No build step.

(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.UploadAggregator = factory();
  }
})(typeof self !== 'undefined' ? self : globalThis, function () {

  class UploadAggregator {
    constructor() {
      this.files = [];        // { id, name, total, committed, inflight, status }
      this.subscribers = [];  // listeners notified on state change
      this._nextId = 1;
    }

    /** Register a new file. Returns an opaque id used by setCommitted/setInflight/setStatus. */
    addFile(name, totalBytes) {
      const id = this._nextId++;
      this.files.push({
        id,
        name,
        total: totalBytes,
        committed: 0,
        inflight: 0,
        status: 'pending', // 'pending' | 'uploading' | 'done' | 'failed'
      });
      this._notify();
      return id;
    }

    /** Set committed bytes for a file. Inflight is reset (committed supersedes). */
    setCommitted(id, bytes) {
      const f = this._find(id);
      if (!f) return;
      // Clamp to [0, total]. GCS resync can return a smaller value than our
      // last committed (rare but documented), so we trust the new value.
      f.committed = Math.max(0, Math.min(f.total, bytes));
      f.inflight = 0;
      this._notify();
    }

    /** Set in-flight bytes for the file's current chunk. */
    setInflight(id, bytes) {
      const f = this._find(id);
      if (!f) return;
      // Inflight + committed must not exceed total — clamp inflight.
      const room = Math.max(0, f.total - f.committed);
      f.inflight = Math.max(0, Math.min(room, bytes));
      this._notify();
    }

    /** Discard in-flight bytes (used on chunk failure before retry/resync). */
    discardInflight(id) {
      const f = this._find(id);
      if (!f) return;
      if (f.inflight !== 0) {
        f.inflight = 0;
        this._notify();
      }
    }

    /** Mark final status. Done sets committed=total; failed leaves committed as last known. */
    setStatus(id, status) {
      const f = this._find(id);
      if (!f) return;
      f.status = status;
      f.inflight = 0;
      if (status === 'done') f.committed = f.total;
      this._notify();
    }

    /** Aggregate stats across all files. */
    stats() {
      let totalBytes = 0;
      let committed = 0;
      let inflight = 0;
      let pending = 0;
      let uploading = 0;
      let done = 0;
      let failed = 0;
      for (const f of this.files) {
        totalBytes += f.total;
        committed += f.committed;
        inflight += f.inflight;
        switch (f.status) {
          case 'pending':   pending++;   break;
          case 'uploading': uploading++; break;
          case 'done':      done++;      break;
          case 'failed':    failed++;    break;
        }
      }
      const loaded = committed + inflight;
      const pct = totalBytes > 0 ? Math.min(100, Math.round((loaded / totalBytes) * 100)) : 0;
      return {
        files: this.files.length,
        totalBytes, committed, inflight, loaded, pct,
        pending, uploading, done, failed,
      };
    }

    /** Get one file's snapshot (for per-tile rendering). */
    fileStats(id) {
      const f = this._find(id);
      if (!f) return null;
      const loaded = f.committed + f.inflight;
      const pct = f.total > 0 ? Math.min(100, Math.round((loaded / f.total) * 100)) : 0;
      return {
        id: f.id, name: f.name, total: f.total,
        committed: f.committed, inflight: f.inflight, loaded,
        pct, status: f.status,
      };
    }

    /** Subscribe to all state changes. Returns unsubscribe function. */
    subscribe(fn) {
      this.subscribers.push(fn);
      return () => {
        const i = this.subscribers.indexOf(fn);
        if (i >= 0) this.subscribers.splice(i, 1);
      };
    }

    _find(id) {
      return this.files.find((f) => f.id === id);
    }

    _notify() {
      // Snapshot stats once per notify so subscribers see consistent state.
      const s = this.stats();
      for (const fn of this.subscribers) {
        try { fn(s, this); } catch (e) { /* subscriber threw — don't break others */ }
      }
    }
  }

  return UploadAggregator;
});
