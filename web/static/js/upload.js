// Direct-to-GCS upload using resumable sessions.
//
// Flow per file:
//   1. Ask server for a signed init URL.
//   2. POST to the init URL with `x-goog-resumable: start` → GCS returns a
//      session URL in the Location header (valid 7 days).
//   3. Upload the file in chunks via PUT with `Content-Range`. Each chunk is
//      retried with exponential backoff; on fatal-looking errors we ask GCS
//      for the last acknowledged offset and resume from there.
//   4. Once all bytes are acknowledged, call /register so the server records
//      the clip and returns the updated list HTML.
//
// Why chunked + resumable:
//   Single-shot PUTs to a signed URL fail totally on any network blip — a
//   dealbreaker for multi-GB 4K phone clips on mobile networks. GCS resumable
//   lets us keep the bytes that already made it across.

// Multiple of 256 KiB (GCS requirement for non-final chunks). 32 MiB is a
// throughput sweet spot: big enough to amortize TLS/HTTP overhead, small
// enough to make a retry cheap on a slow link.
const CHUNK_SIZE = 32 * 1024 * 1024;
const MAX_CHUNK_RETRIES = 6;

function handleDrop(event, zone) {
  event.preventDefault();
  const files = event.dataTransfer.files;
  if (!files.length) return;
  uploadFilesArray(Array.from(files), zone.dataset.projectId, zone.dataset.listTarget);
}

function uploadFiles(input, projectId, target) {
  const files = Array.from(input.files);
  if (!files.length) return;
  uploadFilesArray(files, projectId, target);
  input.value = '';
}

async function uploadFilesArray(files, projectId, targetSelector) {
  const target = document.querySelector(targetSelector);

  const tiles = files.map((file) => {
    const tile = document.createElement('div');
    tile.className = 'clip-item uploading';
    tile.innerHTML =
      `<span class="clip-icon">⏳</span>` +
      `<span class="clip-name">Uploading ${escapeHtml(file.name)} ` +
      `<span class="clip-meta">(${formatBytes(file.size)})</span> ` +
      `<span class="clip-pct">0%</span> ` +
      `<span class="clip-rate"></span></span>`;
    if (target) target.appendChild(tile);
    return tile;
  });

  const results = await Promise.all(
    files.map((file, i) => uploadOne(file, projectId, tiles[i]).catch((err) => {
      console.error('Upload failed', file.name, err);
      tiles[i].innerHTML =
        `<span class="clip-icon">✗</span>` +
        `<span class="clip-name">Failed: ${escapeHtml(file.name)} ` +
        `<span class="clip-meta">— ${escapeHtml(err.message || 'network error')}</span></span>`;
      return null;
    }))
  );

  if (results.some(Boolean) && target) {
    const last = results.filter(Boolean).pop();
    target.innerHTML = last.html;
    if (window.htmx) htmx.process(target);
  }
}

async function uploadOne(file, projectId, tile) {
  const contentType = file.type || 'application/octet-stream';

  const signResp = await fetchWithRetry(`/projects/${projectId}/clips/sign`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ filename: file.name, contentType }),
  });
  if (!signResp.ok) throw new Error(`sign ${signResp.status}: ${await signResp.text()}`);
  const { initURL, objectName } = await signResp.json();

  const sessionURL = await initResumableSession(initURL, contentType);

  await uploadInChunks(sessionURL, file, contentType, tile);

  const regResp = await fetchWithRetry(`/projects/${projectId}/clips/register`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ filename: file.name, objectName }),
  });
  if (!regResp.ok) throw new Error(`register ${regResp.status}: ${await regResp.text()}`);

  return { html: await regResp.text() };
}

// Initiate the resumable session. The signed URL was minted for POST with an
// `x-goog-resumable: start` header; GCS replies 201 with the session URL in
// the Location response header.
async function initResumableSession(initURL, contentType) {
  let lastErr;
  for (let attempt = 0; attempt < MAX_CHUNK_RETRIES; attempt++) {
    try {
      const resp = await fetch(initURL, {
        method: 'POST',
        headers: {
          'Content-Type': contentType,
          'x-goog-resumable': 'start',
        },
      });
      if (resp.status === 201) {
        const loc = resp.headers.get('Location');
        if (!loc) throw new Error('init: missing Location header (check bucket CORS: expose Location)');
        return loc;
      }
      if (!isRetriableStatus(resp.status)) {
        throw new Error(`init ${resp.status}: ${await resp.text()}`);
      }
      lastErr = new Error(`init ${resp.status}`);
    } catch (err) {
      lastErr = err;
    }
    await sleep(backoffMs(attempt));
  }
  throw lastErr || new Error('init failed');
}

// Walk the file in CHUNK_SIZE pieces. On an irrecoverable-looking error,
// ask GCS where it left off and resume from that offset.
async function uploadInChunks(sessionURL, file, contentType, tile) {
  const total = file.size;
  const started = Date.now();
  let offset = 0;
  let failureCount = 0;

  while (offset < total) {
    const end = Math.min(offset + CHUNK_SIZE, total);
    const blob = file.slice(offset, end);

    let result;
    try {
      result = await putChunk(sessionURL, blob, offset, end, total, contentType, (loadedInChunk) => {
        updateProgress(tile, offset + loadedInChunk, total, started);
      });
    } catch (err) {
      if (err.fatal) throw err;
      // Transient error: resync with GCS and let the loop retry from there
      // rather than guessing or re-sending bytes that already made it across.
      failureCount++;
      if (failureCount > MAX_CHUNK_RETRIES) throw err;
      await sleep(backoffMs(failureCount));
      offset = await queryResumeOffsetWithRetry(sessionURL, total);
      updateProgress(tile, offset, total, started);
      continue;
    }
    failureCount = 0;

    if (result.kind === 'done') {
      updateProgress(tile, total, total, started);
      return;
    }
    // 'progress': GCS acknowledged through result.nextOffset - 1.
    offset = result.nextOffset;
    updateProgress(tile, offset, total, started);
  }
}

