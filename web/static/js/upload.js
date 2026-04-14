// Handle drag-and-drop file upload
function handleDrop(event, zone) {
  event.preventDefault();
  const files = event.dataTransfer.files;
  if (!files.length) return;
  uploadFilesArray(Array.from(files), zone.getAttribute('hx-post'), zone.getAttribute('hx-target'));
}

// Handle file input change
function uploadFiles(input, url, target) {
  const files = Array.from(input.files);
  if (!files.length) return;
  uploadFilesArray(files, url, target);
}

async function uploadFilesArray(files, url, targetSelector) {
  const target = document.querySelector(targetSelector);

  for (const file of files) {
    const formData = new FormData();
    formData.append('clip', file);

    // Show uploading indicator
    const indicator = document.createElement('div');
    indicator.className = 'clip-item uploading';
    indicator.innerHTML = `<span class="clip-icon">⏳</span><span class="clip-name">Uploading ${file.name}…</span>`;
    if (target) target.appendChild(indicator);

    try {
      const resp = await fetch(url, { method: 'POST', body: formData });
      const html = await resp.text();
      if (target) {
        target.innerHTML = html;
        htmx.process(target); // re-process htmx attributes on new content
      }
    } catch (err) {
      indicator.innerHTML = `<span class="clip-icon">✗</span><span class="clip-name">Failed: ${file.name}</span>`;
      console.error('Upload failed', err);
    }
  }
}

// Poll for render status and refresh page when done
document.addEventListener('htmx:afterSwap', function(e) {
  const statusEl = document.getElementById('render-status');
  if (!statusEl) return;
  const status = statusEl.dataset.polling;
  if (status === 'false') {
    // Check if done — reload to show download buttons
    setTimeout(() => window.location.reload(), 1500);
  }
});
