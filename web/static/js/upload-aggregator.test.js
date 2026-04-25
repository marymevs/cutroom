// Run with: node --test web/static/js/upload-aggregator.test.js
//
// These tests exercise the aggregator's separation of committed vs inflight
// bytes — the load-bearing invariant for the multi-clip progress bug.

const test = require('node:test');
const assert = require('node:assert');
const UploadAggregator = require('./upload-aggregator.js');

test('addFile registers file and aggregates total bytes', () => {
  const a = new UploadAggregator();
  a.addFile('a.mp4', 1000);
  a.addFile('b.mp4', 2000);
  const s = a.stats();
  assert.strictEqual(s.files, 2);
  assert.strictEqual(s.totalBytes, 3000);
  assert.strictEqual(s.loaded, 0);
  assert.strictEqual(s.pct, 0);
});

test('committed bytes update aggregate stats', () => {
  const a = new UploadAggregator();
  const id1 = a.addFile('a.mp4', 1000);
  const id2 = a.addFile('b.mp4', 1000);

  a.setCommitted(id1, 500);
  a.setCommitted(id2, 1000);

  const s = a.stats();
  assert.strictEqual(s.committed, 1500);
  assert.strictEqual(s.totalBytes, 2000);
  assert.strictEqual(s.pct, 75);
});

test('inflight bytes are added to loaded but tracked separately', () => {
  const a = new UploadAggregator();
  const id = a.addFile('a.mp4', 1000);

  a.setCommitted(id, 200);
  a.setInflight(id, 100);

  const s = a.stats();
  assert.strictEqual(s.committed, 200);
  assert.strictEqual(s.inflight, 100);
  assert.strictEqual(s.loaded, 300);
  assert.strictEqual(s.pct, 30);
});

test('setCommitted resets inflight (committed supersedes)', () => {
  const a = new UploadAggregator();
  const id = a.addFile('a.mp4', 1000);

  a.setInflight(id, 500);
  assert.strictEqual(a.stats().inflight, 500);

  a.setCommitted(id, 500);
  const s = a.stats();
  assert.strictEqual(s.committed, 500);
  assert.strictEqual(s.inflight, 0, 'inflight must reset when committed lands');
  assert.strictEqual(s.loaded, 500);
});

test('discardInflight zeros inflight (chunk failure → retry path)', () => {
  // CRITICAL REGRESSION TEST: this is the bug where the progress bar
  // appears to freeze or jump backwards when a chunk fails. We must NOT
  // count the in-flight bytes from a failed chunk as committed.
  const a = new UploadAggregator();
  const id = a.addFile('a.mp4', 1000);

  a.setCommitted(id, 200);
  a.setInflight(id, 300); // chunk uploading...
  assert.strictEqual(a.stats().loaded, 500);

  a.discardInflight(id); // chunk failed
  const s = a.stats();
  assert.strictEqual(s.committed, 200, 'committed should be unchanged');
  assert.strictEqual(s.inflight, 0, 'inflight discarded');
  assert.strictEqual(s.loaded, 200, 'loaded reflects only the truly committed bytes');
});

test('setCommitted clamps to [0, total]', () => {
  const a = new UploadAggregator();
  const id = a.addFile('a.mp4', 1000);

  a.setCommitted(id, -50);
  assert.strictEqual(a.stats().committed, 0);

  a.setCommitted(id, 1500);
  assert.strictEqual(a.stats().committed, 1000);
});

test('setInflight is clamped so committed+inflight <= total', () => {
  const a = new UploadAggregator();
  const id = a.addFile('a.mp4', 1000);

  a.setCommitted(id, 600);
  a.setInflight(id, 9999);

  const s = a.stats();
  assert.strictEqual(s.committed, 600);
  assert.strictEqual(s.inflight, 400, 'inflight clamped to remaining room');
  assert.strictEqual(s.loaded, 1000);
});

test('GCS resync downward (lower committed than last known) is honored', () => {
  // GCS resumable can occasionally report a lower offset than we thought,
  // for example when partial buffering happened. The aggregator trusts the
  // new value rather than locking the bar at a falsely-high percentage.
  const a = new UploadAggregator();
  const id = a.addFile('a.mp4', 1000);

  a.setCommitted(id, 800);
  a.setCommitted(id, 600); // GCS says only 600 acked
  assert.strictEqual(a.stats().committed, 600);
});