function putChunk(sessionURL, blob, start, end, total, contentType, onChunkProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('PUT', sessionURL, true);
    xhr.setRequestHeader('Content-Type', contentType);
    xhr.setRequestHeader('Content-Range', `bytes ${start}-${end - 1}/${total}`);

    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable && onChunkProgress) onChunkProgress(e.loaded);
    };
    xhr.onload = () => {
      if (xhr.status === 200 || xhr.status === 201) {
        resolve({ kind: 'done' });
        return;
      }
      if (xhr.status === 308) {
        // 308 Resume Incomplete: Range header = "bytes=0-<last byte stored>".
        const range = xhr.getResponseHeader('Range');
        const next = parseRangeEnd(range);
        resolve({ kind: 'progress', nextOffset: next !== null ? next + 1 : end });
        return;
      }
      if (isRetriableStatus(xhr.status)) {
        reject(Object.assign(new Error(`chunk ${xhr.status}`), { fatal: false }));
        return;
      }
      reject(Object.assign(new Error(`chunk ${xhr.status}: ${xhr.responseText}`), { fatal: true }));
    };
    xhr.onerror = () => reject(Object.assign(new Error('network error during chunk'), { fatal: false }));
    xhr.ontimeout = () => reject(Object.assign(new Error('chunk timeout'), { fatal: false }));
    xhr.send(blob);
  });
}

async function queryResumeOffsetWithRetry(sessionURL, total) {
  let lastErr;
  for (let attempt = 0; attempt < MAX_CHUNK_RETRIES; attempt++) {
    try {
      return await queryResumeOffset(sessionURL, total);
    } catch (err) {
      lastErr = err;
      await sleep(backoffMs(attempt));
    }
  }
  throw lastErr || new Error('resume query failed');
}

// Ask GCS how many bytes it has; send an empty PUT with `Content-Range: bytes */total`.
async function queryResumeOffset(sessionURL, total) {
  const xhr = new XMLHttpRequest();
  return new Promise((resolve, reject) => {
    xhr.open('PUT', sessionURL, true);
    xhr.setRequestHeader('Content-Range', `bytes */${total}`);
    xhr.onload = () => {
      if (xhr.status === 200 || xhr.status === 201) {
        resolve(total);
        return;
      }
      if (xhr.status === 308) {
        const range = xhr.getResponseHeader('Range');
        const next = parseRangeEnd(range);
        resolve(next !== null ? next + 1 : 0);
        return;
      }
      reject(new Error(`resume query ${xhr.status}`));
    };
    xhr.onerror = () => reject(new Error('resume query network error'));
    xhr.send();
  });
}

function parseRangeEnd(range) {
  // Format: "bytes=0-12345"
  if (!range) return null;
  const m = /bytes=\d+-(\d+)/.exec(range);
  return m ? parseInt(m[1], 10) : null;
}

function isRetriableStatus(status) {
  return status === 0 || status === 408 || status === 429 || (status >= 500 && status < 600);
}

function backoffMs(attempt) {
  // 500ms, 1s, 2s, 4s, 8s, 16s, with ±25% jitter.
  const base = 500 * Math.pow(2, attempt);
  const jitter = base * 0.25 * (Math.random() * 2 - 1);
  return Math.min(30000, Math.floor(base + jitter));
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

async function fetchWithRetry(url, opts) {
  let lastErr;
  for (let attempt = 0; attempt < MAX_CHUNK_RETRIES; attempt++) {
    try {
      const resp = await fetch(url, opts);
      if (resp.ok || !isRetriableStatus(resp.status)) return resp;
      lastErr = new Error(`${url} ${resp.status}`);
    } catch (err) {
      lastErr = err;
    }
    await sleep(backoffMs(attempt));
  }
  throw lastErr || new Error(`${url} failed`);
}

function updateProgress(tile, loaded, total, startedAt) {
  const pct = total > 0 ? Math.min(100, Math.round((loaded / total) * 100)) : 0;
  const pctEl = tile.querySelector('.clip-pct');
  if (pctEl) pctEl.textContent = `${pct}%`;
  const rateEl = tile.querySelector('.clip-rate');
  if (rateEl) {
    const elapsed = (Date.now() - startedAt) / 1000;
    if (elapsed > 0.5 && loaded > 0) {
      const bps = loaded / elapsed;
      const remaining = Math.max(0, (total - loaded) / bps);
      rateEl.textContent = `· ${formatBytes(bps)}/s · ${formatEta(remaining)} left`;
    }
  }
}

function formatBytes(n) {
  if (!isFinite(n) || n <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  return `${(n / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

function formatEta(seconds) {
  if (!isFinite(seconds)) return '—';
  if (seconds < 60) return `${Math.ceil(seconds)}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${Math.ceil(seconds % 60)}s`;
  return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`;
}

function escapeHtml(s) {
  return s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// Reload the page once the render poll transitions from running → done.
// Only true→false counts; the initial render-status element is polling=false,
// as is the one embedded in the editable manifest partial, and neither should
// trigger a reload.
let wasRenderPolling = false;
document.addEventListener('htmx:afterSwap', function (e) {
  const statusEl = document.getElementById('render-status');
  const isPolling = !!statusEl && statusEl.dataset.polling === 'true';
  if (wasRenderPolling && !isPolling) {
    setTimeout(() => window.location.reload(), 1500);
  }
  wasRenderPolling = isPolling;
});
