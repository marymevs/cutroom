// Direct-to-GCS upload: sign → PUT to GCS → register with server.
// Bytes never pass through Cloud Run, so there's no 32 MiB request body cap.

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
}

async function uploadFilesArray(files, projectId, targetSelector) {
  const target = document.querySelector(targetSelector);

  // Upload all files in parallel. Each tile updates itself on success/failure,
  // then the last one to finish refreshes the clips list from the server.
  const tiles = files.map((file) => {
    const tile = document.createElement('div');
    tile.className = 'clip-item uploading';
    tile.innerHTML =
      `<span class="clip-icon">⏳</span>` +
      `<span class="clip-name">Uploading ${escapeHtml(file.name)}… <span class="clip-pct">0%</span></span>`;
    if (target) target.appendChild(tile);
    return tile;
  });

  const results = await Promise.all(
    files.map((file, i) => uploadOne(file, projectId, tiles[i]).catch((err) => {
      console.error('Upload failed', file.name, err);
      tiles[i].innerHTML =
        `<span class="clip-icon">✗</span>` +
        `<span class="clip-name">Failed: ${escapeHtml(file.name)}</span>`;
      return null;
    }))
  );

  // If at least one succeeded, refresh the clips list from the server so we
  // pick up canonical state (status, order, any server-side transforms).
  if (results.some(Boolean) && target) {
    const last = results.filter(Boolean).pop();
    target.innerHTML = last.html;
    if (window.htmx) htmx.process(target);
  }
}

async function uploadOne(file, projectId, tile) {
  // 1. Ask server for a signed PUT URL.
  const signResp = await fetch(`/projects/${projectId}/clips/sign`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ filename: file.name, contentType: file.type || 'application/octet-stream' }),
  });
  if (!signResp.ok) throw new Error(`sign ${signResp.status}: ${await signResp.text()}`);
  const { uploadURL, objectName, contentType } = await signResp.json();

  // 2. PUT bytes straight to GCS. Use XHR for progress events.
  await putWithProgress(uploadURL, file, contentType, (pct) => {
    const el = tile.querySelector('.clip-pct');
    if (el) el.textContent = `${pct}%`;
  });

  // 3. Register the clip — server re-signs a read URL and renders the list.
  const regResp = await fetch(`/projects/${projectId}/clips/register`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ filename: file.name, objectName }),
  });
  if (!regResp.ok) throw new Error(`register ${regResp.status}: ${await regResp.text()}`);

  return { html: await regResp.text() };
}

function putWithProgress(url, file, contentType, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('PUT', url, true);
    xhr.setRequestHeader('Content-Type', contentType);
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onProgress(Math.round((e.loaded / e.total) * 100));
    };
    xhr.onload = () => (xhr.status >= 200 && xhr.status < 300 ? resolve() : reject(new Error(`PUT ${xhr.status}: ${xhr.responseText}`)));
    xhr.onerror = () => reject(new Error('network error during PUT'));
    xhr.send(file);
  });
}

function escapeHtml(s) {
  return s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// Poll for render status and refresh page when done
document.addEventListener('htmx:afterSwap', function (e) {
  const statusEl = document.getElementById('render-status');
  if (!statusEl) return;
  const status = statusEl.dataset.polling;
  if (status === 'false') {
    setTimeout(() => window.location.reload(), 1500);
  }
});