test('setStatus done sets committed = total and inflight = 0', () => {
  const a = new UploadAggregator();
  const id = a.addFile('a.mp4', 1000);

  a.setInflight(id, 200);
  a.setStatus(id, 'done');
  const s = a.stats();
  assert.strictEqual(s.committed, 1000);
  assert.strictEqual(s.inflight, 0);
  assert.strictEqual(s.done, 1);
  assert.strictEqual(s.pct, 100);
});

test('5-parallel-clip aggregate (the multi-clip bug regression)', () => {
  // CRITICAL REGRESSION TEST: the user reported "stops showing progress
  // before all clips have been uploaded" with multi-file uploads. This
  // exercise: 5 clips, each 1000 bytes, advancing committed bytes
  // independently — total reflects the sum and never goes backwards.
  const a = new UploadAggregator();
  const ids = [];
  for (let i = 0; i < 5; i++) ids.push(a.addFile(`clip-${i}.mp4`, 1000));

  let lastTotal = 0;
  let updates = 0;
  a.subscribe((s) => {
    updates++;
    assert.ok(s.loaded >= lastTotal,
      `loaded should not regress: was ${lastTotal}, now ${s.loaded}`);
    lastTotal = s.loaded;
  });

  // Walk every clip up in 200-byte committed jumps, interleaved.
  for (let step = 200; step <= 1000; step += 200) {
    for (const id of ids) {
      a.setCommitted(id, step);
    }
  }
  // Mark all as done.
  for (const id of ids) a.setStatus(id, 'done');

  const s = a.stats();
  assert.strictEqual(s.totalBytes, 5000);
  assert.strictEqual(s.committed, 5000);
  assert.strictEqual(s.done, 5);
  assert.strictEqual(s.pct, 100);
  assert.ok(updates > 0, 'subscribers should have been notified');
});

test('aborted-xhr-doesn-t-double-count (the secondary bug regression)', () => {
  // CRITICAL REGRESSION TEST: the xhr.upload.onprogress fires up to chunk
  // size on a chunk that ultimately fails. If we counted those as committed,
  // a retry would double-count when the chunk succeeds. The committed/
  // inflight split prevents this.
  const a = new UploadAggregator();
  const id = a.addFile('a.mp4', 1000);

  a.setCommitted(id, 0);
  a.setInflight(id, 400);    // chunk in flight
  a.discardInflight(id);     // chunk aborted (network drop, retry)

  // Retry: chunk succeeds.
  a.setInflight(id, 400);    // re-uploaded
  a.setCommitted(id, 400);   // chunk acked

  const s = a.stats();
  assert.strictEqual(s.committed, 400, 'no double-count from the aborted attempt');
  assert.strictEqual(s.inflight, 0);
  assert.strictEqual(s.loaded, 400);
});

test('subscribers receive consistent stats snapshot', () => {
  const a = new UploadAggregator();
  const seen = [];
  a.subscribe((s) => seen.push(s));

  a.addFile('a.mp4', 100);
  a.addFile('b.mp4', 100);
  a.setCommitted(1, 50);

  // 3 mutations should produce 3 notifications; each carries a stats object.
  assert.strictEqual(seen.length, 3);
  assert.strictEqual(seen[0].files, 1);
  assert.strictEqual(seen[1].files, 2);
  assert.strictEqual(seen[2].committed, 50);
});

test('unsubscribe stops notifications', () => {
  const a = new UploadAggregator();
  let count = 0;
  const off = a.subscribe(() => count++);

  a.addFile('a.mp4', 100);
  off();
  a.addFile('b.mp4', 200);
  assert.strictEqual(count, 1);
});

test('subscriber that throws does not break others', () => {
  const a = new UploadAggregator();
  let okCount = 0;
  a.subscribe(() => { throw new Error('boom'); });
  a.subscribe(() => { okCount++; });
  a.addFile('a.mp4', 100);
  assert.strictEqual(okCount, 1);
});

test('fileStats returns per-file snapshot', () => {
  const a = new UploadAggregator();
  const id = a.addFile('a.mp4', 1000);
  a.setCommitted(id, 250);
  a.setInflight(id, 100);

  const fs = a.fileStats(id);
  assert.strictEqual(fs.name, 'a.mp4');
  assert.strictEqual(fs.committed, 250);
  assert.strictEqual(fs.inflight, 100);
  assert.strictEqual(fs.loaded, 350);
  assert.strictEqual(fs.pct, 35);
  assert.strictEqual(fs.status, 'pending');
});

test('fileStats returns null for unknown id', () => {
  const a = new UploadAggregator();
  assert.strictEqual(a.fileStats(999), null);
});
