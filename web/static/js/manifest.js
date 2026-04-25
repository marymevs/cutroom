// Edit-plan review interactions: row delete, segment renumber, and the
// title-card picker popover.
//
// Why this is a separate file (not inlined in manifest_partial.html):
//   manifest_partial.html is HTMX-swapped on every Build Edit Plan and
//   Save Plan. If this script lived inline it would be promoted into the
//   page each swap and re-execute; top-level `let` declarations cannot be
//   redeclared in the same realm and the swap would silently throw a
//   SyntaxError. As a static file loaded once at page render, the
//   functions stay alive across swaps and the inline `onclick` handlers
//   in the swapped HTML find them on `window`.

(function () {

  function removeRow(btn) {
    const row = btn.closest('.plan-row');
    if (row) row.remove();
  }

  // Segments must carry no gaps in their order numbers — the form omits
  // the Order field entirely and the server reassigns 1..N by submission
  // order. This no-op hook exists so the UI can later renumber on delete
  // if needed.
  function renumberSegments(form) {
    const rows = form.querySelectorAll('.plan-row--segment .row-order');
    rows.forEach((el, i) => { el.textContent = String(i + 1); });
  }

  // ── Card picker popover (PR-5) ──────────────────────────────────────
  //
  // Each .plan-row--card has a button that opens a popover anchored to
  // it. The popover lazy-fetches /cards/grid (the library partial) on
  // first open per page-load, caches it in memory, and lets the user
  // pick a card by clicking a tile. Selection writes into the row's
  // hidden tc_image_id input AND swaps the inline thumbnail. Crucially,
  // no HTMX swap on the manifest form occurs — uncommitted segment edits
  // stay safe.
  let _cardLibraryHTML = null;
  let _activePickerRow = null;

  async function openCardPicker(triggerBtn) {
    closeCardPicker();
    const row = triggerBtn.closest('.plan-row--card');
    if (!row) return;
    _activePickerRow = row;

    if (_cardLibraryHTML === null) {
      const r = await fetch('/cards/grid');
      _cardLibraryHTML = r.ok ? await r.text() : '<div class="card-picker-error">Couldn\'t load library</div>';
    }

    const pop = document.createElement('div');
    pop.className = 'card-picker-popover';
    pop.innerHTML =
      '<div class="card-picker-toolbar">' +
        '<input type="text" class="card-picker-filter" placeholder="Filter cards…" oninput="filterCardPicker(this)" aria-label="Filter cards by name">' +
        '<a href="/cards" target="_blank" class="card-picker-upload-link">+ Upload new</a>' +
      '</div>' +
      '<div class="card-picker-grid">' + _cardLibraryHTML + '</div>';
    row.appendChild(pop);

    // Wire each tile's selection click. The card_tile_partial.html
    // template already has the data-card-id attribute we need, but its
    // built-in delete button is wrong context here — hide it inside
    // the picker.
    pop.querySelectorAll('.card-tile').forEach((tile) => {
      const del = tile.querySelector('.card-delete');
      if (del) del.style.display = 'none';
      tile.addEventListener('click', () => selectCard(tile));
    });

    document.addEventListener('keydown', _pickerEsc);
    setTimeout(() => document.addEventListener('click', _pickerOutsideClick), 0);
  }

  function _pickerEsc(e) { if (e.key === 'Escape') closeCardPicker(); }
  function _pickerOutsideClick(e) {
    if (_activePickerRow && !_activePickerRow.contains(e.target)) closeCardPicker();
  }
  function closeCardPicker() {
    document.removeEventListener('keydown', _pickerEsc);
    document.removeEventListener('click', _pickerOutsideClick);
    if (_activePickerRow) {
      const pop = _activePickerRow.querySelector('.card-picker-popover');
      if (pop) pop.remove();
    }
    _activePickerRow = null;
  }

  function filterCardPicker(input) {
    const q = input.value.toLowerCase();
    const grid = input.closest('.card-picker-popover').querySelector('.card-picker-grid');
    grid.querySelectorAll('.card-tile').forEach((tile) => {
      const name = (tile.querySelector('.card-name')?.textContent || '').toLowerCase();
      const desc = (tile.querySelector('.card-desc')?.textContent || '').toLowerCase();
      tile.style.display = (q === '' || name.includes(q) || desc.includes(q)) ? '' : 'none';
    });
  }

  function selectCard(tile) {
    if (!_activePickerRow) return;
    const id = tile.dataset.cardId;
    const name = tile.querySelector('.card-name')?.textContent || '';
    const img = tile.querySelector('img');
    const thumbSrc = img ? img.src : '';

    _activePickerRow.dataset.imageId = id;
    const hidden = _activePickerRow.querySelector('input[name="tc_image_id"]');
    if (hidden) hidden.value = id;

    // Replace the trigger button's thumb-wrap content with the chosen
    // tile preview.
    const wrap = _activePickerRow.querySelector('.card-picker-thumb-wrap');
    if (wrap) {
      if (thumbSrc) {
        wrap.innerHTML = '<img src="' + thumbSrc + '" alt="' + name + '" width="80" height="45">';
      } else {
        wrap.innerHTML = '<span class="card-picker-thumb-placeholder">' + name + '</span>';
      }
    }
    closeCardPicker();
  }

  // Expose the inline-onclick targets to window so the swapped manifest
  // HTML can find them. Keep this list in sync with the onclick="…"
  // attributes in manifest_partial.html and card_picker_partial.html.
  window.removeRow = removeRow;
  window.renumberSegments = renumberSegments;
  window.openCardPicker = openCardPicker;
  window.closeCardPicker = closeCardPicker;
  window.filterCardPicker = filterCardPicker;
  window.selectCard = selectCard;

})();
