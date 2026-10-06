// Local behaviours for the embedded UI. No inline scripts or eval required.
(function () {
  "use strict";
  var selected = null;
  var editing = null;
  var dialogOrigin = null;
  var resizing = null;
  var browsing = null;
  var columnPreferences = Object.create(null);
  var minColumnWidth = 72, maxColumnWidth = 1200;

  function grid() { return document.querySelector(".data-grid"); }
  function widthKey(table) {
    return "sqliteadmin.column-widths.v1:" + JSON.stringify([table.dataset.database, table.dataset.table]);
  }
  function applyWidths(table) {
    var widths = table.columnWidths;
    table.querySelectorAll("col").forEach(function (col, i) { col.style.width = widths[i] + "px"; });
    table.style.width = widths.reduce(function (sum, width) { return sum + width; }, 0) + "px";
    table.querySelectorAll(".column-resize").forEach(function (handle) {
      var width = widths[handle.closest("th").cellIndex];
      var limits = columnLimits(table, handle.closest("th").cellIndex);
      handle.setAttribute("aria-valuemin", String(limits.min));
      handle.setAttribute("aria-valuemax", String(limits.max));
      handle.setAttribute("aria-valuenow", String(width));
      handle.setAttribute("aria-valuetext", width + " pixels");
    });
  }
  function rememberWidths(table) {
    var saved = Object.create(null);
    table.querySelectorAll("th[data-column]").forEach(function (th) {
      saved[th.dataset.column] = table.columnWidths[th.cellIndex];
    });
    columnPreferences[widthKey(table)] = saved;
    // Storage may be unavailable in private/restricted browser sessions.
    try { localStorage.setItem(widthKey(table), JSON.stringify(saved)); } catch (_) {}
  }
  function initColumns(table) {
    if (table.columnWidths) return;
    var saved = columnPreferences[widthKey(table)];
    if (!saved) {
      try { saved = JSON.parse(localStorage.getItem(widthKey(table))); } catch (_) {}
    }
    table.columnWidths = Array.from(table.tHead.rows[0].cells).map(function (th, i) {
      if (i === 0) return 45;
      var width = saved && saved[th.dataset.column];
      if (!Number.isFinite(width)) width = Math.round(th.getBoundingClientRect().width);
      return Math.max(minColumnWidth, Math.min(maxColumnWidth, width));
    });
    table.classList.add("columns-sized");
    applyWidths(table);
  }
  function columnLimits(table, index) {
    var total = table.columnWidths[index] + table.columnWidths[index + 1];
    return Number.isFinite(total) ? {
      min: Math.max(minColumnWidth, total - maxColumnWidth),
      max: Math.min(maxColumnWidth, total - minColumnWidth)
    } : { min: minColumnWidth, max: maxColumnWidth };
  }
  function resizeColumn(table, index, width) {
    var widths = table.columnWidths;
    var limits = columnLimits(table, index);
    width = Math.max(limits.min, Math.min(limits.max, Math.round(width)));
    // An internal divider belongs to both adjacent columns. Keep the total
    // table width (and every subsequent divider) fixed while moving it.
    if (index + 1 < widths.length) widths[index + 1] += widths[index] - width;
    widths[index] = width;
    applyWidths(table);
  }
  function finishResize(save) {
    if (!resizing) return;
    var state = resizing;
    resizing = null;
    if (save) rememberWidths(state.table);
    else resizeColumn(state.table, state.index, state.width);
    state.handle.classList.remove("is-resizing");
    document.body.classList.remove("resizing-columns");
    if (state.handle.hasPointerCapture(state.pointer)) state.handle.releasePointerCapture(state.pointer);
  }

  document.addEventListener("pointerdown", function (e) {
    var handle = e.target.closest && e.target.closest(".column-resize");
    if (!handle || e.button !== 0 || resizing) return;
    e.preventDefault();
    var table = handle.closest("table");
    var index = handle.closest("th").cellIndex;
    resizing = { table: table, handle: handle, index: index, width: table.columnWidths[index],
      x: e.clientX, pointer: e.pointerId };
    handle.setPointerCapture(e.pointerId);
    handle.classList.add("is-resizing");
    document.body.classList.add("resizing-columns");
  });
  document.addEventListener("pointermove", function (e) {
    if (!resizing || e.pointerId !== resizing.pointer) return;
    resizeColumn(resizing.table, resizing.index, resizing.width + e.clientX - resizing.x);
  });
  ["pointerup", "pointercancel", "lostpointercapture"].forEach(function (type) {
    document.addEventListener(type, function (e) {
      if (resizing && e.pointerId === resizing.pointer) finishResize(type === "pointerup");
    });
  });
  function cells() { return Array.from(document.querySelectorAll(".data-grid [data-grid-cell]")); }
  function position(cell) {
    return { row: cell.parentElement.sectionRowIndex, col: cell.cellIndex };
  }
  function at(pos) {
    var table = grid();
    var row = table && table.tBodies[0].rows[pos.row];
    return row && row.cells[pos.col] && row.cells[pos.col].matches("[data-grid-cell]") ? row.cells[pos.col] : null;
  }
  function hint(text) {
    var el = document.getElementById("grid-hint");
    if (el) el.textContent = text;
  }
  function selectCell(cell, focus) {
    if (!cell || !cell.isConnected) return;
    cells().forEach(function (c) {
      c.classList.remove("is-selected");
      c.tabIndex = -1;
      c.setAttribute("aria-selected", "false");
      c.parentElement.classList.remove("row-selected");
    });
    selected = cell;
    cell.classList.add("is-selected");
    cell.parentElement.classList.add("row-selected");
    cell.tabIndex = 0;
    cell.setAttribute("aria-selected", "true");
    var label = document.getElementById("selected-column");
    var value = document.getElementById("selected-value");
    var edit = document.getElementById("edit-selected");
    if (label) label.textContent = cell.dataset.col;
    if (value) {
      value.textContent = cell.dataset.null === "true" ? "NULL" : cell.dataset.value || cell.textContent.trim();
      value.title = value.textContent;
    }
    if (edit) edit.disabled = !cell.dataset.editUrl;
    if (focus !== false) {
      cell.focus({ preventScroll: true });
      cell.scrollIntoView({ block: "nearest", inline: "nearest" });
    }
  }
  function initGrid() {
    var table = grid();
    if (!table) { selected = null; return; }
    initColumns(table);
    var n = Number(table.dataset.start) || 1;
    Array.from(table.tBodies[0].rows).forEach(function (row) {
      var number = row.querySelector(".row-number");
      if (number) number.textContent = row.classList.contains("inserted") ? "+" : n++;
    });
    if (!editing && (!selected || !selected.isConnected)) {
      selected = null;
      var label = document.getElementById("selected-column");
      var value = document.getElementById("selected-value");
      var edit = document.getElementById("edit-selected");
      if (label) label.textContent = "No cell selected";
      if (value) { value.textContent = "Select a cell to inspect its value"; value.removeAttribute("title"); }
      if (edit) edit.disabled = true;
      var first = table.querySelector("[data-grid-cell]");
      if (first) first.tabIndex = 0;
    }
  }
  function neighbour(cell, key, backwards) {
    var pos = position(cell);
    if (key === "ArrowUp" || key === "ArrowDown" || key === "Enter") {
      pos.row += key === "ArrowUp" || backwards ? -1 : 1;
      return at(pos);
    }
    if (key === "Home" || key === "End") {
      var row = cell.parentElement;
      return key === "Home" ? row.cells[1] : row.cells[row.cells.length - 1];
    }
    if (key === "ArrowLeft" || key === "ArrowRight") {
      pos.col += key === "ArrowLeft" ? -1 : 1;
      return at(pos);
    }
    var all = cells();
    return all[all.indexOf(cell) + (backwards ? -1 : 1)] || null;
  }
  function editorActions(show) {
    var actions = document.getElementById("grid-editor-actions");
    var edit = document.getElementById("edit-selected");
    if (actions) actions.hidden = !show;
    if (edit) edit.hidden = show;
  }
  function setNull(isNull) {
    if (!editing || editing.busy) return;
    editing.form.elements.null.value = isNull ? "1" : "";
    editing.form.classList.toggle("is-null", isNull);
    var button = document.querySelector("[data-grid-null]");
    if (button) button.setAttribute("aria-pressed", String(isNull));
    editing.input.placeholder = isNull ? "NULL" : "";
    if (isNull) editing.input.value = "";
    editing.input.focus();
  }
  function beginEdit(cell, replacement) {
    if (editing || browsing || !cell || !cell.dataset.editUrl) return;
    selectCell(cell, false);
    var template = document.getElementById("grid-editor-template");
    if (!template) return;
    var form = template.content.firstElementChild.cloneNode(true);
    form.setAttribute("hx-post", cell.dataset.editUrl);
    form.elements.key.value = cell.parentElement.dataset.rowKey;
    form.elements.col.value = cell.dataset.col;
    form.elements.null.value = cell.dataset.null === "true" ? "1" : "";
    var input = form.elements.value;
    input.value = cell.dataset.value || "";
    input.setAttribute("aria-label", "Edit " + cell.dataset.col);
    editing = { cell: cell, form: form, input: input, original: Array.from(cell.childNodes),
      value: input.value, isNull: form.elements.null.value, pos: position(cell), busy: false, next: null };
    cell.replaceChildren(form);
    cell.classList.add("editing");
    editorActions(true);
    setNull(form.elements.null.value === "1");
    // NULL has no text, but preserve non-null text (including line breaks).
    if (replacement !== undefined) {
      input.value = replacement;
      setNull(false);
    }
    window.htmx.process(form);
    input.focus();
    if (replacement === undefined) input.select();
    else input.setSelectionRange(input.value.length, input.value.length);
    hint("Enter to stage · Tab to stage and move · Shift+Enter for a new line · Esc to cancel");
  }
  function restoreEditor() {
    var state = editing;
    if (!state) return null;
    state.cell.replaceChildren.apply(state.cell, state.original);
    state.cell.classList.remove("editing");
    editing = null;
    editorActions(false);
    return state.cell;
  }
  function cancelEdit() {
    if (!editing || editing.busy) return;
    var cell = restoreEditor();
    selectCell(cell);
    hint("Edit cancelled. Previous staged changes are kept.");
  }
  function finishNext(state) {
    var next = state.next;
    var cell = next && next.pos ? at(next.pos) : at(state.pos);
    selectCell(cell, !(next && next.action));
    if (next && next.edit) beginEdit(cell);
    if (next && next.action) next.action();
  }
  function saveEdit(next) {
    if (!editing) return;
    if (editing.busy) { if (next) editing.next = next; return; }
    editing.next = next || null;
    if (editing.input.value === editing.value && editing.form.elements.null.value === editing.isNull) {
      var unchanged = editing;
      restoreEditor();
      finishNext(unchanged);
      if (!editing) hint("No change to stage.");
      return;
    }
    editing.form.requestSubmit();
  }
  function initBackupSchedule() {
    var form = document.querySelector("[data-backup-schedule]");
    if (!form || form.dataset.initialized) return;
    form.dataset.initialized = "true";
    var frequency = form.querySelector("[data-backup-frequency]");
    var select = frequency.querySelector("select");
    var interval = form.querySelector("[data-backup-interval]");
    var input = interval.querySelector("input");
    select.value = ["60", "1440", "10080"].includes(input.value) ? input.value : "custom";
    frequency.hidden = false;
    interval.hidden = select.value !== "custom";
    select.addEventListener("change", function () {
      interval.hidden = select.value !== "custom";
      if (select.value === "custom") input.focus();
      else input.value = select.value;
    });
  }
  function closeDialog() {
    var d = document.getElementById("dialog");
    // Closing a dialog cannot cancel a server-side restore already underway.
    // Keep its progress visible until the request succeeds or reports an error.
    if (d && d.querySelector(".htmx-request")) return;
    if (d) d.replaceChildren();
    if (dialogOrigin && dialogOrigin.isConnected) dialogOrigin.focus();
    dialogOrigin = null;
  }

  // Stage before navigating away or performing another action. Failed requests
  // leave the editor open with the typed text intact.
  document.addEventListener("click", function (e) {
    var t = e.target;
    if (!(t instanceof Element)) return;
    // Resizing is independent of sorting and must not stage an open editor.
    if (t.closest(".column-resize")) { e.preventDefault(); e.stopImmediatePropagation(); return; }
    if (t.closest("[data-grid-cancel]")) { e.preventDefault(); cancelEdit(); return; }
    if (t.closest("[data-grid-apply]")) { e.preventDefault(); saveEdit(); return; }
    if (t.closest("[data-grid-null]")) {
      e.preventDefault();
      if (editing) setNull(editing.form.elements.null.value !== "1");
      return;
    }
    if (t.closest("[data-grid-edit]")) { beginEdit(selected); return; }
    if (editing && !editing.form.contains(t)) {
      var other = t.closest("[data-grid-cell]");
      if (other === editing.cell) return;
      e.preventDefault();
      e.stopImmediatePropagation();
      if (other) saveEdit({ pos: position(other) });
      else {
        var action = t.closest("a, button, summary");
        var control = t.closest("input, textarea, select");
        saveEdit(action ? { action: function () { if (action.isConnected) action.click(); } } :
          control ? { action: function () { if (control.isConnected) control.focus(); } } : null);
      }
      return;
    }
    var cell = t.closest("[data-grid-cell]");
    if (cell && !t.closest(".cell-form")) selectCell(cell);
    if (t.closest("[data-close-dialog]") || t.classList.contains("overlay")) {
      e.preventDefault(); closeDialog(); return;
    }
    var dismiss = t.closest("[data-dismiss]");
    if (dismiss) { var f = dismiss.closest(".flash"); if (f) f.remove(); return; }
    var add = t.closest("[data-add-row]");
    if (add) {
      e.preventDefault();
      var tpl = document.getElementById(add.dataset.addRow);
      var body = document.getElementById(add.dataset.target);
      var i = Number(body.dataset.next || body.children.length);
      body.dataset.next = String(i + 1);
      body.insertAdjacentHTML("beforeend", tpl.innerHTML.split("__i__").join(String(i)));
      var first = body.lastElementChild && body.lastElementChild.querySelector("input");
      if (first) first.focus();
      return;
    }
    var rm = t.closest("[data-remove-row]");
    if (rm) { e.preventDefault(); rm.closest("tr").remove(); }
  }, true);

  document.addEventListener("dblclick", function (e) {
    var cell = e.target.closest && e.target.closest("[data-grid-cell]");
    if (cell) {
      e.preventDefault();
      if (editing && editing.cell !== cell) saveEdit({ pos: position(cell), edit: true });
      else beginEdit(cell);
    }
  });
  document.addEventListener("focusin", function (e) {
    if (e.target.matches("[data-grid-cell]") && !editing) selectCell(e.target, false);
  });
  document.addEventListener("submit", function (e) {
    if (editing && e.target !== editing.form) {
      e.preventDefault(); e.stopImmediatePropagation();
      var form = e.target;
      saveEdit({ action: function () { form.requestSubmit(); } });
    }
  }, true);
  document.addEventListener("keydown", function (e) {
    if (e.isComposing) return;
    if (resizing && e.key === "Escape") { e.preventDefault(); finishResize(false); return; }
    var active = document.activeElement;
    if (active && active.matches(".column-resize") && ["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key)) {
      e.preventDefault();
      var table = active.closest("table");
      var index = active.closest("th").cellIndex;
      var width = table.columnWidths[index];
      if (e.key === "Home") width = minColumnWidth;
      else if (e.key === "End") width = maxColumnWidth;
      else width += (e.key === "ArrowLeft" ? -1 : 1) * (e.shiftKey ? 50 : 10);
      resizeColumn(table, index, width);
      rememberWidths(table);
      return;
    }
    if (editing && (active === editing.input || active.closest("#grid-editor-actions"))) {
      if (e.key === "Escape") { e.preventDefault(); cancelEdit(); return; }
      if (e.key === "Tab" || (e.key === "Enter" && (!e.shiftKey || e.ctrlKey || e.metaKey))) {
        e.preventDefault();
        var target = neighbour(editing.cell, e.key, e.shiftKey);
        saveEdit(target ? { pos: position(target) } : null);
        return;
      }
    } else if (active && active.matches("[data-grid-cell]")) {
      if (["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown", "Home", "End", "Tab"].includes(e.key)) {
        var next = neighbour(active, e.key, e.shiftKey);
        if (next) { e.preventDefault(); selectCell(next); }
        return;
      }
      if (e.key === "Enter" || e.key === "F2") { e.preventDefault(); beginEdit(active); return; }
      if (e.key === "Backspace" || e.key === "Delete") { e.preventDefault(); beginEdit(active, ""); return; }
      if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
        e.preventDefault(); beginEdit(active, e.key); return;
      }
    }
    if (e.key === "Escape") { closeDialog(); return; }
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      var form = active && active.closest("form");
      if (form) { e.preventDefault(); form.requestSubmit(); }
    }
    // Keep keyboard focus inside an open modal.
    if (e.key === "Tab") {
      var dialog = document.querySelector("#dialog .dialog-card");
      if (dialog) {
        var focusables = Array.from(dialog.querySelectorAll("button:not(:disabled), input:not([type=hidden]), select, textarea, a[href]"));
        var first = focusables[0], last = focusables[focusables.length - 1];
        if (e.shiftKey && active === first) { e.preventDefault(); last.focus(); }
        else if (!e.shiftKey && active === last) { e.preventDefault(); first.focus(); }
      }
    }
  });
  document.addEventListener("copy", function (e) {
    var cell = document.activeElement;
    if (cell && cell.matches("[data-grid-cell]") && e.clipboardData) {
      e.clipboardData.setData("text/plain", cell.dataset.null === "true" ? "" : cell.dataset.value || cell.textContent.trim());
      e.preventDefault(); hint("Cell value copied.");
    }
  });
  document.addEventListener("paste", function (e) {
    var cell = document.activeElement;
    if (cell && cell.matches("[data-grid-cell]") && cell.dataset.editUrl && e.clipboardData) {
      e.preventDefault(); beginEdit(cell, e.clipboardData.getData("text/plain"));
    }
  });
  document.addEventListener("input", function (e) {
    var t = e.target;
    if (!(t instanceof Element)) return;
    if (editing && t === editing.input) {
      editing.form.elements.null.value = "";
      editing.form.classList.remove("is-null");
      var nullButton = document.querySelector("[data-grid-null]");
      if (nullButton) nullButton.setAttribute("aria-pressed", "false");
    }
    if (t.matches("[data-confirm-match]")) {
      var btn = t.form && t.form.querySelector("[data-confirm-target]");
      if (btn) btn.disabled = t.value !== t.dataset.confirmMatch;
    }
    if (t.matches("[data-mode-for]") && t.value !== "") {
      var sel = t.form && t.form.querySelector('select[name="' + t.dataset.modeFor + '"]');
      if (sel) sel.value = "value";
    }
    if (t.id === "sidebar-filter") {
      var q = t.value.toLowerCase();
      var any = false;
      document.querySelectorAll(".sidebar .objlist li").forEach(function (li) {
        li.hidden = q !== "" && !li.textContent.toLowerCase().includes(q);
        if (!li.hidden) any = true;
      });
      var empty = document.getElementById("sidebar-empty");
      if (empty) empty.hidden = any || q === "";
    }
  });
  document.addEventListener("htmx:beforeRequest", function (e) {
    if (e.detail.target && e.detail.target.id === "table-browser") {
      var table = grid();
      var header = e.detail.elt.closest("th[data-column]");
      browsing = { xhr: e.detail.xhr, scroll: table.parentElement.scrollLeft,
        focusColumn: header ? header.dataset.column : null };
      rememberWidths(table);
      hint("Updating table…");
    }
    if (editing && e.detail.elt === editing.form) {
      editing.xhr = e.detail.xhr;
      editing.busy = true;
      editing.input.readOnly = true;
      hint("Staging edit…");
    }
    if (e.detail.target && e.detail.target.id === "dialog" && !document.querySelector("#dialog .dialog-card")) {
      dialogOrigin = document.activeElement;
    }
  });
  document.addEventListener("htmx:afterSwap", function (e) {
    initGrid();
    initBackupSchedule();
    if (browsing && e.detail.target && e.detail.target.id === "table-browser") {
      grid().parentElement.scrollLeft = browsing.scroll;
      var header = Array.from(grid().querySelectorAll("th[data-column]")).find(function (th) {
        return th.dataset.column === browsing.focusColumn;
      });
      if (header) header.querySelector(".column-sort").focus({ preventScroll: true });
    }
    if (e.detail.target && e.detail.target.id === "dialog") {
      var card = document.querySelector("#dialog .dialog-card");
      if (card) {
        card.setAttribute("role", "dialog");
        card.setAttribute("aria-modal", "true");
        var heading = card.querySelector("h3");
        if (heading) { heading.id = "dialog-heading"; card.setAttribute("aria-labelledby", heading.id); }
        var focus = card.querySelector("[autofocus], input:not([type=hidden]), select, textarea, button");
        if (focus) focus.focus();
      }
    }
  });
  document.addEventListener("htmx:afterRequest", function (e) {
    var elt = e.detail.elt;
    if (browsing && e.detail.xhr === browsing.xhr) {
      browsing = null;
      if (!e.detail.successful || e.detail.xhr.getResponseHeader("HX-Retarget")) {
        hint("Could not update the table. Try again.");
      }
    }
    if (editing && e.detail.xhr === editing.xhr) {
      var state = editing;
      var xhr = e.detail.xhr;
      if (e.detail.successful && !xhr.getResponseHeader("HX-Retarget") && !xhr.getResponseHeader("HX-Redirect")) {
        editing = null;
        editorActions(false);
        initGrid();
        finishNext(state);
        if (!editing) hint("Edit staged. Review and commit when you’re ready.");
      } else {
        state.busy = false;
        state.next = null;
        state.input.readOnly = false;
        state.input.focus();
        hint("Could not stage the edit. Your text is kept; try again or press Esc.");
      }
      return;
    }
    if (e.detail.successful && elt.closest && elt.closest("#dialog") && e.detail.target && e.detail.target.id !== "dialog" && !e.detail.xhr.getResponseHeader("HX-Retarget")) closeDialog();
  });
  window.addEventListener("beforeunload", function (e) {
    if (editing && (editing.input.value !== editing.value || editing.form.elements.null.value !== editing.isNull)) {
      e.preventDefault(); e.returnValue = "";
    }
  });
  document.addEventListener("htmx:historyRestore", function () {
    selected = null;
    editing = null;
    browsing = null;
    editorActions(false);
    initGrid();
    var schedule = document.querySelector("[data-backup-schedule]");
    if (schedule) delete schedule.dataset.initialized;
    initBackupSchedule();
  });
  initGrid();
  initBackupSchedule();
})();
