// /cards page — upload + delete handlers.
//
// Card uploads are small (≤10MB by spec) so we use a single-shot multipart
// POST rather than the chunked resumable flow used for clip uploads. The
// server returns either:
//   - 200 with a card_warn_partial.html fragment (portrait detected) → swap
//     it into #card-upload-feedback so the user can confirm.
//   - 200 with a card_tile_partial.html fragment (success) → prepend it
//     to #cards-grid (or replace the empty-state if grid was empty).
//   - 400 with a small inline error fragment → swap into #card-upload-feedback.

function handleCardDrop(event, zone) {
  event.preventDefault();
  const files = event.dataTransfer.files;
  if (!files || !files.length) return;
  postCard(files[0]);
}

function uploadCardFile(input) {
  if (!input.files || !input.files.length) return;
  postCard(input.files[0]);
  input.value = '';
}

async function postCard(file, extraFields) {
  const fb = document.getElementById('card-upload-feedback');
  if (fb) fb.innerHTML = '<div class="card-upload-progress">Uploading…</div>';

  const fd = new FormData();
  fd.append('file', file);
  if (extraFields) {
    for (const [k, v] of Object.entries(extraFields)) fd.append(k, v);
  }

  const resp = await fetch('/cards', { method: 'POST', body: fd });
  const text = await resp.text();

  if (!resp.ok) {
    if (fb) fb.innerHTML = text;
    return;
  }

  // Distinguish warn-partial from tile-partial by the wrapping class.
  // Both are HTML fragments returned at status 200.
  if (text.indexOf('card-warn') !== -1) {
    if (fb) {
      fb.innerHTML = text;
      // The warn form has a hidden file input — wire the original File
      // object into it so resubmission with force=1 doesn't need a re-pick.
      const form = fb.querySelector('form');
      if (form) form.dataset.pendingFile = '';
      window._pendingCardFile = file;
    }
    return;
  }

  // Success: clear feedback, swap the new tile into the grid.
  if (fb) fb.innerHTML = '';
  insertNewTile(text);
  window._pendingCardFile = null;
}

// Called by the warn form's submit override — re-runs postCard with force=1
// instead of letting the form submit natively (which would lose the file).
function rebindCardForce(form) {
  if (!window._pendingCardFile) return true; // fall through to native submit
  const file = window._pendingCardFile;
  const fields = {
    name: form.querySelector('input[name="name"]').value,
    description: form.querySelector('input[name="description"]').value,
    force: '1',
  };
  // Prevent native submit; we drive the upload via fetch.
  setTimeout(() => postCard(file, fields), 0);
  return false;
}

function dismissCardWarn(btn) {
  const fb = document.getElementById('card-upload-feedback');
  if (fb) fb.innerHTML = '';
  window._pendingCardFile = null;
}

function insertNewTile(html) {
  const grid = document.getElementById('cards-grid');
  if (!grid) return;
  // If the grid currently shows the empty-state, replace the whole region.
  if (grid.querySelector('.card-grid-empty')) {
    grid.innerHTML =
      '<div class="card-grid" role="grid" aria-label="Card library">' +
      html +
      '</div>';
    return;
  }
  // Else prepend the new tile so newest-first ordering is visible.
  const inner = grid.querySelector('.card-grid');
  if (inner) inner.insertAdjacentHTML('afterbegin', html);
  else grid.innerHTML = html;
}

async function deleteCard(id, btn) {
  if (!confirm('Delete this card? This cannot be undone.')) return;

  if (btn) btn.disabled = true;
  const resp = await fetch('/cards/' + encodeURIComponent(id), { method: 'DELETE' });
  if (!resp.ok && resp.status !== 204) {
    if (btn) btn.disabled = false;
    alert('Delete failed: ' + resp.status);
    return;
  }

  // Remove the tile from the grid. If we just emptied the grid, refetch
  // so the empty-state placeholder renders correctly.
  const tile = document.querySelector('.card-tile[data-card-id="' + id + '"]');
  if (tile) tile.remove();

  const grid = document.getElementById('cards-grid');
  const remaining = grid ? grid.querySelectorAll('.card-tile').length : 0;
  if (grid && remaining === 0) {
    const fresh = await fetch('/cards/grid');
    if (fresh.ok) grid.innerHTML = await fresh.text();
  }
}
