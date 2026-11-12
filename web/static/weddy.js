// Meteor's runtime config, as far as WeKan's pages and tests read it: Weddy
// is served at the root of its origin, and every script it loads comes from
// that same origin - there is no ROOT_URL host to load code from.
window.__meteor_runtime_config__ = window.__meteor_runtime_config__ || {
  ROOT_URL_PATH_PREFIX: '',
  PUBLIC_SETTINGS: { packages: { 'dynamic-import': { useLocationOrigin: true } } },
};
// Weddy's WeKan in the browser: Unpoly for the pages, Server-Sent Events for what
// other people change, and interact.js for MultiDrag - every finger, or the mouse,
// drags its own card at the same time, and a card dragged while others are
// selected (Ctrl/Cmd-click) takes them along. From wekan/wami's public/multidrag.
(function () {
  'use strict';

  var dragging = 0;   // drags in progress, over all pointers
  var itemDrag = null, itemDragJustEnded = false; // a checklist item pressed or dragged
  var stale = false;  // the board changed during a drag: reload when the last ends

  // This page, as the server names it in an event it caused (X-Weddy-Page).
  var pageId = Array.prototype.map.call((window.crypto && crypto.getRandomValues) ? crypto.getRandomValues(new Uint8Array(12)) : [], function (n) {
    return 'abcdefghijklmnopqrstuvwxyz0123456789'[n % 36];
  }).join('') || String(Date.now());
  // A form sent for the board is answered with the board, so this page names
  // itself in it and skips the server's echo of the same change - a second
  // refresh replaced what the member was already pointing at. A script's own
  // call (callAsync) does not name the page: it relies on the refresh.
  up.on('up:request:load', function (event) {
    var req = event.request;
    if (req && req.method !== 'GET' && /\.board-wrapper/.test(String(req.target || ''))) req.headers['X-Weddy-Page'] = pageId;
  });
  var staleUnderPopOver = false; // the board changed while a pop-over was open
  var staleUnderForm = false;    // ... or while an inlined form was open
  // An inlined form closed: hidden, and a templated one's fields taken away.
  function closeInlinedForm(form) {
    form.classList.add('is-hidden');
    if (!form.querySelector(':scope > template.js-inlined-form-body')) return;
    Array.prototype.slice.call(form.childNodes).forEach(function (n) {
      if (!(n.matches && n.matches('template.js-inlined-form-body'))) form.removeChild(n);
    });
  }
  function reloadBoard(underPopOver) {
    // A drag in progress - or a press on a checklist item that may become
    // one - is not swept away: the board catches up after it.
    if (dragging > 0 || itemDrag) { stale = true; return; }
    // An open pop-over is not swept away by somebody else's change: the board
    // catches up when it closes. A pop-over that changes how the board is
    // drawn (Card Settings) asks for the board under it at once: the
    // pop-over is outside the board, so it stays.
    if (!underPopOver && document.querySelector('.js-pop-over')) { staleUnderPopOver = true; return; }
    // Nor is an inlined form opened in the card details (WeKan's inlinedForm
    // stays as its page changes around it): the board catches up when it
    // closes. The add-list form is always there, so it is not one of these.
    if (document.querySelector('.board-wrapper .js-card-details .js-inlined-form:not(.is-hidden)')) { staleUnderForm = true; return; }
    // A change of this page's own - a form still on its way - is announced
    // before its answer arrives: reload after it, or the older page wins.
    if (up.network.isBusy()) { setTimeout(function () { reloadBoard(underPopOver); }, 150); return; }
    stale = false;
    if (!document.querySelector('.board-wrapper')) return;
    // What the user opened in the card details - a <details> such as the
    // Scrum fields' editor - stays open through somebody else's change: the
    // same ones, by their place, are opened again in the new page.
    var all = function () { return Array.prototype.slice.call(document.querySelectorAll('.board-wrapper .js-card-details details')); };
    var card = document.querySelector('.board-wrapper .js-card-details');
    var open = all().map(function (d, i) { return d.open ? i : -1; }).filter(function (i) { return i >= 0; });
    var cardID = card && card.dataset.card;
    var p = up.reload('.board-wrapper');
    if (open.length && p && p.then) p.then(function () {
      var now = document.querySelector('.board-wrapper .js-card-details');
      if (!now || now.dataset.card !== cardID) return;
      var list = all();
      open.forEach(function (i) { if (list[i]) list[i].open = true; });
    }, function () {});
  }

  // A form sent is answered with the whole board: what was waiting is in it.
  up.on('up:form:submit', function () { staleUnderForm = false; });
  // WeKan's submit-on-Enter (models/lib/editorSubmitKey.js, editorSubmit.js):
  // in every editor, with the member's preference Enter saves and
  // Shift+Enter is a new line; without it Enter is a new line and
  // Ctrl/Cmd+Enter saves. Enter that ends an IME composition never saves.
  // The preference as the newest page part says it: the board is re-rendered
  // on each card opened, the body only on a full load.
  function submitOnEnter(el) {
    var at = (el && el.closest && el.closest('[data-submit-on-enter]')) || document.querySelector('.board-wrapper[data-submit-on-enter]') || document.body;
    return !!at && at.getAttribute('data-submit-on-enter') === 'true';
  }
  function isSubmitKey(event) {
    if (event.defaultPrevented || event.isComposing || event.keyCode === 229) return false;
    if (event.key !== 'Enter' && event.keyCode !== 13) return false;
    return submitOnEnter(event.target) ? !event.shiftKey : !!(event.ctrlKey || event.metaKey);
  }
  document.addEventListener('keydown', function (event) {
    var editor = event.target;
    if (!editor || editor.tagName !== 'TEXTAREA' || editor.readOnly || editor.disabled || !editor.form || !isSubmitKey(event)) return;
    var button = Array.prototype.find.call(editor.form.elements, function (c) {
      return (c.tagName === 'BUTTON' || c.tagName === 'INPUT') && c.type === 'submit' && !c.disabled && c.getClientRects().length;
    });
    event.preventDefault();
    event.stopPropagation();
    if (button) button.click();
    else if (editor.form.requestSubmit) editor.form.requestSubmit();
  });
  // WeKan's multilineTitles.creationTitles: the composer's text as one title,
  // or - "Many" chosen - one a line, blank lines left out.
  function creationTitles(value, separate) {
    var title = String(value || '').replace(/\r\n?/g, '\n').trim();
    if (!separate) return title ? [title] : [];
    return title.split('\n').map(function (l) { return l.trim(); }).filter(Boolean);
  }
  // WeKan's inline Add List composer (#6465): the list header's + opens it
  // after that list; it adds the lists right after it, in order, through
  // createListAfter.
  function closeAddListInline() {
    document.querySelectorAll('.js-add-list-inline').forEach(function (n) { n.remove(); });
  }
  up.on('click', '.js-add-list-here', function (event) {
    event.preventDefault();
    event.stopPropagation();
    closeAddListInline();
    var list = this.closest('.js-list');
    var tpl = document.querySelector('template.js-add-list-inline-template');
    if (!list || !tpl) return;
    var node = tpl.content.firstElementChild.cloneNode(true);
    node.dataset.after = this.dataset.list;
    node.dataset.swimlane = this.dataset.swimlane || '';
    var group = 'title-mode-' + Math.random().toString(36).slice(2);
    node.querySelectorAll('.js-multiline-title-mode').forEach(function (r) { r.name = group; });
    list.after(node);
    node.querySelector('textarea').focus();
  });
  up.on('click', '.js-close-add-list-inline', function (event) {
    event.preventDefault();
    closeAddListInline();
  });
  document.addEventListener('submit', function (event) {
    var form = event.target;
    if (!form.matches || !form.matches('.js-add-list-inline-form')) return;
    event.preventDefault();
    var node = form.closest('.js-add-list-inline');
    var board = document.querySelector('.board-wrapper[data-board]');
    var input = form.querySelector('.list-name-input');
    var mode = form.querySelector('.js-multiline-title-mode:checked');
    var titles = creationTitles(input.value, mode && mode.value === 'separate');
    if (!titles.length || form.dataset.busy || !board) return;
    form.dataset.busy = '1';
    input.disabled = true;
    var after = node.dataset.after || null;
    var done = 0;
    var next = function () {
      if (done >= titles.length) return Promise.resolve();
      return callAsync('createListAfter', { title: titles[done], boardId: board.dataset.board,
        swimlaneId: node.dataset.swimlane || null, afterListId: after, type: 'list' }).then(function (id) {
        after = id;
        done += 1;
        return next();
      });
    };
    next().then(function () {
      closeAddListInline();
      up.reload('.board-wrapper');
    }, function () {
      // What was not added stays in the composer, as WeKan's.
      input.value = titles.slice(done).join('\n');
      input.disabled = false;
      delete form.dataset.busy;
      input.focus();
    });
  }, true);
  // Member Settings' toggle: the profile, and this page, at once.
  up.on('click', '.js-change-settings', function (event) {
    event.preventDefault();
    openPopOver('change-settings');
    var c = document.querySelector('.js-pop-over .js-submit-on-enter-check');
    if (c) c.hidden = !submitOnEnter();
  });
  up.on('click', '.js-pop-over .js-toggle-submit-on-enter', function (event) {
    event.preventDefault();
    var on = !submitOnEnter();
    callAsync('toggleSubmitOnEnter').then(function () {
      document.querySelectorAll('[data-submit-on-enter]').forEach(function (e) { e.setAttribute('data-submit-on-enter', on ? 'true' : ''); });
      var c = document.querySelector('.js-pop-over .js-submit-on-enter-check');
      if (c) c.hidden = !on;
    });
  });
  // Other users' changes arrive as "board-changed".
  up.compiler('.board-wrapper[data-live]', function (el) {
    if (typeof EventSource === 'undefined') return;
    var es = new EventSource(el.getAttribute('data-live'));
    es.addEventListener('board-changed', function (e) {
      // This page's own change (publishFrom) is on it already.
      if (String(e.data || '').split(' ')[1] === pageId) return;
      reloadBoard();
    });
    return function () { es.close(); };
  });

  // WeKan's sidebar and pop-overs: the sidebar opens and closes, the board menu
  // opens from it, and one pop-over is open at a time.
  // As in WeKan, there is one pop-over at a time: opening one makes it from
  // its <template>, closing it takes it away.
  function closePopOvers() {
    var had = document.querySelector('.js-pop-over');
    document.querySelectorAll('.js-pop-over').forEach(function (p) { p.remove(); });
    if (had && staleUnderPopOver) {
      staleUnderPopOver = false;
      setTimeout(reloadBoard, 0);
    }
  }
  function openPopOver(name) {
    closePopOvers();
    var t = document.querySelector('template.js-pop-over-template[data-pop="' + name + '"]');
    if (!t) return;
    var p = document.createElement('div');
    p.className = 'pop-over js-pop-over';
    p.setAttribute('role', 'dialog');
    p.setAttribute('aria-label', t.getAttribute('data-title') || '');
    // WeKan's pop-over names itself by its template (popup.jade data-popup).
    if (t.dataset.popup) p.setAttribute('data-popup', t.dataset.popup);
    p.appendChild(t.content.cloneNode(true));
    document.body.appendChild(p);
    up.hello(p);
  }
  // The sidebar stays as the user left it - open or not, its activities shown
  // or not - while the board reloads under it, as WeKan's does: kept for the tab.
  function sidebarState(key, value) {
    try {
      if (value === undefined) return sessionStorage.getItem('weddy-sidebar-' + key) === '1';
      sessionStorage.setItem('weddy-sidebar-' + key, value ? '1' : '0');
    } catch (e) { /* no storage: the sidebar just closes on reload */ }
    return false;
  }
  up.on('click', '.js-toggle-page-sidebar', function (event) {
    event.preventDefault();
    var sb = document.querySelector('.board-sidebar');
    if (sb) {
      var open = sb.classList.toggle('is-open');
      sidebarState('open', open);
      if (!open) sidebarState('archives', false);
    }
  });
  up.on('click', '.js-close-sidebar', function (event, el) {
    event.preventDefault();
    el.closest('.board-sidebar').classList.remove('is-open');
    sidebarState('open', false);
    sidebarState('archives', false);
    sidebarState('filter', false);
  });
  // WeKan's header Create Board icon: the pop-over, its title to type into.
  up.on('click', '.js-create-board', function (event) {
    event.preventDefault();
    openPopOver('create-board');
    var t = document.querySelector('.js-pop-over .js-new-board-title');
    if (t) t.focus();
  });
  // WeKan's Popup.open(name) and Popup.close(), for what opens a pop-over by
  // its WeKan name: the Weddy pop-over whose template has that data-popup.
  if (!window.Popup) {
    window.Popup = {
      open: function (name) {
        return function () {
          var t = document.querySelector('template.js-pop-over-template[data-popup="' + name + 'Popup"]');
          if (t) openPopOver(t.dataset.pop);
        };
      },
      close: function () { closePopOvers(); },
    };
  }
  // Board Settings / Card (WeKan's boardCardSettingsPopup).
  up.on('click', '.js-open-board-card-settings', function (event) {
    event.preventDefault();
    openPopOver('board-card-settings');
  });
  // A row's checkbox: the board's setting for that side (/boards/update, as
  // WeKan's Boards.update), a Scrum field's visibility (scrum.configure), or
  // the user's own labels text - each cycling as WeKan's does.
  function setCardSettingsBox(box, on) {
    box.classList.toggle('is-checked', on);
    box.setAttribute('aria-checked', on ? 'true' : 'false');
    var m = box.querySelector('.materialCheckBox');
    if (m) m.classList.toggle('is-checked', on);
  }
  up.on('click', '.js-card-settings-toggle', function (event, box) {
    event.preventDefault();
    var row = box.closest('.js-card-field-order-row');
    var pane = box.closest('.js-card-settings-pane');
    var on = !box.classList.contains('is-checked');
    var d = row.dataset, done;
    if (d.kind === 'scrum') {
      var vis = {}; vis[d.scrum] = on;
      done = window.Meteor.callAsync('scrum.configure', pane.dataset.board, { visibility: vis }, Number(pane.dataset.revision) || 0).then(function () {
        document.querySelectorAll('.js-card-settings-pane').forEach(function (p) { p.dataset.revision = String((Number(p.dataset.revision) || 0) + 1); });
      });
    } else if (d.kind === 'personal') {
      // Shown follows the override: shown, then hidden, then the board's.
      done = d.side === 'minicard'
        ? window.Meteor.callAsync('setShowLabelTextOverride', on)
        : window.Meteor.callAsync('/users/update', { _id: window.Meteor.userId() }, { $set: { 'profile.cardLabelTextOverride': on } });
    } else {
      var set = {}; set[d.field] = on;
      done = window.Meteor.callAsync('/boards/update', { _id: pane.dataset.board }, { $set: set });
    }
    done.then(function () {
      setCardSettingsBox(box, on);
      reloadBoard(true);
    }, function (err) { window.alert((err && (err.reason || err.message)) || String(err)); });
  });
  // A section's checkbox: the whole section shown on that side or not.
  up.on('click', '.js-card-settings-section-shown', function (event, box) {
    event.preventDefault();
    var pane = box.closest('.js-card-settings-pane');
    var on = !box.classList.contains('is-checked');
    var hidden = Array.prototype.slice.call(pane.querySelectorAll('.js-card-settings-section'))
      .filter(function (sec) {
        var b = sec.querySelector('.js-card-settings-section-shown');
        return b === box ? !on : !b.classList.contains('is-checked');
      }).map(function (sec) { return sec.dataset.section; });
    var set = {};
    set[pane.dataset.side === 'minicard' ? 'cardSettingsHiddenOnMinicard' : 'cardSettingsHiddenOnCard'] = hidden;
    window.Meteor.callAsync('/boards/update', { _id: pane.dataset.board }, { $set: set }).then(function () {
      setCardSettingsBox(box, on);
      box.closest('.js-card-settings-section').classList.toggle('is-section-hidden', !on);
      reloadBoard(true);
    }, function (err) { window.alert((err && (err.reason || err.message)) || String(err)); });
  });
  up.on('click', '.js-open-board-menu', function (event) {
    event.preventDefault();
    openPopOver('board-menu');
  });
  // The Export pop-over's "what to include" (WeKan's exportScope.js): one
  // selection for the page, kept as the pop-over is closed and reopened, and
  // carried by every tool link's ?fields=.
  var exportOff = {};
  function applyExportSelection(p) {
    if (!p) return;
    var fields = [];
    p.querySelectorAll('.js-export-card-details-toggle, .js-export-field-toggle').forEach(function (a) {
      var field = a.classList.contains('js-export-card-details-toggle') ? 'card-details' : a.dataset.field;
      var on = !exportOff[field];
      a.setAttribute('aria-checked', on ? 'true' : 'false');
      var box = a.querySelector('.materialCheckBox');
      if (box) box.classList.toggle('is-checked', on);
      if (on) fields.push(field);
    });
    p.querySelectorAll('a.js-export-fields').forEach(function (a) {
      var url = new URL(a.getAttribute('href'), location.href);
      url.searchParams.set('fields', fields.join(','));
      a.setAttribute('href', url.pathname + url.search);
    });
  }
  up.on('click', '.js-export-board', function (event) {
    event.preventDefault();
    openPopOver('export');
    applyExportSelection(document.querySelector('.js-pop-over'));
  });
  // #1173: a swimlane's, a list's or a card's Export and Import pop-overs
  // (exportScopeBody): the same selection, the scope's files, or a WeKan
  // export brought in beside it.
  var scope = null;
  var menuLane = null;
  function openScope(mode, kind, target) {
    scope = { kind: kind, target: target };
    openPopOver(mode + '-scope');
    var p = document.querySelector('.js-pop-over');
    var t = document.querySelector('template.js-pop-over-template[data-pop="' + mode + '-scope"]');
    if (!p || !t) return;
    var title = t.getAttribute('data-title-' + kind) || '';
    p.querySelector('.js-scope-title').textContent = title;
    p.setAttribute('aria-label', title);
    var board = t.getAttribute('data-board');
    p.querySelectorAll('a.js-scope-link').forEach(function (a) {
      var path = a.getAttribute('data-path');
      var url = '/api/boards/' + encodeURIComponent(board) + '/' + path;
      var q = new URLSearchParams(a.getAttribute('data-query') || '');
      // A card's PDF and Excel are the card's own (exportScope.js routeFor).
      if (kind === 'card' && (path === 'exportPDF' || path === 'exportExcel')) {
        url = '/api/boards/' + encodeURIComponent(board) + '/lists/' + encodeURIComponent(target.listId) + '/cards/' + encodeURIComponent(target.cardId) + '/' + path;
      } else {
        Object.keys(target).forEach(function (k) { if (target[k]) q.set(k, target[k]); });
      }
      q.set('fields', '');
      a.setAttribute('href', url + '?' + q.toString());
    });
    applyExportSelection(p);
  }
  up.on('click', '.js-open-swimlane-menu', function (event, el) {
    event.preventDefault();
    menuLane = el.getAttribute('data-swimlane');
    openPopOver('swimlane-menu');
  });
  up.on('click', '.js-pop-over .js-export-swimlane, .js-pop-over .js-import-swimlane', function (event, el) {
    event.preventDefault();
    openScope(el.classList.contains('js-export-swimlane') ? 'export' : 'import', 'swimlane', { swimlaneId: menuLane });
  });
  up.on('click', '.js-pop-over .js-export-list, .js-pop-over .js-import-list', function (event, el) {
    event.preventDefault();
    if (!menuList) return;
    openScope(el.classList.contains('js-export-list') ? 'export' : 'import', 'list', { listId: menuList.dataset.list });
  });
  up.on('click', '.js-pop-over .js-export-card, .js-pop-over .js-import-card', function (event, el) {
    event.preventDefault();
    openScope(el.classList.contains('js-export-card') ? 'export' : 'import', 'card', { cardId: el.dataset.card, listId: el.dataset.list });
  });
  // A checklist's menu (checklistActionsPopup) and its Export
  // (exportChecklistPopup): the same body, the checklist as its scope.
  var menuChecklist = null;
  up.on('click', '.js-open-checklist-details-menu', function (event, el) {
    event.preventDefault();
    menuChecklist = el.getAttribute('data-checklist');
    openPopOver('checklist-actions');
  });
  // checklistDeletePopup: the confirmation, for that checklist.
  up.on('click', '.js-pop-over .js-delete-checklist', function (event) {
    event.preventDefault();
    if (!menuChecklist) return;
    var t = document.querySelector('template.js-pop-over-template[data-pop="checklist-delete"]');
    openPopOver('checklist-delete');
    var f = document.querySelector('.js-pop-over .js-checklist-delete-form');
    if (f && t) f.setAttribute('action', t.getAttribute('data-card-url') + encodeURIComponent(menuChecklist) + '/delete');
  });
  // checklistSortableItems: an item dragged - by its handle when the member
  // shows drag handles, else by the item - among its checklist's items, a
  // placeholder where it will land, and the order saved on the drop.
  document.addEventListener('pointerdown', function (event) {
    if (event.button !== 0 || !event.target.closest) return;
    var item = event.target.closest('.js-checklist-items .js-checklist-item');
    var box = item && item.closest('.js-checklist-items');
    if (!box || !box.getAttribute('data-sort-url')) return;
    var onHandle = !!event.target.closest('.checklistitem-handle');
    if (box.getAttribute('data-handles') === 'true' ? !onHandle : event.target.closest('button, a, textarea, input, form, .check-box-container')) return;
    itemDrag = { item: item, box: box, x: event.clientX, y: event.clientY, started: false };
    // A refresh already on its way would replace the item under the pointer:
    // it is called off, and the board catches up after the drop.
    if (up.network.isBusy()) { up.fragment.abort('.board-wrapper'); stale = true; }
  });
  document.addEventListener('pointermove', function (event) {
    var d = itemDrag;
    if (!d) return;
    if (!d.started) {
      if (Math.abs(event.clientX - d.x) + Math.abs(event.clientY - d.y) < 5) return;
      var rect = d.item.getBoundingClientRect();
      d.started = true;
      d.offset = d.y - rect.top;
      d.placeholder = document.createElement('div');
      d.placeholder.className = 'placeholder checklist-item-placeholder';
      d.placeholder.style.height = rect.height + 'px';
      d.box.insertBefore(d.placeholder, d.item);
      d.item.classList.add('ui-sortable-helper');
      document.body.classList.add('is-dragging-checklist-item');
      if (window.getSelection) window.getSelection().removeAllRanges();
      d.item.style.cssText += ';position:fixed;left:' + rect.left + 'px;width:' + rect.width + 'px;top:' + rect.top + 'px;z-index:1000;pointer-events:none;';
      dragging++;
    }
    event.preventDefault();
    d.item.style.top = (event.clientY - d.offset) + 'px';
    var before = null;
    Array.prototype.some.call(d.box.querySelectorAll(':scope > .js-checklist-item'), function (el) {
      if (el === d.item) return false;
      var r = el.getBoundingClientRect();
      if (event.clientY < r.top + r.height / 2) { before = el; return true; }
      return false;
    });
    if (before) d.box.insertBefore(d.placeholder, before);
    else d.box.appendChild(d.placeholder);
  });
  document.addEventListener('pointerup', function () {
    var d = itemDrag;
    itemDrag = null;
    if (d && !d.started && stale && dragging === 0) setTimeout(reloadBoard, 0);
    if (!d || !d.started) return;
    d.box.insertBefore(d.item, d.placeholder);
    d.placeholder.remove();
    d.item.classList.remove('ui-sortable-helper');
    document.body.classList.remove('is-dragging-checklist-item');
    ['position', 'left', 'width', 'top', 'z-index', 'pointer-events'].forEach(function (k) { d.item.style.removeProperty(k); });
    itemDragJustEnded = true;
    setTimeout(function () { itemDragJustEnded = false; }, 0);
    var body = new URLSearchParams();
    d.box.querySelectorAll(':scope > .js-checklist-item').forEach(function (el) { body.append('item', el.getAttribute('data-item')); });
    fetch(d.box.getAttribute('data-sort-url'), { method: 'POST', credentials: 'same-origin', body: body, headers: { 'X-Weddy-Page': pageId } })
      .finally(function () {
        dragging = Math.max(0, dragging - 1);
        if (dragging === 0 && stale) reloadBoard();
      });
  });
  // The click that ends a drag does not open the item's editor.
  document.addEventListener('click', function (event) {
    if (itemDragJustEnded) { event.preventDefault(); event.stopPropagation(); }
  }, true);
  // copyAndMoveChecklist: the chosen board's swimlanes, lists and the cards
  // of that swimlane and list, the checklist's own card chosen first.
  function fillChecklistTarget(form, boardId, first) {
    fetch('/b/' + encodeURIComponent(boardId) + '/places?cards=1', { credentials: 'same-origin' })
      .then(function (r) { return r.json(); }).then(function (p) {
        form._places = p;
        var fill = function (sel, rows, label, want) {
          sel.innerHTML = '';
          rows.forEach(function (o) {
            var opt = document.createElement('option');
            opt.value = o._id;
            opt.textContent = label(o);
            if (o._id === want) opt.selected = true;
            sel.appendChild(opt);
          });
        };
        fill(form.querySelector('.js-select-swimlanes'), p.swimlanes || [], function (o) { return o.title; }, first && form.dataset.swimlane);
        fill(form.querySelector('.js-select-lists'), p.lists || [], function (o) { return o.title; }, first && form.dataset.list);
        fillChecklistCards(form, first && form.dataset.card);
      });
  }
  function fillChecklistCards(form, want) {
    var p = form._places || {};
    var lane = form.querySelector('.js-select-swimlanes').value, list = form.querySelector('.js-select-lists').value;
    var sel = form.querySelector('.js-select-cards');
    sel.innerHTML = '';
    (p.cards || []).forEach(function (c) {
      if (c.listId !== list || (lane && c.swimlaneId !== lane)) return;
      var opt = document.createElement('option');
      opt.value = c._id;
      opt.textContent = c.title;
      if (c._id === want) opt.selected = true;
      sel.appendChild(opt);
    });
  }
  up.on('click', '.js-pop-over .js-move-checklist, .js-pop-over .js-copy-checklist', function (event, el) {
    event.preventDefault();
    if (!menuChecklist) return;
    var name = el.classList.contains('js-move-checklist') ? 'checklist-move' : 'checklist-copy';
    var t = document.querySelector('template.js-pop-over-template[data-pop="' + name + '"]');
    openPopOver(name);
    var form = document.querySelector('.js-pop-over .js-checklist-target');
    if (!form || !t) return;
    form.setAttribute('action', '/b/' + form.dataset.board + '/cards/' + form.dataset.card + '/checklists/' + encodeURIComponent(menuChecklist) + '/' + t.getAttribute('data-path'));
    fillChecklistTarget(form, form.querySelector('.js-select-boards').value, true);
  });
  up.on('change', '.js-checklist-target .js-select-boards', function (event, sel) { fillChecklistTarget(sel.form, sel.value, false); });
  up.on('change', '.js-checklist-target .js-select-swimlanes, .js-checklist-target .js-select-lists', function (event, sel) { fillChecklistCards(sel.form); });
  // attachmentGallery's "+": cardAttachmentsPopup, whose "Computer" picks a
  // file and sends it.
  up.on('click', '.card-attachmentGallery .js-add-attachment', function (event) {
    event.preventDefault();
    openPopOver('card-attachments');
  });
  // attachmentActionsPopup, the attachment's own.
  up.on('click', '.card-attachmentGallery .js-open-attachment-menu', function (event, el) {
    event.preventDefault();
    openPopOver('attachment-actions-' + el.getAttribute('data-attachment'));
  });
  up.on('click', '.js-pop-over .js-computer-upload', function (event, el) {
    event.preventDefault();
    var input = el.closest('form').querySelector('.js-attach-file');
    if (input) input.click();
  });
  up.on('change', '.js-pop-over .js-attach-file', function (event, el) {
    // Sent in the background, as WeKan's uploader: the page stays, the board
    // is answered with the file on it.
    if (el.files && el.files.length) up.submit(el.form);
  });
  // attachmentViewer: a tile opens its file in the overlay - an image, a PDF,
  // video, audio or text - and anything else in its own tab.
  up.on('click', '.card-attachmentGallery .open-preview', function (event, el) {
    event.preventDefault();
    var kind = el.getAttribute('data-kind'), src = el.getAttribute('data-src');
    var overlay = document.getElementById('viewer-overlay');
    if (!overlay || kind === 'none') { window.open(src, '_blank', 'noopener'); return; }
    ['image-viewer', 'video-viewer', 'audio-viewer', 'pdf-viewer', 'txt-viewer', 'office-viewer', 'zip-viewer'].forEach(function (id) {
      var v = document.getElementById(id);
      if (v) v.classList.add('hidden');
    });
    if (kind === 'zip') {
      // WeKan's ZIP preview: the entry names, as inert text, never links.
      var zip = document.getElementById('zip-viewer');
      zip.textContent = zip.dataset.loading || '';
      zip.classList.remove('hidden');
      fetch('/cdn/storage/attachments/' + encodeURIComponent(el.getAttribute('data-attachment-id')) + '/zip-entries', { credentials: 'same-origin' })
        .then(function (r) { return r.json().then(function (d) { if (!r.ok) throw new Error(d.error); return d.entries || []; }); })
        .then(function (entries) {
          zip.replaceChildren();
          if (!entries.length) { zip.textContent = zip.dataset.empty || ''; return; }
          var ul = document.createElement('ul');
          entries.forEach(function (name) { var li = document.createElement('li'); li.textContent = name; ul.appendChild(li); });
          zip.appendChild(ul);
        }, function () { zip.textContent = zip.dataset.error || ''; });
      document.getElementById('attachment-name').textContent = el.getAttribute('data-name') || '';
      overlay.classList.remove('hidden');
      return;
    }
    var view = document.getElementById({ image: 'image-viewer', video: 'video-viewer', audio: 'audio-viewer', pdf: 'pdf-viewer', text: 'txt-viewer' }[kind]);
    if (!view) return;
    if (view.tagName === 'OBJECT') view.setAttribute('data', src);
    else view.setAttribute('src', src);
    view.classList.remove('hidden');
    document.getElementById('attachment-name').textContent = el.getAttribute('data-name') || '';
    overlay.classList.remove('hidden');
  });
  up.on('click', '#viewer-close', function () {
    var overlay = document.getElementById('viewer-overlay');
    if (!overlay) return;
    overlay.classList.add('hidden');
    overlay.querySelectorAll('video, audio').forEach(function (m) { m.pause(); });
  });
  // cardCustomFieldsPopup, from the Custom Fields section's hamburger.
  up.on('click', '.js-open-custom-fields-settings', function (event) {
    event.preventDefault();
    openPopOver('card-custom-fields');
  });
  up.on('click', '.js-pop-over .js-open-create-custom-field', function (event) {
    event.preventDefault();
    openPopOver('create-custom-field');
    var name = document.querySelector('.js-pop-over .js-field-name');
    if (name) name.focus();
  });
  // The custom fields' layout: one per row or a grid, the member's own
  // (toggleCustomFieldsGrid), at once on this page.
  up.on('click', '#toggleCustomFieldsGridButton', function (event, el) {
    event.preventDefault();
    var body = el.closest('.card-details-group-custom-fields').querySelector('.card-details-group-body');
    callAsync('toggleCustomFieldsGrid').then(function () {
      if (!body) return;
      var rows = body.classList.contains('custom-fields-one-per-row');
      body.classList.toggle('custom-fields-one-per-row', !rows);
      body.classList.toggle('custom-fields-grid', rows);
    });
  });
  // A string template's last input, written in and Enter: one more input.
  up.on('keydown', '.js-card-customfield-stringtemplate-item.last', function (event, el) {
    if (event.key !== 'Enter' || !el.value.trim() || event.ctrlKey || event.metaKey) return;
    event.preventDefault();
    var next = el.cloneNode();
    next.value = '';
    el.classList.remove('last');
    el.parentNode.insertBefore(next, el.nextSibling);
    next.focus();
  });
  // cardCustomField-*: the field's title, its value or its pencil opens its
  // editor; a checkbox's square alone saves the other value at once.
  up.on('click', '.card-details-item-customfield .js-edit-card-custom-field-value', function (event, el) {
    event.preventDefault();
    var item = el.closest('.card-details-item-customfield');
    if (event.target.closest && event.target.closest('.js-card-custom-field-checkbox .check-box-container')) {
      var toggle = item.querySelector('form.js-card-custom-field-checkbox-toggle');
      if (toggle) up.submit(toggle);
      return;
    }
    var form = item.querySelector('form.js-inlined-form');
    if (!form) return;
    el.closest('.js-card-details').querySelectorAll('.card-details-item-customfield .js-inlined-form').forEach(function (f) { if (f !== form) closeInlinedForm(f); });
    var body = form.querySelector(':scope > template.js-inlined-form-body');
    if (body && !form.querySelector('input, textarea, select')) form.appendChild(body.content.cloneNode(true));
    form.classList.remove('is-hidden');
    var input = form.querySelector('textarea, select, input:not([type=hidden])');
    if (input) input.focus();
  });
  // The checkbox editor's square follows its box.
  up.on('change', '.js-card-customfield-checkbox-editor .js-card-customfield-checkbox-input', function (event, el) {
    var square = el.parentNode.querySelector('.materialCheckBox');
    if (square) square.classList.toggle('is-checked', el.checked);
  });
  // customFieldCopyButton: the field's value to the clipboard.
  up.on('click', '.js-copy-custom-field', function (event, el) {
    event.preventDefault();
    var value = el.getAttribute('data-value') || '';
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(value);
    el.classList.add('is-copied');
    setTimeout(function () { el.classList.remove('is-copied'); }, 1200);
  });
  // The checklists' inlined forms: a "+" opens its own form - the
  // checklist's for an item, the section's for a checklist.
  up.on('click', '.js-checklists-section .js-open-inlined-form, .card-details-group-description .js-open-inlined-form, .card-details-subtasks .js-open-inlined-form', function (event, el) {
    event.preventDefault();
    var opens = el.getAttribute('data-opens');
    var where = {
      'add-checklist': ['.js-checklists-section', 'form.js-add-checklist'],
      'add-checklist-item': ['.js-checklist', 'form.js-add-checklist-item'],
      'edit-checklist-title': ['.js-checklist', 'form.js-edit-checklist-title'],
      'edit-checklist-item': ['.js-checklist-item', 'form.js-edit-checklist-item'],
      'edit-description': ['.card-details-group-description', 'form.js-card-description'],
      'add-subtask': ['.card-details-subtasks', 'form.js-add-subtask'],
    }[opens];
    if (!where) return;
    var box = el.closest(where[0]);
    var form = box && box.querySelector(where[1]);
    if (!form) return;
    // One inlined form at a time, as WeKan's, and its fields only while open.
    el.closest('.js-card-details').querySelectorAll('.js-checklists-section .js-inlined-form, .card-details-group-description .js-inlined-form, .card-details-subtasks .js-inlined-form').forEach(function (f) { if (f !== form) closeInlinedForm(f); });
    var body = form.querySelector(':scope > template.js-inlined-form-body');
    if (body && !form.querySelector('textarea')) form.appendChild(body.content.cloneNode(true));
    form.classList.remove('is-hidden');
    form.querySelector('textarea').focus();
  });
  // WeKan's editDateForm: a month drawn day by day, the day picked marked,
  // and on Save the time the browser makes of that date and the time typed
  // (empty: the opener's default - 17:00 for a due date), refused when the
  // date is missing or its year is not four digits (datepicker.js).
  function pad2(n) { return (n < 10 ? '0' : '') + n; }
  function isoDay(d) { return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()); }
  function drawCalendar(form) {
    var lang = document.documentElement.lang || undefined;
    var shown = form._shown, picked = form.querySelector('.js-date-field').value;
    form.querySelector('.js-calendar-month-label').textContent = shown.toLocaleDateString(lang, { month: 'long', year: 'numeric' });
    var head = form.querySelector('.js-calendar-weekdays');
    head.innerHTML = '';
    for (var i = 0; i < 7; i++) {
      var th = document.createElement('th');
      th.scope = 'col';
      th.textContent = new Date(2024, 0, 1 + i).toLocaleDateString(lang, { weekday: 'short' });
      head.appendChild(th);
    }
    var body = form.querySelector('.js-calendar-weeks');
    body.innerHTML = '';
    var first = new Date(shown.getFullYear(), shown.getMonth(), 1);
    var lead = (first.getDay() + 6) % 7; // weeks start on Monday
    var days = new Date(shown.getFullYear(), shown.getMonth() + 1, 0).getDate();
    var today = isoDay(new Date()), tr = null;
    for (var cell = 0; cell < lead + days; cell++) {
      if (cell % 7 === 0) { tr = document.createElement('tr'); body.appendChild(tr); }
      var td = document.createElement('td');
      if (cell >= lead) {
        var d = new Date(shown.getFullYear(), shown.getMonth(), cell - lead + 1), iso = isoDay(d);
        var b = document.createElement('button');
        b.type = 'button';
        b.className = 'primary js-calendar-day' + (iso === today ? ' is-today' : '');
        b.setAttribute('data-date', iso);
        b.setAttribute('aria-pressed', iso === picked ? 'true' : 'false');
        b.title = d.toLocaleDateString(lang, { dateStyle: 'full' });
        b.textContent = String(d.getDate());
        if (iso === today) td.className = 'is-today-cell';
        td.appendChild(b);
      }
      tr.appendChild(td);
    }
  }
  // calendarTimeInput: the hour and minute selects over the hidden time.
  function setPickerTime(form, hhmm) {
    var p = hhmm.split(':');
    var fill = function (sel, n, want) {
      sel.innerHTML = '';
      for (var i = 0; i < n; i++) {
        var o = document.createElement('option');
        o.value = o.textContent = pad2(i);
        if (pad2(i) === want) o.selected = true;
        sel.appendChild(o);
      }
    };
    fill(form.querySelector('.js-calendar-hour'), 24, pad2(+p[0]));
    fill(form.querySelector('.js-calendar-minute'), 60, pad2(+(p[1] || 0)));
    form.querySelector('.js-time-field').value = pad2(+p[0]) + ':' + pad2(+(p[1] || 0));
  }
  function openDatePicker(el, defaultTime) {
    openPopOver('edit-date');
    var form = document.querySelector('.js-pop-over form.edit-date');
    if (!form) return;
    var pop = form.closest('.js-pop-over');
    if (el.getAttribute('data-title')) pop.setAttribute('aria-label', el.getAttribute('data-title'));
    form.setAttribute('action', el.getAttribute('data-url'));
    var now = new Date();
    if (defaultTime === 'now') defaultTime = pad2(now.getHours()) + ':' + pad2(now.getMinutes());
    var at = el.getAttribute('data-date') ? new Date(el.getAttribute('data-date')) : null;
    if (at && isNaN(at)) at = null;
    // #6809: no date yet - today is picked, so Save alone saves today.
    form.querySelector('.js-date-field').value = isoDay(at || now);
    setPickerTime(form, at ? pad2(at.getHours()) + ':' + pad2(at.getMinutes()) : defaultTime);
    form.querySelector('.js-delete-date').hidden = !at;
    form._shown = new Date((at || now).getFullYear(), (at || now).getMonth(), 1);
    drawCalendar(form);
  }
  up.on('click', '.js-checklist-due-date, .js-checklist-item-due-date, .js-checklists-section .js-edit-date', function (event, el) {
    event.preventDefault();
    event.stopPropagation();
    openDatePicker(el, '17:00');
  });
  up.on('click', '.js-card-details .card-details-item .js-edit-date, .js-received-date, .js-start-date, .js-due-date, .js-end-date', function (event, el) {
    event.preventDefault();
    event.stopPropagation();
    openDatePicker(el, el.getAttribute('data-default-time') || '12:00');
  });
  up.on('change', '.js-pop-over form.edit-date .js-calendar-hour, .js-pop-over form.edit-date .js-calendar-minute', function (event, el) {
    var form = el.form;
    form.querySelector('.js-time-field').value = form.querySelector('.js-calendar-hour').value + ':' + form.querySelector('.js-calendar-minute').value;
  });
  up.on('click', '.js-pop-over form.edit-date .js-calendar-day', function (event, el) {
    var form = el.form;
    form.querySelector('.js-date-field').value = el.getAttribute('data-date');
    form.querySelectorAll('.js-calendar-day').forEach(function (b) { b.setAttribute('aria-pressed', b === el ? 'true' : 'false'); });
  });
  up.on('click', '.js-pop-over form.edit-date .js-calendar-previous, .js-pop-over form.edit-date .js-calendar-next, .js-pop-over form.edit-date .js-calendar-today', function (event, el) {
    var form = el.form, s = form._shown;
    if (el.classList.contains('js-calendar-today')) form._shown = new Date(new Date().getFullYear(), new Date().getMonth(), 1);
    else form._shown = new Date(s.getFullYear(), s.getMonth() + (el.classList.contains('js-calendar-next') ? 1 : -1), 1);
    drawCalendar(form);
  });
  up.on('click', '.js-pop-over form.edit-date .js-calendar-year-previous, .js-pop-over form.edit-date .js-calendar-year-next', function (event, el) {
    var form = el.form, s = form._shown;
    form._shown = new Date(s.getFullYear() + (el.classList.contains('js-calendar-year-next') ? 1 : -1), s.getMonth(), 1);
    drawCalendar(form);
  });
  up.on('click', '.js-pop-over form.edit-date .js-delete-date', function (event, el) {
    var form = el.form;
    form.querySelector('[name=delete]').value = '1';
    up.submit(form, { params: { delete: '1' } });
  });
  up.on('up:form:submit', 'form.edit-date', function (event, form) {
    if (form.querySelector('[name=delete]').value === '1') return;
    var err = form.querySelector('.js-date-error');
    var fail = function (key) { event.preventDefault(); err.textContent = err.getAttribute('data-' + key); err.hidden = false; };
    var day = form.querySelector('.js-date-field').value;
    if (!/^\d{4}-\d{2}-\d{2}$/.test(day)) return fail('invalid-date');
    var time = (form.querySelector('.js-time-field').value || form._defaultTime || '12:00').trim();
    var m = /^(\d{1,2})(?::(\d{2}))?$/.exec(time);
    if (!m || +m[1] > 23 || +(m[2] || 0) > 59) return fail('invalid-time');
    var at = new Date(day + 'T' + pad2(+m[1]) + ':' + (m[2] || '00') + ':00');
    if (isNaN(at)) return fail('invalid-date');
    if (at.getFullYear() < 1000 || at.getFullYear() > 9999) return fail('invalid-year');
    form.querySelector('[name=at]').value = at.toISOString();
    // Unpoly has read the form already: the time goes into what it sends.
    if (event.params && event.params.set) event.params.set('at', at.toISOString());
  });
  // checklistItemAssigneePopup: for that item, its assignee marked.
  up.on('click', '.js-checklist-item-assignee', function (event, el) {
    event.preventDefault();
    event.stopPropagation();
    var url = el.getAttribute('data-url'), current = el.getAttribute('data-assignee');
    openPopOver('checklist-item-assignee');
    var f = document.querySelector('.js-pop-over .js-checklist-item-assignee-form');
    if (!f) return;
    f.setAttribute('action', url);
    f.querySelectorAll('.js-set-checklist-item-assignee').forEach(function (b) {
      b.classList.toggle('is-active', !!current && b.value === current);
    });
  });
  // checklistItemDeletePopup: the confirmation, for that item.
  up.on('click', '.js-delete-checklist-item', function (event, el) {
    event.preventDefault();
    var url = el.getAttribute('data-url');
    openPopOver('checklist-item-delete');
    var f = document.querySelector('.js-pop-over .js-checklist-item-delete-form');
    if (f) f.setAttribute('action', url);
  });
  up.on('click', '.js-pop-over .js-export-checklist', function (event) {
    event.preventDefault();
    if (menuChecklist) openScope('export', 'checklist', { checklistId: menuChecklist });
  });
  up.on('change', '.js-pop-over .js-scope-import-file', function (event, el) {
    var file = el.files && el.files[0];
    if (!file || !scope) return;
    var p = el.closest('.js-pop-over');
    var t = document.querySelector('template.js-pop-over-template[data-pop="import-scope"]');
    var status = p.querySelector('.js-scope-import-status');
    var board = t.getAttribute('data-board');
    var fields = [];
    p.querySelectorAll('.js-export-field-toggle').forEach(function (a) {
      if (!exportOff[a.dataset.field]) fields.push(a.dataset.field);
    });
    var target = { boardId: board };
    Object.keys(scope.target).forEach(function (k) {
      // A card's import goes below the card: the card is its place.
      if (scope.kind === 'card' && k === 'listId') return;
      target[k] = scope.target[k];
    });
    status.textContent = t.getAttribute('data-importing');
    var done = function (counts) {
      var parts = [];
      Object.keys(counts || {}).forEach(function (k) { if (counts[k] > 0) parts.push(counts[k] + ' ' + k); });
      status.textContent = t.getAttribute('data-done') + ' ' + (parts.join(', ') || '0');
      up.reload('.board-wrapper');
    };
    var failed = function (notWekan) {
      status.textContent = t.getAttribute(notWekan ? 'data-not-wekan' : 'data-failed');
    };
    if (/\.zip$/i.test(file.name)) {
      var q = new URLSearchParams(target);
      q.set('fields', fields.join(','));
      fetch('/api/import/zip?' + q.toString(), { method: 'POST', headers: { 'Content-Type': 'application/zip' }, body: file, credentials: 'same-origin' })
        .then(function (r) { return r.json().then(function (a) { return { ok: r.ok, a: a }; }); })
        .then(function (x) { if (x.ok && x.a.ok) done(x.a.counts); else failed(x.a.error === 'import-not-wekan-export'); })
        .catch(function () { failed(false); });
    } else {
      file.text().then(function (text) {
        var doc;
        try { doc = JSON.parse(text); } catch (e) { failed(true); return; }
        return fetch('/_methods/importScoped', { method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin',
          body: JSON.stringify([target, doc, fields]) })
          .then(function (r) { return r.json(); })
          .then(function (a) { if (a && a.result) done(a.result); else failed(false); });
      }).catch(function () { failed(false); });
    }
    el.value = '';
  });
  up.on('click', '.js-export-card-details-toggle, .js-export-field-toggle', function (event, el) {
    // The pop-over stays open: choosing five sections is not five reopens.
    event.preventDefault();
    var field = el.classList.contains('js-export-card-details-toggle') ? 'card-details' : el.dataset.field;
    exportOff[field] = !exportOff[field];
    applyExportSelection(el.closest('.js-pop-over'));
  });
  up.on('click', '.js-watch-board', function (event) {
    event.preventDefault();
    openPopOver('watch');
  });
  up.on('click', '.js-open-add-swimlane-menu', function (event, el) {
    event.preventDefault();
    openPopOver('add-swimlane');
    var p = document.querySelector('.js-pop-over');
    if (!p) return;
    var after = p.querySelector('.js-swimlane-after');
    if (after) after.value = el.dataset.swimlane;
    var input = p.querySelector('.swimlane-name-input');
    if (input) input.focus();
  });
  // The header's member menu, and its Edit Profile (userHeader.jade).
  up.on('click', '.js-open-header-member-menu', function (event) {
    event.preventDefault();
    openPopOver('member-menu');
  });
  up.on('click', '.js-edit-profile', function (event) {
    event.preventDefault();
    openPopOver('edit-profile');
  });
  up.on('click', '.js-add-members', function (event) {
    event.preventDefault();
    openPopOver('card-members');
  });
  // A choice in a pop-over is done when the board has reloaded with it.
  // ... and only a choice made in it: an update the page was already waiting
  // for when the pop-over opened leaves it open.
  var closeAfterChoice = false;
  up.on('up:form:submit', '.js-pop-over form', function () { closeAfterChoice = true; });
  up.on('submit', '.js-pop-over form', function () { closeAfterChoice = true; });
  up.on('up:fragment:inserted', function (event, el) {
    if (closeAfterChoice && el && el.matches && el.matches('.board-wrapper')) {
      closeAfterChoice = false;
      closePopOvers();
    }
  });
  up.on('click', '.js-close-pop-over', function (event) {
    event.preventDefault();
    closePopOvers();
  });
  up.on('keydown', function (event) {
    if (event.key === 'Escape') {
      // WeKan's EscapeActions: a pop-over first, else an inlined form of the card.
      if (document.querySelector('.js-pop-over')) closePopOvers();
      else document.querySelectorAll('.js-card-details .js-checklists-section .js-inlined-form:not(.is-hidden), .js-card-details .card-details-group-description .js-inlined-form:not(.is-hidden), .js-card-details .card-details-item-customfield .js-inlined-form:not(.is-hidden)').forEach(function (f) { closeInlinedForm(f); });
      if (staleUnderForm && !document.querySelector('.board-wrapper .js-card-details .js-inlined-form:not(.is-hidden)')) { staleUnderForm = false; setTimeout(reloadBoard, 0); }
    }
  });

  // A section heading with a caret (WeKan's client/lib/sectionCaret.js): it
  // shows or hides the section after it, and the caret says which.
  up.on('click', '.js-toggle-card-section, .js-toggle-show-activities', function (event, el) {
    // A link or button in the heading does its own thing, not the fold.
    var control = event.target.closest && event.target.closest('a, button');
    if (control && control !== el && el.contains(control)) return;
    event.preventDefault();
    var section = el.nextElementSibling;
    if (!section) return;
    var open = section.classList.toggle('is-hidden') === false;
    // What the heading shows only while its section is open (the layout
    // toggle) leaves with the fold and comes back with it.
    if (!open) {
      el._openOnly = Array.prototype.slice.call(el.querySelectorAll('.js-section-open-only'));
      el._openOnly.forEach(function (n) { n.remove(); });
    } else if (el._openOnly) {
      el._openOnly.forEach(function (n) { el.appendChild(n); });
      el._openOnly = null;
    }
    if (el.classList.contains('js-toggle-show-activities')) sidebarState('activities', open);
    // What is shown on demand is put on the page the first time it opens.
    var stamp = open && section.querySelector(':scope > template.js-stamp-on-open');
    if (stamp) {
      section.appendChild(stamp.content.cloneNode(true));
      stamp.remove();
      up.hello(section);
    }
    var caret = el.querySelector('.fa-caret-right, .fa-caret-down');
    if (caret) {
      caret.classList.toggle('fa-caret-down', open);
      caret.classList.toggle('fa-caret-right', !open);
    }
  });

  // Meteor.user() and Meteor.userId() as Node.js WeKan's pages have them: the
  // signed-in user, without secrets (/api/user), refreshed every few seconds -
  // for WeKan's scripts and tests, which read the user that way.
  var currentUser = null;
  // Meteor.call / Meteor.callAsync: WeKan's methods that Weddy has, through
  // POST /_methods/{name}; a failure is a Meteor.Error-like {error, reason}.
  function callAsync(name) {
    var args = Array.prototype.slice.call(arguments, 1);
    return fetch('/_methods/' + encodeURIComponent(name), {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(args),
    }).then(function (r) {
      return r.json().then(function (d) {
        if (!r.ok) {
          var e = new Error(d.reason || d.error);
          e.error = d.error; e.reason = d.reason;
          throw e;
        }
        var done = function () { return d.result; };
        // WeKan's pages change language the moment it is set: draw this page
        // again in it - its lang and direction too - before the call answers.
        if (name === 'setLanguage') return refreshUser().then(rerenderPage).then(done, done);
        return refreshUser().then(done);
      });
    });
  }
  function rerenderPage() {
    return fetch(window.location.href, { credentials: 'same-origin' }).then(function (r) { return r.text(); }).then(function (html) {
      var doc = new DOMParser().parseFromString(html, 'text/html');
      document.documentElement.lang = doc.documentElement.lang;
      document.documentElement.dir = doc.documentElement.dir;
      document.title = doc.title;
      return up.render({ target: 'body', document: html, history: false });
    });
  }
  if (!window.Meteor) {
    window.Meteor = {
      user: function () { return currentUser; },
      userId: function () { return currentUser ? currentUser._id : null; },
      callAsync: callAsync,
      // Meteor.apply(name, args, [options], [callback]).
      apply: function (name, args, options, cb) {
        if (typeof options === 'function') { cb = options; options = null; }
        var p = callAsync.apply(null, [name].concat(args || []));
        if (cb) p.then(function (res) { cb(undefined, res); }, function (err) { cb(err); });
        return undefined;
      },
      call: function () {
        var args = Array.prototype.slice.call(arguments);
        var cb = typeof args[args.length - 1] === 'function' ? args.pop() : null;
        var p = callAsync.apply(null, args);
        if (cb) p.then(function (res) { cb(undefined, res); }, function (err) { cb(err); });
        return undefined;
      },
    };
  }
  function unreadOf(user) {
    var list = (user && user.profile && user.profile.notifications) || [];
    return list.filter(function (n) { return !n.read; }).length;
  }
  function refreshUser() {
    return fetch('/api/user', { credentials: 'same-origin' }).then(function (r) {
      return r.ok ? r.json() : null;
    }).then(function (u) {
      if (!u) return;
      currentUser = u;
      var unread = unreadOf(u);
      document.querySelectorAll('.notifications-drawer-toggle').forEach(function (el) {
        el.classList.toggle('alert', unread > 0);
        var c = el.querySelector('.notifications-count');
        if (c) c.textContent = unread > 0 ? String(unread) : '';
      });
    }).catch(function () {});
  }
  // The notifications drawer: opened from the indicator; its menu marks all
  // read, or - after a confirmation in the user's language - deletes all.
  up.on('click', '.notifications-drawer-toggle', function (event) {
    event.preventDefault();
    var d = document.getElementById('notifications-drawer');
    if (d) d.classList.toggle('is-hidden');
  });
  up.on('click', '#notifications-drawer .close', function (event) {
    event.preventDefault();
    document.getElementById('notifications-drawer').classList.add('is-hidden');
  });
  up.on('click', '#notifications-drawer .notification-menu-toggle', function (event) {
    event.preventDefault();
    document.querySelector('#notifications-drawer .notification-menu').classList.toggle('is-hidden');
  });
  function drawerShowRead(on) {
    var d = document.getElementById('notifications-drawer');
    d.classList.toggle('show-read', on);
    d.querySelector('.js-filter-unread').classList.toggle('selected', !on);
    d.querySelector('.js-view-all').classList.toggle('selected', on);
    d.querySelector('.notification-menu').classList.add('is-hidden');
  }
  up.on('click', '#notifications-drawer .js-filter-unread', function (event) { event.preventDefault(); drawerShowRead(false); });
  up.on('click', '#notifications-drawer .js-view-all', function (event) { event.preventDefault(); drawerShowRead(true); });
  // #5171: a grouped row opens to show all of its card's or board's entries.
  // Admin Panel / Organizations and Teams: new and edit pop-overs, and the
  // feature columns - a row's box, or a column's select-all / unselect-all.
  ['org', 'team'].forEach(function (kind) {
    var K = kind.charAt(0).toUpperCase() + kind.slice(1);
    up.on('click', '.new-' + kind, function (event) { event.preventDefault(); openPopOver('new-' + kind); });
    up.on('click', '.edit-' + kind, function (event, link) {
      event.preventDefault();
      openPopOver('edit-' + kind + '-' + link.getAttribute('data-id'));
    });
    up.on('change', '.js-toggle-' + kind + '-feature', function (event, box) {
      var f = box.getAttribute('data-feature');
      Meteor.callAsync('set' + f.charAt(0).toUpperCase() + f.slice(1), { _id: box.getAttribute('data-id') }, box.checked)
        .then(function () { up.reload('.setting-content'); });
    });
    up.on('click', '.js-' + kind + '-feature-all', function (event, link) {
      event.preventDefault();
      Meteor.callAsync('setAll' + K + 'sFeature', link.getAttribute('data-feature'), link.getAttribute('data-value') === 'true')
        .then(function () { up.reload('.setting-content'); });
    });
  });
  // WeKan's passwordInput: show or hide the password; a button of its own,
  // so Space and Enter work it without sending the form.
  up.on('click', '.password-toggle-btn', function (event, btn) {
    event.preventDefault();
    var input = btn.parentNode.querySelector('input.password-field');
    if (input) input.type = input.type === 'password' ? 'text' : 'password';
  });
  // Admin Panel / People: a user's editUserPopup, and Active switched by a click.
  up.on('click', '.edit-user', function (event, link) {
    event.preventDefault();
    openPopOver('edit-user-' + link.getAttribute('data-user-id'));
  });
  up.on('click', '.js-toggle-active-status', function (event, el) {
    event.preventDefault();
    var body = new URLSearchParams({ loginDisabled: el.getAttribute('data-is-active') === 'true' ? 'true' : 'false' });
    fetch('/admin/people/' + encodeURIComponent(el.getAttribute('data-user-id')), { method: 'POST', body: body, credentials: 'same-origin', redirect: 'manual' })
      .then(function () { up.reload('.setting-content'); });
  });
  // The minicard's "Mark as complete": the card does not open.
  up.on('click', '.js-toggle-card-complete', function (event, link) {
    event.preventDefault();
    event.stopPropagation();
    var m = location.pathname.match(/^\/b\/([^/]+)/);
    if (!m) return;
    var body = new URLSearchParams({ complete: link.getAttribute('data-complete') });
    fetch('/b/' + m[1] + '/cards/' + encodeURIComponent(link.getAttribute('data-card')) + '/complete',
      { method: 'POST', body: body, credentials: 'same-origin' }).then(function () { up.reload('.board-wrapper'); });
  });
  up.on('click', '#notifications-drawer .js-toggle-notification-group', function (event, link) {
    event.preventDefault();
    var open = link.getAttribute('aria-expanded') !== 'true';
    link.setAttribute('aria-expanded', open ? 'true' : 'false');
    link.textContent = (open ? '▾' : '▸') + link.textContent.slice(1);
    document.querySelectorAll('#notifications-drawer .notification.is-group-rest').forEach(function (li) {
      if (li.getAttribute('data-group') === link.getAttribute('data-key')) li.classList.toggle('is-group-open', open);
    });
  });
  function drawerPost(path) {
    return fetch(path, { method: 'POST', credentials: 'same-origin' }).then(function () { return refreshUser(); });
  }
  up.on('click', '#notifications-drawer .mark-all-read', function (event) {
    event.preventDefault();
    drawerPost('/notifications/read-all').then(function () {
      document.querySelectorAll('#notifications-drawer .notification').forEach(function (n) { n.classList.add('is-read'); });
    });
  });
  up.on('click', '#notifications-drawer .delete-all', function (event, el) {
    event.preventDefault();
    if (!window.confirm(el.dataset.confirm)) return;
    drawerPost('/notifications/delete-all').then(function () {
      document.querySelector('#notifications-drawer .notifications').innerHTML = '';
    });
  });

  // Member Settings' notification delivery: a schedule is read in the
  // clock of the browser that saves it (WeKan's browserTimezone).
  // The import page's "People in the file": one choice checked, and the
  // form says which. Session.set('importMembersMode', mode) chooses it too,
  // as WeKan's page reads it from its Session.
  function chooseMembersMode(box, mode) {
    var link = box.querySelector('.js-import-members-mode[data-mode="' + mode + '"]');
    if (!link) return;
    box.querySelectorAll('.js-import-members-mode .materialCheckBox').forEach(function (c) {
      c.classList.toggle('is-checked', c.parentNode === link);
    });
    box.querySelector('.js-import-members-mode-value').value = mode;
  }
  up.compiler('.import-members-mode', function (box) {
    box.addEventListener('click', function (event) {
      var link = event.target.closest('.js-import-members-mode');
      if (!link) return;
      event.preventDefault();
      chooseMembersMode(box, link.getAttribute('data-mode'));
    });
  });
  // "One board per project": a checkbox the form sends as splitBy, kept in
  // Session('importSplitByProject') as WeKan's page keeps it.
  function chooseSplit(on) {
    document.querySelectorAll('.js-import-split-toggle .materialCheckBox').forEach(function (c) { c.classList.toggle('is-checked', !!on); });
    document.querySelectorAll('.js-import-split-value').forEach(function (i) { i.value = on ? 'swimlane' : ''; });
  }
  up.on('click', '.js-import-split-toggle', function (event) {
    event.preventDefault();
    Session.set('importSplitByProject', !Session.get('importSplitByProject'));
  });
  up.compiler('.js-import-split-toggle', function () { chooseSplit(window.Session && Session.get('importSplitByProject')); });
  // Meteor's Session, for the page state WeKan keeps there.
  if (!window.Session) {
    var session = {};
    window.Session = {
      get: function (key) { return session[key]; },
      set: function (key, value) {
        session[key] = value;
        if (key === 'importMembersMode') {
          document.querySelectorAll('.import-members-mode').forEach(function (box) { chooseMembersMode(box, value); });
        }
        if (key === 'importSplitByProject') chooseSplit(value);
      },
    };
  }

  up.compiler('input.js-browser-timezone', function (el) {
    try { el.value = Intl.DateTimeFormat().resolvedOptions().timeZone || ''; } catch (e) { el.value = ''; }
  });

  // The notifications indicator: alert while there are unread ones.
  up.compiler('.notifications-drawer-toggle', function () {
    refreshUser();
    var timer = setInterval(refreshUser, 4000);
    return function () { clearInterval(timer); };
  });

  // A button rule: run it, on the board or on the open card (rules.runButton).
  up.on('click', '.js-run-board-button, .js-run-card-button', function (event, el) {
    event.preventDefault();
    var args = ['rules.runButton', el.dataset.ruleId];
    var details = el.closest('.js-card-details');
    if (el.classList.contains('js-run-card-button') && details) args.push(details.dataset.card);
    callAsync.apply(null, args).then(function () { up.reload('.board-wrapper'); }, function () {});
  });

  // The sidebar's members (membersWidget): a member's menu, removing one, and
  // adding one found by a search.
  up.on('click', '.board-sidebar a.js-member', function (event, el) {
    event.preventDefault();
    openPopOver('member-' + el.dataset.user);
  });
  up.on('click', '.js-pop-over a.js-remove-member', function (event, el) {
    event.preventDefault();
    openPopOver('remove-member-' + el.dataset.user);
  });
  up.on('click', '.js-manage-board-members', function (event) {
    event.preventDefault();
    openPopOver('add-member');
    var input = document.querySelector('.js-pop-over .js-search-member-input');
    if (input) input.focus();
  });
  // WeKan's Filter sidebar: its Advanced Filter applied on change, by the
  // board drawn again with only the cards it matches - the server decides,
  // with the same query a rule's advanced-filter trigger uses.
  function applyBoardFilter(text) {
    var wrapper = document.querySelector('.board-wrapper');
    var view = document.querySelector('.js-filter-view');
    if (!wrapper) return;
    var url = new URL(window.location.href);
    url.searchParams.delete('advanced'); url.searchParams.delete('last');
    if (text) {
      url.searchParams.set('advanced', text);
      var last = view ? view.dataset.applied : '';
      if (last) url.searchParams.set('last', last);
    }
    up.navigate({ url: url.pathname + url.search, target: '.board-wrapper', history: 'replace', cache: false });
  }
  up.on('click', '.js-open-filter-view', function (event) {
    event.preventDefault();
    closePopOvers();
    var sb = document.querySelector('.board-sidebar');
    if (!sb) return;
    sb.classList.add('is-open');
    var view = sb.querySelector('.js-filter-view');
    if (view) view.classList.remove('is-hidden');
    sidebarState('open', true);
    sidebarState('filter', true);
  });
  up.on('click', '.js-filter-reset', function (event) {
    event.preventDefault();
    applyBoardFilter('');
  });
  up.on('change', '.js-field-advanced-filter', function (event, el) {
    applyBoardFilter(el.value.trim());
  });
  var memberSearch = null;
  up.compiler('.board-sidebar', function (sb) {
    if (sidebarState('open')) sb.classList.add('is-open');
    var filterView = sb.querySelector('.js-filter-view');
    if (filterView && sidebarState('filter')) filterView.classList.remove('is-hidden');
    var archives = sb.querySelector('.js-archives-view');
    if (archives && sidebarState('archives')) archives.classList.remove('is-hidden');
    var toggle = sb.querySelector('.js-toggle-show-activities');
    if (toggle && sidebarState('activities') && toggle.nextElementSibling.classList.contains('is-hidden')) toggle.click();
  });
  up.on('input', '.js-search-member-input', function (event, input) {
    clearTimeout(memberSearch);
    memberSearch = setTimeout(function () {
      var board = document.querySelector('.board-wrapper');
      var list = input.closest('.js-pop-over').querySelector('.js-member-search-results');
      fetch('/b/' + board.dataset.board + '/members/search?q=' + encodeURIComponent(input.value), { credentials: 'same-origin' })
        .then(function (r) { return r.json(); }).then(function (users) {
          list.innerHTML = '';
          users.forEach(function (u) {
            var li = document.createElement('li');
            li.className = 'item js-member-item';
            var a = document.createElement('a');
            a.className = 'name js-select-member member';
            a.href = '#';
            a.dataset.user = u._id;
            a.title = u.fullname + ' (' + u.username + ')';
            var name = document.createElement('span');
            name.className = 'full-name';
            name.textContent = u.fullname + ' ';
            var un = document.createElement('span');
            un.className = 'username';
            un.textContent = '(' + u.username + ')';
            a.appendChild(name);
            a.appendChild(un);
            li.appendChild(a);
            list.appendChild(li);
          });
        });
    }, 200);
  });
  up.on('click', '.js-member-search-results .js-select-member', function (event, el) {
    event.preventDefault();
    var board = document.querySelector('.board-wrapper');
    var form = new FormData();
    form.append('userId', el.dataset.user);
    fetch('/b/' + board.dataset.board + '/members', { method: 'POST', body: form, credentials: 'same-origin' })
      .then(function () { closePopOvers(); up.reload('.board-wrapper'); });
  });

  // The card's actions menu (cardDetailsActionsPopup) and Archive's confirmation.
  up.on('click', '.js-open-card-details-menu', function (event) {
    event.preventDefault();
    openPopOver('card-actions');
  });
  up.on('click', '.js-pop-over a.js-move-card', function (event) {
    event.preventDefault();
    openPopOver('move-card');
  });
  up.on('click', '.js-pop-over a.js-copy-card', function (event) {
    event.preventDefault();
    openPopOver('copy-card');
  });
  // Another board chosen: its swimlanes and lists in the selects.
  up.on('change', '.js-card-target .js-select-boards', function (event, sel) {
    var form = sel.closest('form');
    fetch('/b/' + sel.value + '/places', { credentials: 'same-origin' }).then(function (r) { return r.json(); }).then(function (p) {
      [['swimlanes', '.js-select-swimlanes'], ['lists', '.js-select-lists']].forEach(function (k) {
        var target = form.querySelector(k[1]);
        target.innerHTML = '';
        (p[k[0]] || []).forEach(function (o) {
          var opt = document.createElement('option');
          opt.value = o._id;
          opt.textContent = o.n + '. ' + o.title;
          target.appendChild(opt);
        });
      });
    });
  });
  up.on('click', '.js-pop-over a.js-archive', function (event) {
    event.preventDefault();
    openPopOver('card-archive');
  });

  // The card's labels (cardLabelsPopup) and a new one (createLabelPopup).
  up.on('click', '.js-add-labels', function (event) {
    event.preventDefault();
    openPopOver('card-labels');
  });
  up.on('click', '.js-pop-over .js-add-label', function (event) {
    event.preventDefault();
    openPopOver('create-label');
  });
  up.on('click', '.js-palette-color', function (event, el) {
    var radio = el.querySelector('input');
    if (radio) radio.checked = true;
    el.parentNode.querySelectorAll('.js-palette-color').forEach(function (p) { p.classList.toggle('is-selected', p === el); });
  });
  up.on('click', '.js-change-visibility', function (event) {
    event.preventDefault();
    openPopOver('visibility');
  });
  up.on('click', '.js-edit-board-title', function (event) {
    event.preventDefault();
    openPopOver('board-title');
  });
  // Archived Items: the board's Archive in its sidebar (archivesSidebar).
  up.on('click', '.js-open-archives', function (event) {
    event.preventDefault();
    closePopOvers();
    var sb = document.querySelector('.board-sidebar');
    if (!sb) return;
    sb.classList.add('is-open');
    var view = sb.querySelector('.js-archives-view');
    if (view) view.classList.remove('is-hidden');
    sidebarState('open', true);
    sidebarState('archives', true);
  });
  up.on('click', '.js-restore-card, .js-restore', function (event, el) {
    event.preventDefault();
    // A card whose list is gone or full asks for another list first (#6772).
    if (el.dataset.pop) { openPopOver(el.dataset.pop); return; }
    up.submit(el.closest('form'));
  });
  up.on('click', '.js-restore-all-cards', function (event, el) {
    event.preventDefault();
    up.submit(el.closest('form'));
  });
  // A pop-over the server asked to show at once: Restore all's left-out count.
  // and the card a restore was refused for, its list being full.
  up.compiler('.js-open-pop-on-load', function (el) {
    setTimeout(function () { openPopOver(el.getAttribute('data-pop')); }, 0);
  });

  // A card full-screen, or not: the user's own choice (toggleCardMaximized).
  // WeKan's card caret: the user's own fold of the opened card's details.
  up.on('click', '.js-card-collapse-toggle', function (event, el) {
    event.preventDefault();
    callAsync('setCardCollapsed', el.dataset.collapsed !== 'true').then(function () { up.reload('.board-wrapper'); });
  });
  up.on('click', '.js-maximize-card-details, .js-minimize-card-details', function (event) {
    event.preventDefault();
    callAsync('toggleCardMaximized').then(function () { up.reload('.board-wrapper'); });
  });
  // Copy link: the card's absolute address to the clipboard.
  up.on('click', '.js-copy-card-link', function (event, el) {
    event.preventDefault();
    var url = window.location.origin + el.dataset.path;
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(url);
    closePopOvers();
  });
  // The card title's editor: open on its leading part.
  up.on('click', '.card-details-title-edit-zone', function (event, el) {
    event.preventDefault();
    var form = el.closest('.card-details-header').querySelector('.js-card-details-title');
    if (!form) return;
    form.classList.remove('is-hidden');
    form.querySelector('textarea').focus();
  });

  // A link in a minicard's title opens on its own, without opening the card
  // or its editor (#6639): caught before the minicard sees the click.
  document.addEventListener('click', function (event) {
    var link = event.target.closest && event.target.closest('.minicard-title-text .viewer a');
    if (!link) return;
    event.preventDefault();
    event.stopPropagation();
    window.open(link.href, '_blank', 'noopener');
  }, true);

  // An invitation to the board: accept it, or decline it and go to All Boards.
  up.on('click', '.js-member-invite-accept', function (event, el) {
    event.preventDefault();
    var board = el.closest('.board-wrapper');
    callAsync('acceptInvite', board.dataset.board).then(function () { up.reload('.board-wrapper'); });
  });
  up.on('click', '.js-member-invite-decline', function (event, el) {
    event.preventDefault();
    var board = el.closest('.board-wrapper');
    callAsync('quitBoard', board.dataset.board).then(function () { up.navigate({ url: '/' }); });
  });

  // The board's star: the user's favorite (WeKan's toggleBoardStar).
  up.on('click', '.js-star-board', function (event, el) {
    event.preventDefault();
    var board = el.closest('.board-wrapper');
    if (!board) return;
    var on = !el.classList.contains('is-active');
    el.classList.toggle('is-active', on);
    var icon = el.querySelector('.fa');
    if (icon) {
      icon.classList.toggle('fa-star', on);
      icon.classList.toggle('fa-star-o', !on);
    }
    el.title = on ? el.dataset.unstar : el.dataset.star;
    callAsync('toggleBoardStar', board.dataset.board).catch(function () {
      el.classList.toggle('is-active', !on);
    });
  });

  // A list's caret collapses it, for this user, in this swimlane (#6712):
  // at once on the page, and kept in the profile as WeKan's
  // setListCollapsedState, so a reload shows it the same.
  up.on('click', '.js-collapse', function (event, el) {
    event.preventDefault();
    var list = el.closest('.js-list');
    var board = el.closest('.board-wrapper');
    if (!list || !board) return;
    var collapsed = !list.classList.contains('list-collapsed');
    list.classList.toggle('list-collapsed', collapsed);
    var icon = el.querySelector('.fa');
    if (icon) {
      icon.classList.toggle('fa-caret-right', collapsed);
      icon.classList.toggle('fa-caret-down', !collapsed);
    }
    var label = collapsed ? el.dataset.uncollapse : el.dataset.collapse;
    if (label) { el.title = label; el.setAttribute('aria-label', label); }
    var key = list.dataset.list + (list.dataset.swimlane ? ':' + list.dataset.swimlane : '');
    callAsync('setListCollapsedState', board.dataset.board, key, collapsed).catch(function () {});
  });

  // A list's card composer: on the page only while open - at the top of the
  // list from its "+", at the top or the bottom from its menu - and × takes it
  // away (WeKan's inlined form).
  var menuList = null; // the list whose menu is open
  function openComposer(list, position) {
    if (!list) return;
    list.querySelectorAll('form.js-card-composer').forEach(function (f) { f.remove(); });
    var t = list.querySelector('template.js-card-composer-template');
    var body = list.querySelector('.js-minicards');
    if (!t || !body) return;
    var form = t.content.firstElementChild.cloneNode(true);
    form.querySelector('.js-card-position').value = position;
    if (position === 'top') body.parentNode.insertBefore(form, body);
    else body.parentNode.insertBefore(form, body.nextSibling);
    up.hello(form);
    form.querySelector('textarea').focus();
  }
  // Multi-Selection: on, the sidebar shows what to do with the selection;
  // a list's menu selects all its cards; the selection goes to the Archive
  // with WeKan's archiveSelectedCards.
  // The selection outlasts the board's reloads, as WeKan's Multi-Selection.
  // objects are WeKan's structural selection: swimlanes, lists, checklists
  // and items, "kind:id", beside the cards.
  var multiSelection = { on: false, cards: {}, objects: {} };
  function showMultiSelection(wrapper) {
    if (!multiSelection.on) return;
    var sb = wrapper.querySelector('.board-sidebar');
    if (sb) sb.classList.add('is-open');
    var view = wrapper.querySelector('.js-multiselection-view');
    if (view) view.classList.remove('is-hidden');
    wrapper.querySelectorAll('.js-minicard').forEach(function (c) {
      c.classList.toggle('is-selected', !!multiSelection.cards[c.dataset.card]);
    });
    wrapper.querySelectorAll('.js-toggle-multi-selection').forEach(function (b) {
      var on = !!multiSelection.cards[b.dataset.card];
      b.classList.remove('is-hidden');
      b.classList.toggle('is-checked', on);
      b.setAttribute('aria-checked', on ? 'true' : 'false');
    });
    wrapper.querySelectorAll('.js-structural-selection').forEach(function (b) {
      var on = !!multiSelection.objects[b.dataset.kind + ':' + b.dataset.id];
      b.classList.remove('is-hidden');
      b.classList.toggle('is-checked', on);
      b.setAttribute('aria-checked', on ? 'true' : 'false');
    });
  }
  // WeKan's folds, each the user's own: a swimlane (setSwimlaneCollapsedState),
  // a minicard (setCardCollapsedState) and a minicard's checklist - the same
  // fold as in the opened card (setCollapsedCardSection, "checklist-<id>").
  // A fold never opens the card it sits on.
  function toggleFold(el) {
    var board = document.querySelector('.board-wrapper[data-board]');
    if (!board) return;
    var folded = el.dataset.collapsed !== 'true', p;
    if (el.classList.contains('js-collapse-swimlane')) {
      p = callAsync('setSwimlaneCollapsedState', board.dataset.board, el.dataset.swimlane, folded);
    } else if (el.classList.contains('js-collapse-minicard')) {
      p = callAsync('setCardCollapsedState', board.dataset.board, el.dataset.card, folded);
    } else {
      var mini = el.closest('.js-minicard[data-card]');
      if (!mini) return;
      p = callAsync('setCollapsedCardSection', mini.dataset.card, 'checklist-' + el.dataset.checklist, folded);
    }
    p.then(function () { up.reload('.board-wrapper'); }, function () {});
  }
  document.addEventListener('click', function (event) {
    var el = event.target.closest && event.target.closest('.js-collapse-swimlane, .js-collapse-minicard, .minicard-checklist .js-collapse-checklist');
    if (!el) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    toggleFold(el);
  }, true);
  document.addEventListener('keydown', function (event) {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    var el = event.target.closest && event.target.closest('.js-collapse-minicard, .minicard-checklist .js-collapse-checklist');
    if (!el) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    toggleFold(el);
  }, true);
  // A selection checkbox selects; it never opens the card it sits on.
  document.addEventListener('click', function (event) {
    var b = event.target.closest && event.target.closest('.js-structural-selection, .js-toggle-multi-selection');
    if (!b) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    if (b.classList.contains('js-structural-selection')) {
      var key = b.dataset.kind + ':' + b.dataset.id;
      if (multiSelection.objects[key]) delete multiSelection.objects[key];
      else multiSelection.objects[key] = true;
    } else if (multiSelection.cards[b.dataset.card]) delete multiSelection.cards[b.dataset.card];
    else multiSelection.cards[b.dataset.card] = true;
    var wrapper = b.closest('.board-wrapper');
    if (wrapper) showMultiSelection(wrapper);
  }, true);
  // WeKan's moveObjectsPopup: board, swimlane, list, card, checklist and item
  // offered step by step (structuralMoveOptions), a change clearing the steps
  // after it; Move sends the mixed selection to moveBoardObjects.
  var moveFields = ['boardId', 'swimlaneId', 'listId', 'cardId', 'checklistId', 'itemId'];
  function mixedSelection() {
    var out = Object.keys(multiSelection.objects).map(function (k) {
      var i = k.indexOf(':');
      return { kind: k.slice(0, i), id: k.slice(i + 1) };
    });
    Object.keys(multiSelection.cards).forEach(function (id) { out.push({ kind: 'card', id: id }); });
    return out;
  }
  function loadMoveOptions(form) {
    var target = form._target;
    return callAsync('structuralMoveOptions', target).then(function (opts) {
      moveFields.forEach(function (key) {
        var sel = form.querySelector('.js-object-destination[data-field="' + key + '"]');
        if (!sel) return;
        sel.replaceChildren();
        if (key !== 'boardId') {
          var empty = document.createElement('option');
          empty.value = '';
          empty.textContent = sel.dataset.empty || '—';
          sel.appendChild(empty);
        }
        (opts[key] || []).forEach(function (o) {
          var opt = document.createElement('option');
          opt.value = o._id;
          opt.textContent = o.title;
          if (target[key] === o._id) opt.selected = true;
          sel.appendChild(opt);
        });
      });
    }, function (e) { showMoveError(form, e); });
  }
  function showMoveError(form, e) {
    var w = form.querySelector('.js-move-objects-error');
    w.textContent = (e && (e.reason || e.message)) || '';
    w.classList.toggle('is-hidden', !w.textContent);
  }
  // WeKan's mixed drag (structuralSelection.js): a selected object dragged
  // onto a swimlane, list, card, checklist or item moves the whole selection
  // there, before or after it as the drop's half says. Read from the page's
  // data attributes; names in the preview are text, never cloned controls.
  function structuralEntityAt(el, drop) {
    if (!el || !el.closest) return null;
    var tries = [
      ['item', '.minicard-checklist-item[data-item], .js-checklist-item[data-item]', 'item'],
      ['checklist', '.minicard-checklist[data-checklist], .js-checklist[data-checklist]', 'checklist'],
      ['card', '.js-minicard[data-card]', 'card'],
      ['list', '.js-list[data-list]', 'list'],
    ];
    for (var i = 0; i < tries.length; i++) {
      var node = el.closest(tries[i][1]);
      if (node) return { kind: tries[i][0], id: node.dataset[tries[i][2]], node: node };
    }
    var head = el.closest('.js-swimlane-header[data-swimlane]');
    if (head) return { kind: 'swimlane', id: head.dataset.swimlane, node: head };
    var lane = drop && el.closest('.js-swimlane[data-swimlane]');
    if (lane) return { kind: 'swimlane', id: lane.dataset.swimlane, node: lane };
    return null;
  }
  function structuralTargetFor(entity, boardID) {
    var t = { boardId: boardID, position: 'before' };
    var node = entity.node, kind = entity.kind;
    if (kind === 'item') {
      t.itemId = entity.id;
      var cl = node.closest('[data-checklist]');
      if (cl) t.checklistId = cl.dataset.checklist;
      kind = 'checklist';
    } else if (kind === 'checklist') t.checklistId = entity.id;
    if (kind === 'checklist') {
      var mc = node.closest('.js-minicard[data-card]') || node.closest('.js-card-details[data-card]');
      if (mc) t.cardId = mc.dataset.card;
      kind = 'card';
    } else if (kind === 'card') t.cardId = entity.id;
    if (kind === 'card' || kind === 'list') {
      var list = node.closest('.js-list[data-list]');
      if (list) { t.listId = list.dataset.list; t.swimlaneId = list.dataset.swimlane || undefined; }
    }
    if (!t.swimlaneId) {
      var lane = node.closest('[data-swimlane]');
      if (lane) t.swimlaneId = lane.dataset.swimlane;
    }
    Object.keys(t).forEach(function (k) { if (t[k] === undefined) delete t[k]; });
    return t;
  }
  function structuralSelected(entity) {
    return entity.kind === 'card' ? !!multiSelection.cards[entity.id] : !!multiSelection.objects[entity.kind + ':' + entity.id];
  }
  // Pointer events, in the capture phase: the board's own drag (interact.js)
  // listens to them, so a drag of the selection never starts it too.
  var structuralDrag = null;
  document.addEventListener('pointerdown', function (event) {
    if (event.button !== 0 || !multiSelection.on || !Object.keys(multiSelection.objects).length) return;
    if (event.target.closest('input,textarea,button,select,.materialCheckBox')) return;
    var entity = structuralEntityAt(event.target, false);
    if (!entity || !structuralSelected(entity)) return;
    event.stopImmediatePropagation();
    event.preventDefault();
    structuralDrag = { x: event.clientX, y: event.clientY, selection: mixedSelection() };
  }, true);
  document.addEventListener('pointermove', function (event) {
    var d = structuralDrag;
    if (!d) return;
    if (!d.preview && Math.hypot(event.clientX - d.x, event.clientY - d.y) < 7) return;
    if (!d.preview) {
      d.preview = document.createElement('div');
      d.preview.className = 'structural-drag-preview';
      var count = document.createElement('div');
      count.className = 'structural-drag-count';
      count.textContent = String(d.selection.length);
      d.preview.appendChild(count);
      document.body.appendChild(d.preview);
    }
    event.preventDefault();
    d.preview.style.left = (event.clientX + 12) + 'px';
    d.preview.style.top = (event.clientY + 12) + 'px';
    if (d.target) d.target.node.classList.remove('structural-drop-target');
    d.target = structuralEntityAt(document.elementFromPoint(event.clientX, event.clientY), true);
    if (d.target) d.target.node.classList.add('structural-drop-target');
  }, true);
  document.addEventListener('pointerup', function (event) {
    var d = structuralDrag;
    if (!d) return;
    structuralDrag = null;
    if (d.preview) d.preview.remove();
    if (d.target) d.target.node.classList.remove('structural-drop-target');
    if (!d.preview || !d.target) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    document.addEventListener('click', function (c) { c.preventDefault(); c.stopImmediatePropagation(); }, { capture: true, once: true });
    var board = document.querySelector('.board-wrapper[data-board]');
    if (!board) return;
    var destination = structuralTargetFor(d.target, board.dataset.board);
    // Before or after by the drop's half - of a list's header when dropped on
    // it, the part of the list one aims at, not of the whole column.
    var under = document.elementFromPoint(event.clientX, event.clientY);
    var header = d.target.kind === 'list' && under && under.closest && under.closest('.list-header');
    var bounds = (header && d.target.node.contains(header) ? header : d.target.node).getBoundingClientRect();
    destination.position = event.clientY < bounds.y + bounds.height / 2 ? 'before' : 'after';
    callAsync('moveBoardObjects', board.dataset.board, d.selection, destination, { drag: true }).then(function () {
      multiSelection = { on: false, cards: {}, objects: {} };
      up.reload('.board-wrapper');
    }, function (e) {
      // An incomplete destination is corrected in the Move pop-over.
      openPopOver('move-objects');
      var form = document.querySelector('.js-pop-over .js-move-objects-form');
      if (!form) return;
      form._target = destination;
      loadMoveOptions(form).then(function () { showMoveError(form, e); });
    });
  }, true);
  document.addEventListener('keydown', function (event) {
    if (event.key !== 'Escape' || !structuralDrag) return;
    if (structuralDrag.preview) structuralDrag.preview.remove();
    if (structuralDrag.target) structuralDrag.target.node.classList.remove('structural-drop-target');
    structuralDrag = null;
  }, true);
  up.on('click', '.js-move-selection', function (event) {
    event.preventDefault();
    var board = document.querySelector('.board-wrapper[data-board]');
    if (!board || !mixedSelection().length) return;
    openPopOver('move-objects');
    var form = document.querySelector('.js-pop-over .js-move-objects-form');
    form._target = { boardId: board.dataset.board };
    loadMoveOptions(form);
  });
  document.addEventListener('change', function (event) {
    var sel = event.target;
    if (!sel.matches || !sel.matches('.js-move-objects-form .js-object-destination')) return;
    var form = sel.closest('.js-move-objects-form');
    var key = sel.dataset.field;
    var next = Object.assign({}, form._target);
    next[key] = sel.value;
    if (!sel.value) delete next[key];
    moveFields.slice(moveFields.indexOf(key) + 1).forEach(function (k) { delete next[k]; });
    form._target = next;
    showMoveError(form, null);
    loadMoveOptions(form);
  });
  up.on('click', '.js-move-objects', function (event, el) {
    event.preventDefault();
    var form = el.closest('.js-move-objects-form');
    var board = document.querySelector('.board-wrapper[data-board]');
    if (!form || !board || el.disabled) return;
    el.disabled = true;
    var target = Object.assign({}, form._target, { position: form.querySelector('.js-object-position').value });
    callAsync('moveBoardObjects', board.dataset.board, mixedSelection(), target).then(function () {
      multiSelection = { on: false, cards: {}, objects: {} };
      closePopOvers();
      up.reload('.board-wrapper');
    }, function (e) {
      el.disabled = false;
      showMoveError(form, e);
    });
  });
  up.compiler('.board-wrapper', function (wrapper) { showMultiSelection(wrapper); });
  up.on('click', '.js-multiselection-activate', function (event, el) {
    event.preventDefault();
    multiSelection.on = true;
    showMultiSelection(el.closest('.board-wrapper'));
  });
  up.on('click', '.js-pop-over .js-select-cards', function (event) {
    event.preventDefault();
    var list = menuList;
    closePopOvers();
    if (!list) return;
    multiSelection.on = true;
    list.querySelectorAll('.js-minicard').forEach(function (c) { multiSelection.cards[c.dataset.card] = true; });
    var wrapper = document.querySelector('.board-wrapper');
    if (wrapper) showMultiSelection(wrapper);
  });
  up.on('click', '.js-archive-selection', function (event) {
    event.preventDefault();
    var board = document.querySelector('.board-wrapper');
    var ids = Object.keys(multiSelection.cards);
    if (!ids.length) return;
    callAsync('archiveSelectedCards', board.dataset.board, ids).then(function () {
      multiSelection = { on: false, cards: {}, objects: {} };
      up.reload('.board-wrapper');
    },
      function (e) { window.alert(e.reason || e.message); });
  });
  // Watch or Unwatch the list whose menu is open (WeKan's watch method).
  up.on('click', '.js-pop-over .js-toggle-watch-list', function (event) {
    event.preventDefault();
    var list = menuList;
    closePopOvers();
    if (!list) return;
    callAsync('watch', 'list', list.dataset.list, list.dataset.watching ? null : 'watching').then(function () { up.reload('.board-wrapper'); });
  });
  // WeKan's List permissions (#4527): the page of the list whose menu is open.
  up.on('click', '.js-pop-over .js-list-permissions', function (event, el) {
    event.preventDefault();
    var list = menuList;
    closePopOvers();
    if (!list) return;
    up.navigate({ url: el.dataset.base + '/lists/' + encodeURIComponent(list.dataset.list) + '/permissions' });
  });
  up.on('click', '.js-open-list-menu', function (event, el) {
    event.preventDefault();
    menuList = el.closest('.js-list');
    openPopOver('list-menu');
    var w = document.querySelector('.js-pop-over .js-toggle-watch-list');
    if (w) w.textContent = menuList.dataset.watching ? w.dataset.unwatch : w.dataset.watch;
  });
  // listBody.jade's "+ Add card" under the list: the composer at the bottom.
  up.on('click', '.js-list a.open-minicard-composer', function (event, el) {
    event.preventDefault();
    openComposer(el.closest('.js-list'), 'bottom');
  });
  up.on('click', '.js-add-card', function (event, el) {
    event.preventDefault();
    var inMenu = el.closest('.js-pop-over');
    var list = inMenu ? menuList : el.closest('.js-list');
    if (inMenu) closePopOvers();
    openComposer(list, el.classList.contains('list-header-plus-bottom') ? 'bottom' : 'top');
  });
  // WeKan's #4693: tick a subtask done in its parent's Subtasks.
  up.on('click', '.js-toggle-subtask-done', function (event, el) {
    event.preventDefault();
    var done = el.getAttribute('aria-checked') !== 'true';
    callAsync('setSubtaskDone', el.dataset.parent, el.dataset.subtask, done).then(function () {
      up.reload('.board-wrapper');
    }, function () {});
  });
  up.on('click', '.js-close-inlined-form', function (event, el) {
    event.preventDefault();
    var form = el.closest('.js-inlined-form');
    if (form.classList.contains('js-card-composer')) form.remove();
    else closeInlinedForm(form);
    if (staleUnderForm) { staleUnderForm = false; setTimeout(reloadBoard, 0); }
  });

  // Ctrl/Cmd-click selects cards for a drag that moves many.
  up.on('click', '.js-minicard', function (event, el) {
    if (el.getAttribute('data-dragged') === '1') {
      el.removeAttribute('data-dragged');
      event.preventDefault();
      event.stopImmediatePropagation();
      return;
    }
    if (event.ctrlKey || event.metaKey) {
      event.preventDefault();
      event.stopImmediatePropagation();
      if (el.classList.toggle('is-selected')) multiSelection.cards[el.dataset.card] = true;
      else delete multiSelection.cards[el.dataset.card];
    }
  });

  // Where the pointer is: the list body under it, and the place among its cards.
  function dropTarget(x, y, moving) {
    var stack = document.elementsFromPoint(x, y);
    for (var i = 0; i < stack.length; i++) {
      // The dragged cards are under the pointer too, still inside the list they
      // came from: look past them.
      if (moving.some(function (m) { return m.contains(stack[i]); })) continue;
      var body = stack[i].closest && stack[i].closest('.js-minicards');
      if (!body) continue;
      var cards = Array.prototype.filter.call(body.querySelectorAll('.js-minicard'), function (c) {
        return moving.indexOf(c) === -1;
      });
      var before = null, after = null;
      for (var j = 0; j < cards.length; j++) {
        var r = cards[j].getBoundingClientRect();
        if (y < r.top + r.height / 2) { after = cards[j]; break; }
        before = cards[j];
      }
      return { body: body, before: before, after: after };
    }
    return null;
  }

  function sortOf(el) { return parseFloat(el.getAttribute('data-sort')) || 0; }

  // The sorts of n cards placed between two neighbours.
  function sorts(target, n) {
    var lo = target.before ? sortOf(target.before) : null;
    var hi = target.after ? sortOf(target.after) : null;
    var out = [];
    for (var i = 1; i <= n; i++) {
      if (lo === null && hi === null) out.push(i - 1);
      else if (lo === null) out.push(hi - (n + 1 - i));
      else if (hi === null) out.push(lo + i);
      else out.push(lo + (hi - lo) * i / (n + 1));
    }
    return out;
  }

  function resetPosition(el) {
    el.style.transform = '';
    el.removeAttribute('data-x');
    el.removeAttribute('data-y');
    el.classList.remove('is-dragging');
  }

  function drop(event) {
    var el = event.target;
    var wrapper = el.closest('.board-wrapper');
    var moving = el.classList.contains('is-selected')
      ? Array.prototype.slice.call(wrapper.querySelectorAll('.js-minicard.is-selected'))
      : [el];
    var target = dropTarget(event.client.x, event.client.y, moving);
    resetPosition(el);
    if (!target) return Promise.resolve(false);
    var place = sorts(target, moving.length);
    var moves = moving.map(function (card, i) {
      return {
        cardId: card.getAttribute('data-card'),
        listId: target.body.getAttribute('data-list'),
        swimlaneId: target.body.getAttribute('data-swimlane'),
        sort: place[i],
      };
    });
    // Shown at once where it was dropped; the server's answer redraws the board.
    var anchor = target.after;
    moving.forEach(function (card) {
      card.classList.remove('is-selected');
      delete multiSelection.cards[card.dataset.card];
      target.body.insertBefore(card, anchor);
    });
    return fetch(wrapper.getAttribute('data-move'), {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ moves: moves }),
    }).then(function (res) { return res.ok; }, function () { return false; });
  }

  if (window.interact) {
    interact.maxInteractions(Infinity); // MultiDrag: one interaction per pointer
    interact('.js-minicard').draggable({
      inertia: false,
      autoScroll: true,
      listeners: {
        start: function (event) {
          dragging++;
          event.target.classList.add('is-dragging');
        },
        move: function (event) {
          var t = event.target;
          var x = (parseFloat(t.getAttribute('data-x')) || 0) + event.dx;
          var y = (parseFloat(t.getAttribute('data-y')) || 0) + event.dy;
          t.style.transform = 'translate(' + x + 'px, ' + y + 'px)';
          t.setAttribute('data-x', x);
          t.setAttribute('data-y', y);
        },
        end: function (event) {
          event.target.setAttribute('data-dragged', '1');
          setTimeout(function () { event.target.removeAttribute('data-dragged'); }, 0);
          drop(event).then(function () {
            dragging = Math.max(0, dragging - 1);
            if (dragging === 0) reloadBoard();
          });
        },
      },
    });
  }

  // WeKan's Rules page: its title, then a trigger, then an action, which
  // saves the rule (rules.createRule) - one pane at a time, as rulesMain.
  var ruleState = { name: '', trigger: null };
  function rulesPage(el) { return el.closest('.rules-page'); }
  function showPane(page, name) {
    var t = page.querySelector('template.js-rules-pane[data-pane="' + name + '"]');
    var stage = page.querySelector('.js-rules-stage');
    if (!t || !stage) return;
    stage.innerHTML = '';
    stage.appendChild(t.content.cloneNode(true));
    stage.querySelectorAll('.js-rule-name').forEach(function (n) { n.textContent = ruleState.name; });
    up.hello(stage);
  }
  up.compiler('.rules-page', function (page) {
    if (page.dataset.view === 'import') {
      showPane(page, 'list');
      openPopOver('rules-import-export');
    } else {
      showPane(page, page.dataset.view || 'list');
    }
  });
  // On the Rules page, Board Settings' Rules entries switch the view in place.
  function rulesMenuEntry(view) {
    return function (event) {
      var page = document.querySelector('.rules-page');
      if (!page) return; // on the board they are links to the Rules page
      event.preventDefault();
      closePopOvers();
      document.querySelector('.board-sidebar').classList.remove('is-open');
      if (view === 'import') openPopOver('rules-import-export');
      else showPane(page, view);
    };
  }
  up.on('click', '.js-open-rules-view, .js-open-rules-list-view', rulesMenuEntry('list'));
  up.on('click', '.js-open-rules-workflow-view', rulesMenuEntry('workflow'));
  up.on('click', '.js-open-rules-import-export', rulesMenuEntry('import'));
  // WeKan's rulesBots: add and remove a bot, hand a card to one, stop, answer
  // and reply - each a method, then the view again; drag a card to a column
  // to move it, as on the board.
  function botsDone(promise, errorBox) {
    return promise.then(function () { location.reload(); }, function (e) {
      var box = errorBox && document.querySelector(errorBox);
      if (box) { box.textContent = (e && (e.reason || e.error)) || String(e); box.hidden = false; }
    });
  }
  up.on('submit', '.js-bots-add', function (event, form) {
    event.preventDefault();
    var view = form.closest('.bots-view');
    var token = form.querySelector('.js-bots-token').value;
    var doc = { boardId: view.dataset.board, title: form.querySelector('.js-bots-title').value.trim(),
      url: form.querySelector('.js-bots-url').value.trim(), bot: true, enabled: true };
    if (token) doc.token = token;
    botsDone(Meteor.callAsync('/integrations/insert', doc), '.js-bots-add-error');
  });
  up.on('click', '.js-bots-remove', function (event, el) {
    event.preventDefault();
    botsDone(Meteor.callAsync('/integrations/remove', { _id: el.getAttribute('data-id') }));
  });
  up.on('click', '.js-bots-dispatch', function (event, el) {
    var choose = el.closest('.bots-drawer').querySelector('.js-bots-choose');
    if (choose && choose.value) botsDone(Meteor.callAsync('botDispatch', el.getAttribute('data-card-id'), choose.value));
  });
  up.on('click', '.js-bots-stop', function (event, el) {
    botsDone(Meteor.callAsync('botStop', el.getAttribute('data-run-id')));
  });
  up.on('click', '.js-bots-answer', function (event, el) {
    botsDone(Meteor.callAsync('botAnswer', el.getAttribute('data-run-id'), el.getAttribute('data-value')));
  });
  up.on('submit', '.js-bots-reply', function (event, form) {
    event.preventDefault();
    var text = form.querySelector('.js-bots-reply-text').value;
    if (text.trim()) botsDone(Meteor.callAsync('botReply', form.getAttribute('data-run-id'), text));
  });
  var botsDragged = null;
  up.on('dragstart', '.js-bots-card', function (event, el) {
    botsDragged = el;
    if (event.dataTransfer) event.dataTransfer.setData('text/plain', el.getAttribute('data-card-id'));
  });
  up.on('dragover', '.js-bots-drop', function (event) { event.preventDefault(); });
  up.on('drop', '.js-bots-drop', function (event, column) {
    event.preventDefault();
    var card = botsDragged;
    botsDragged = null;
    if (!card || card.closest('.js-bots-drop') === column) return;
    var view = column.closest('.bots-view');
    var last = 0;
    column.querySelectorAll('.js-bots-card').forEach(function (c) { last = Math.max(last, Number(c.getAttribute('data-sort')) || 0); });
    fetch('/b/' + view.dataset.board + '/move', {
      method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ moves: [{ cardId: card.getAttribute('data-card-id'), listId: column.getAttribute('data-list-id'),
        swimlaneId: card.getAttribute('data-swimlane-id'), sort: last + 1 }] }),
    }).then(function () { location.reload(); });
  });
  // Export the ticked rules, or all of them.
  up.on('click', '.js-rules-export-json, .js-rules-export-csv', function (event, el) {
    var page = document.querySelector('.rules-page');
    if (!page) return;
    var ids = Array.prototype.map.call(page.querySelectorAll('.js-rule-select:checked'), function (b) { return b.dataset.ruleId; });
    if (ids.length) el.href = el.href.split('&ids=')[0] + '&ids=' + ids.join(',');
  });
  // WeKan's rulesImportExport parsers: its round-trippable CSV, Trello Butler
  // text and n8n / Node-RED workflows, each a list of { title, trigger, action }.
  function csvRecords(text) {
    var rows = [], row = [], field = '', quoted = false;
    for (var i = 0; i < text.length; i++) {
      var ch = text[i];
      if (quoted) {
        if (ch === '"' && text[i + 1] === '"') { field += '"'; i++; }
        else if (ch === '"') quoted = false;
        else field += ch;
      } else if (ch === '"') quoted = true;
      else if (ch === ',') { row.push(field); field = ''; }
      else if (ch === '\n' || ch === '\r') {
        if (ch === '\r' && text[i + 1] === '\n') i++;
        row.push(field); field = '';
        if (row.length > 1 || row[0] !== '') rows.push(row);
        row = [];
      } else field += ch;
    }
    if (field !== '' || row.length) { row.push(field); rows.push(row); }
    return rows;
  }
  function csvToRules(text) {
    var rows = csvRecords(String(text || '').trim());
    if (!rows.length) return [];
    var head = rows.shift();
    var json = function (v) { try { return v ? JSON.parse(v) : {}; } catch (e) { return {}; } };
    return rows.map(function (cells) {
      var r = {};
      head.forEach(function (h, i) { r[h] = cells[i]; });
      if (!r.triggerType || !r.actionType) return null;
      var trigger = json(r.triggerFields), action = json(r.actionFields);
      trigger.activityType = r.triggerType; action.actionType = r.actionType;
      return { title: r.title, trigger: trigger, action: action };
    }).filter(Boolean);
  }
  function parseTrelloButler(text) {
    var rules = [], unmapped = [];
    String(text || '').split('\n').map(function (l) { return l.trim(); }).filter(Boolean).forEach(function (line) {
      // The list name as written: a trigger matches its list by exact name.
      var m = line.match(/when a card is added to list ["\u201c](.+?)["\u201d].*move the card to the (top|bottom)/i);
      if (!m) { unmapped.push(line); return; }
      rules.push({ title: line, trigger: { activityType: 'createCard', listName: m[1], swimlaneName: '*', cardTitle: '*', userId: '*' },
        action: { actionType: m[2].toLowerCase() === 'top' ? 'moveCardToTop' : 'moveCardToBottom', listName: '*', swimlaneName: '*' } });
    });
    return { rules: rules, unmapped: unmapped };
  }
  function workflowTrigger(type, name) {
    var s = ((type || '') + ' ' + (name || '')).toLowerCase();
    if (/schedule|cron|interval|inject/.test(s)) return { activityType: 'scheduledTrigger', scheduleKind: 'calendar', scheduleType: 'daily', atTime: '09:00', listName: '*', swimlaneName: '*' };
    if (/trigger|webhook|http in|http-in|start/.test(s)) return { activityType: 'createCard', listName: '*', swimlaneName: '*', cardTitle: '*', userId: '*' };
    return null;
  }
  function workflowAction(type, name) {
    var s = ((type || '') + ' ' + (name || '')).toLowerCase();
    if (/archive/.test(s)) return { actionType: 'archive' };
    if (/move.*top|to top/.test(s)) return { actionType: 'moveCardToTop', listName: '*', swimlaneName: '*' };
    if (/move.*bottom/.test(s)) return { actionType: 'moveCardToBottom', listName: '*', swimlaneName: '*' };
    if (/complete|done/.test(s)) return { actionType: 'markCardComplete' };
    if (/email|mail|smtp|gmail/.test(s)) return { actionType: 'sendEmail', emailTo: '', emailSubject: 'Imported workflow', emailMsg: '' };
    if (/create.*card|wekan|card/.test(s)) return { actionType: 'createCard', cardName: name || 'Imported card', listName: '*', swimlaneName: '*' };
    return null;
  }
  function parseWorkflow(text, format) {
    var data;
    try { data = JSON.parse(text); } catch (e) { return { rules: [], unmapped: [], error: 'invalid JSON' }; }
    var fmt = format;
    if (!fmt || fmt === 'auto') {
      if (data && data.nodes && data.connections) fmt = 'n8n';
      else if (Array.isArray(data) || (data && data.flows)) fmt = 'nodered';
    }
    var rules = [], unmapped = [];
    if (fmt === 'n8n') {
      var byName = {};
      (data.nodes || []).forEach(function (n) { byName[n.name] = n; });
      Object.keys(data.connections || {}).forEach(function (src) {
        var from = byName[src];
        if (!from) return;
        var trig = workflowTrigger(from.type, src);
        ((data.connections[src].main || []).flat()).forEach(function (o) {
          var to = o && byName[o.node];
          if (!to) return;
          var act = workflowAction(to.type, to.name);
          if (trig && act) rules.push({ title: src + ' \u2192 ' + (to.name || o.node), trigger: trig, action: act });
          else unmapped.push(src + ' \u2192 ' + o.node);
        });
      });
    } else if (fmt === 'nodered') {
      var nodes = Array.isArray(data) ? data : (data.flows || []), byId = {};
      nodes.forEach(function (n) { byId[n.id] = n; });
      nodes.forEach(function (n) {
        var trig = workflowTrigger(n.type, n.name);
        if (!trig) return;
        ((n.wires || []).flat()).forEach(function (id) {
          var to = byId[id];
          if (!to) return;
          var act = workflowAction(to.type, to.name), label = (n.name || n.type) + ' \u2192 ' + (to.name || to.type);
          if (act) rules.push({ title: label, trigger: trig, action: act }); else unmapped.push(label);
        });
      });
    } else return { rules: [], unmapped: [], error: 'unknown format' };
    return { rules: rules, unmapped: unmapped };
  }
  // Saves each as a new rule of this board, and says how many - and how many
  // lines it could not map - as WeKan's reportImport.
  function importParsedRules(el, rules, unmapped) {
    var page = document.querySelector('.rules-page');
    var pop = el.closest('.js-pop-over');
    var msg = pop.querySelector('.js-rules-import-message');
    var words = pop.querySelector('[data-done]');
    Promise.all(rules.map(function (r) {
      return callAsync('rules.createRule', page.dataset.board, r.title || 'Imported rule', r.trigger || {}, r.action || {});
    })).then(function () {
      var text = (words.dataset.done || '').replace('__count__', rules.length);
      if (unmapped && unmapped.length) text += ' \u2014 ' + (words.dataset.unmapped || '').replace('__count__', unmapped.length);
      msg.textContent = text;
      up.reload('.rules-list');
    }, function (e) { msg.textContent = e.reason || e.message; });
  }
  up.on('click', '.js-rules-import-csv', function (event, el) {
    event.preventDefault();
    importParsedRules(el, csvToRules(el.closest('.js-pop-over').querySelector('.js-rules-import-text').value), []);
  });
  up.on('click', '.js-rules-import-trello', function (event, el) {
    event.preventDefault();
    var p = parseTrelloButler(el.closest('.js-pop-over').querySelector('.js-rules-import-text').value);
    importParsedRules(el, p.rules, p.unmapped);
  });
  up.on('click', '.js-rules-import-workflow', function (event, el) {
    event.preventDefault();
    var pop = el.closest('.js-pop-over');
    var p = parseWorkflow(pop.querySelector('.js-rules-import-text').value, pop.querySelector('.js-workflow-format').value);
    if (p.error) { pop.querySelector('.js-rules-import-message').textContent = p.error; return; }
    importParsedRules(el, p.rules, p.unmapped);
  });
  window.weddyRulesImport = { csvToRules: csvToRules, parseTrelloButler: parseTrelloButler, parseWorkflow: parseWorkflow }; // for the tests
  // Import: each rule of an exported JSON, saved as a new rule of this board.
  up.on('click', '.js-rules-import-json', function (event, el) {
    event.preventDefault();
    var page = document.querySelector('.rules-page');
    var pop = el.closest('.js-pop-over');
    var msg = pop.querySelector('.js-rules-import-message');
    var parsed;
    try { parsed = JSON.parse(pop.querySelector('.js-rules-import-text').value); } catch (e) { msg.textContent = String(e.message); return; }
    var rules = Array.isArray(parsed) ? parsed : (parsed && parsed.rules) || [];
    Promise.all(rules.map(function (r) {
      return callAsync('rules.createRule', page.dataset.board, r.title || '', r.trigger || {}, r.action || {});
    })).then(function () {
      closePopOvers();
      up.reload('.rules-page');
    }, function (e) { msg.textContent = e.reason || e.message; });
  });
  // WeKan's rulesWorkflow builder: chips dragged into When and Then, Add Rule;
  // an action dropped onto a rule replaces what it does.
  var workflowDrag = null; // { kind: 'trigger' | 'action', doc, label }
  var workflowPick = { trigger: null, action: null };
  function workflowSlot(slot, pick) {
    slot.textContent = '';
    var span = document.createElement('span');
    span.className = pick ? 'workflow-node-text' : 'workflow-slot-placeholder';
    span.textContent = pick ? pick.label : slot.dataset.empty;
    slot.appendChild(span);
    var stage = slot.closest('.rules-workflow');
    var add = stage && stage.querySelector('.js-create-workflow-rule');
    if (add) add.disabled = !(workflowPick.trigger && workflowPick.action);
  }
  document.addEventListener('dragstart', function (event) {
    var chip = event.target.closest && event.target.closest('.js-trigger-chip, .js-action-chip');
    if (!chip) return;
    workflowDrag = { kind: chip.matches('.js-trigger-chip') ? 'trigger' : 'action', doc: JSON.parse(chip.dataset.doc), label: chip.dataset.label };
    if (event.dataTransfer) event.dataTransfer.setData('text/plain', chip.dataset.label);
  });
  document.addEventListener('dragover', function (event) {
    if (event.target.closest && event.target.closest('.js-when-slot, .js-then-slot, .js-rule-node')) event.preventDefault();
  });
  document.addEventListener('drop', function (event) {
    var target = event.target.closest && event.target.closest('.js-when-slot, .js-then-slot, .js-rule-node');
    if (!target || !workflowDrag) return;
    event.preventDefault();
    var drag = workflowDrag;
    workflowDrag = null;
    if (target.matches('.js-when-slot') && drag.kind === 'trigger') { workflowPick.trigger = drag; workflowSlot(target, drag); }
    else if (target.matches('.js-then-slot') && drag.kind === 'action') { workflowPick.action = drag; workflowSlot(target, drag); }
    else if (target.matches('.js-rule-node') && drag.kind === 'action') {
      var err = target.closest('.rules-workflow').querySelector('.js-workflow-error');
      var title = (target.querySelector('.workflow-rule-title') || {}).textContent || '';
      callAsync('rules.updateRule', target.dataset.ruleId, title, JSON.parse(target.dataset.trigger || '{}'), drag.doc).then(function () { up.reload('.rules-page'); }, function (e) { err.textContent = e.reason || e.message; err.hidden = false; });
    }
  });
  up.on('click', '.js-create-workflow-rule', function (event, el) {
    event.preventDefault();
    if (!workflowPick.trigger || !workflowPick.action) return;
    var stage = el.closest('.rules-workflow');
    var titleField = stage.querySelector('.js-workflow-rule-title');
    var title = titleField.value.trim() || (workflowPick.trigger.label + ' \u2192 ' + workflowPick.action.label);
    var err = stage.querySelector('.js-workflow-error');
    el.disabled = true;
    callAsync('rules.createRule', rulesPage(el).dataset.board, title, workflowPick.trigger.doc, workflowPick.action.doc).then(function () {
      workflowPick = { trigger: null, action: null };
      up.reload('.rules-page');
    }, function (e) { err.textContent = e.reason || e.message; err.hidden = false; el.disabled = false; });
  });
  up.on('click', '.js-delete-workflow-rule', function (event, el) {
    event.preventDefault();
    callAsync('rules.deleteRule', el.closest('.js-rule-node').dataset.ruleId).then(function () { up.reload('.rules-page'); });
  });
  // WeKan's Wrike workflow import: lists and rules made on the server.
  up.on('click', '.js-rules-import-wrike', function (event, el) {
    event.preventDefault();
    var page = document.querySelector('.rules-page');
    var pop = el.closest('.js-pop-over');
    var msg = pop.querySelector('.js-rules-import-message');
    callAsync('rules.importWrikeWorkflow', page.dataset.board, pop.querySelector('.js-rules-import-text').value).then(function (res) {
      msg.textContent = (el.dataset.done || '').replace('__name__', res.name).replace('__lists__', res.listsCreated)
        .replace('__rules__', res.rulesCreated).replace('__skipped__', res.notRead);
      up.reload('.rules-list');
    }, function (e) { msg.textContent = e.reason || e.message; });
  });
  up.on('click', '.rules-page .js-goto-details', function (event, el) {
    event.preventDefault();
    showPane(rulesPage(el), 'details-' + el.dataset.ruleId);
  });
  function selectRules(el, on) {
    rulesPage(el).querySelectorAll('.js-rule-select').forEach(function (b) { b.checked = on; });
  }
  up.on('click', '.rules-page .js-rules-select-all', function (event, el) { event.preventDefault(); selectRules(el, true); });
  up.on('click', '.rules-page .js-rules-select-none', function (event, el) { event.preventDefault(); selectRules(el, false); });
  up.on('click', '.rules-page .js-rules-delete-selected', function (event, el) {
    event.preventDefault();
    var ids = Array.prototype.map.call(rulesPage(el).querySelectorAll('.js-rule-select:checked'), function (b) { return b.dataset.ruleId; });
    Promise.all(ids.map(function (id) { return callAsync('rules.deleteRule', id); })).then(function () { up.reload('.rules-page'); });
  });
  // Utils.getTriggerActionDesc: the item's words, and what is chosen in it.
  function ruleDesc(button) {
    var item = button.closest('.trigger-item');
    var parts = [];
    item.querySelectorAll('.trigger-content > *').forEach(function (el) {
      if (el.classList.contains('trigger-text')) {
        parts.push(el.textContent.trim().toLowerCase());
      } else if (el.querySelector('select')) {
        var sel = el.querySelector('select');
        parts.push(sel.options[sel.selectedIndex] ? sel.options[sel.selectedIndex].text.trim().toLowerCase() : '');
      } else if (el.querySelector('textarea, input')) {
        parts.push(el.querySelector('textarea, input').value.trim());
      }
    });
    return parts.filter(function (x) { return x; }).join(' ');
  }
  function val(page, id) { var el = page.querySelector('#' + id); return el ? el.value : ''; }
  up.on('click', '.js-goto-trigger', function (event, el) {
    event.preventDefault();
    var page = rulesPage(el);
    ruleState = { name: val(page, 'ruleTitle').trim(), trigger: null };
    showPane(page, 'trigger');
  });
  // #2713: edit a rule - the same wizard, saving it in place (rules.updateRule).
  up.on('click', '.rules-page .js-edit-rule-full', function (event, el) {
    event.preventDefault();
    ruleState = { name: el.dataset.title || '', trigger: null, editing: el.dataset.ruleId };
    showPane(rulesPage(el), 'trigger');
  });
  up.on('click', '.rules-page .js-goback', function (event, el) {
    event.preventDefault();
    showPane(rulesPage(el), el.dataset.to);
  });
  up.on('click', '.rules-page .triggers-side-menu li', function (event, el) {
    var pane = el.closest('.js-rules-stage');
    pane.querySelectorAll('.triggers-side-menu li').forEach(function (li) { li.classList.toggle('active', li === el); });
    pane.querySelectorAll('.js-rules-tab').forEach(function (t) { t.classList.toggle('is-hidden', t.dataset.tab !== el.dataset.tab); });
  });
  // The triggers, as boardTriggers.js and buttonTriggers.js make them.
  var triggerMakers = {
    'js-add-create-trigger': function (p) {
      return { activityType: 'createCard', cardTitle: '*', listName: val(p, 'create-list-name'), swimlaneName: val(p, 'create-swimlane-name') };
    },
    'js-add-gen-moved-trigger': function () {
      return { activityType: 'moveCard', cardTitle: '*', listName: '*', oldListName: '*', swimlaneName: '*' };
    },
    'js-add-moved-trigger': function (p) {
      var list = val(p, 'move-list-name'), sw = val(p, 'create-swimlane-name-2');
      return val(p, 'move-action') === 'moved-from'
        ? { activityType: 'moveCard', cardTitle: '*', swimlaneName: sw, listName: '*', oldListName: list }
        : { activityType: 'moveCard', cardTitle: '*', swimlaneName: sw, listName: list, oldListName: '*' };
    },
    // #2076: moved forward or back, matched by list order on the server.
    'js-add-move-direction-trigger': function (p) {
      return { activityType: 'moveCardDirection', direction: val(p, 'move-direction') === 'back' ? 'back' : 'forward', cardTitle: '*' };
    },
    'js-add-arch-trigger': function (p) {
      var kind = { archived: 'archivedCard', unarchived: 'restoredCard', commented: 'addComment' }[val(p, 'arch-action')];
      return { activityType: kind, cardTitle: '*' };
    },
    // cardTriggers.js
    'js-add-gen-label-trigger': function (p) { return { activityType: val(p, 'label-action') === 'removed' ? 'removedLabel' : 'addedLabel' }; },
    'js-add-spec-label-trigger': function (p) {
      return { activityType: val(p, 'spec-label-action') === 'removed' ? 'removedLabel' : 'addedLabel', labelId: val(p, 'spec-label') };
    },
    'js-add-gen-member-trigger': function (p) { return { activityType: val(p, 'gen-member-action') === 'removed' ? 'unjoinMember' : 'joinMember' }; },
    'js-add-spec-member-trigger': function (p) {
      return { activityType: val(p, 'spec-member-action') === 'removed' ? 'unjoinMember' : 'joinMember', username: val(p, 'spec-member') };
    },
    'js-add-gen-assignee-trigger': function (p) { return { activityType: val(p, 'gen-assignee-action') === 'removed' ? 'unjoinAssignee' : 'joinAssignee' }; },
    'js-add-spec-assignee-trigger': function (p) {
      return { activityType: val(p, 'spec-assignee-action') === 'removed' ? 'unjoinAssignee' : 'joinAssignee', username: val(p, 'spec-assignee') };
    },
    'js-add-attachment-trigger': function (p) { return { activityType: val(p, 'attach-action') === 'removed' ? 'deleteAttachment' : 'addAttachment' }; },
    // #2194: the card's title or description contains a text; nothing without one.
    'js-add-text-contains-trigger': function (p) {
      var text = val(p, 'text-contains-value').trim();
      return text ? { activityType: 'textContainsTrigger', textContains: text } : null;
    },
    // scheduledTriggers.js
    'js-add-scheduled-trigger': function (p) {
      return { activityType: 'scheduledTrigger', scheduleKind: 'calendar', scheduleType: val(p, 'schedule-type'),
        atTime: val(p, 'schedule-time') || '09:00', weekday: parseInt(val(p, 'schedule-weekday'), 10),
        dayOfMonth: parseInt(val(p, 'schedule-dom'), 10) || 1, onDate: val(p, 'schedule-date') || '',
        listName: val(p, 'schedule-list-name') || '*', swimlaneName: '*' };
    },
    'js-add-due-trigger': function (p) {
      return { activityType: 'scheduledTrigger', scheduleKind: 'due', dueCondition: val(p, 'due-condition'),
        dateField: val(p, 'due-date-field') === 'startAt' ? 'startAt' : 'dueAt', days: parseInt(val(p, 'due-days'), 10) || 0,
        atTime: val(p, 'due-time') || '09:00', listName: '*', swimlaneName: '*' };
    },
    'js-add-aging-trigger': function (p) {
      return { activityType: 'scheduledTrigger', scheduleKind: 'aging', days: parseInt(val(p, 'aging-days'), 10) || 7,
        atTime: val(p, 'aging-time') || '09:00', listName: val(p, 'aging-list-name') || '*', swimlaneName: '*' };
    },
    'js-add-cron-trigger': function (p) {
      var input = p.querySelector('#cron-expression');
      var cron = String(input.value || '').trim().replace(/\s+/g, ' ');
      // Five fields, as WeKan's form asks; the server checks the rest.
      if (cron.split(' ').length !== 5) {
        input.setCustomValidity(input.dataset.invalid || 'invalid'); input.reportValidity();
        return null;
      }
      input.setCustomValidity('');
      return { activityType: 'scheduledTrigger', scheduleKind: 'cron', cron: cron, listName: val(p, 'cron-list-name') || '*', swimlaneName: '*' };
    },
    'js-add-due-date-changed-trigger': function () { return { activityType: 'a-dueAt' }; },
    'js-add-start-date-changed-trigger': function () { return { activityType: 'a-startAt' }; },
    'js-add-end-date-changed-trigger': function () { return { activityType: 'a-endAt' }; },
    'js-add-received-date-changed-trigger': function () { return { activityType: 'a-receivedAt' }; },
    // checklistTriggers.js
    'js-add-gen-check-trigger': function (p) { return { activityType: val(p, 'gen-check-action') === 'removed' ? 'removeChecklist' : 'addChecklist' }; },
    'js-add-spec-check-trigger': function (p) {
      return { activityType: val(p, 'spec-check-action') === 'removed' ? 'removeChecklist' : 'addChecklist', checklistName: val(p, 'check-name') };
    },
    'js-add-gen-comp-trigger': function (p) {
      return { activityType: val(p, 'gen-comp-check-action') === 'uncompleted' ? 'uncompleteChecklist' : 'completeChecklist' };
    },
    'js-add-spec-comp-trigger': function (p) {
      return { activityType: val(p, 'spec-comp-check-action') === 'uncompleted' ? 'uncompleteChecklist' : 'completeChecklist',
        checklistName: val(p, 'spec-comp-check-name') };
    },
    'js-add-gen-check-item-trigger': function (p) { return { activityType: val(p, 'check-item-gen-action') === 'unchecked' ? 'uncheckedItem' : 'checkedItem' }; },
    'js-add-spec-check-item-trigger': function (p) {
      return { activityType: val(p, 'check-item-spec-action') === 'unchecked' ? 'uncheckedItem' : 'checkedItem', checklistItemName: val(p, 'check-item-name') };
    },
    'js-add-button-trigger': function (p) {
      return { activityType: 'button', buttonType: val(p, 'button-type') || 'card', buttonLabel: val(p, 'button-label') || 'Run' };
    },
  };
  // The actions, as boardActions.js and cardActions.js make them.
  var actionMakers = {
    'js-add-gen-move-action': function (p) {
      return { actionType: val(p, 'move-gen-action') === 'bottom' ? 'moveCardToBottom' : 'moveCardToTop', listName: '*', swimlaneName: '*' };
    },
    'js-add-spec-move-action': function (p) {
      return { actionType: val(p, 'move-spec-action') === 'bottom' ? 'moveCardToBottom' : 'moveCardToTop',
        listName: val(p, 'listName'), swimlaneName: val(p, 'swimlaneName') || '*', boardId: val(p, 'board-id') };
    },
    'js-add-arch-action': function (p) { return { actionType: val(p, 'arch-action') }; },
    'js-add-swimlane-action': function (p) { return { actionType: 'addSwimlane', swimlaneName: val(p, 'swimlane-name') }; },
    'js-create-card-action': function (p) {
      return { actionType: 'createCard', cardName: val(p, 'card-name'), listName: val(p, 'list-name'), swimlaneName: val(p, 'swimlane-name2') };
    },
    'js-set-date-action': function (p) { return { actionType: val(p, 'setdate-action'), dateField: val(p, 'setdate-datefield') }; },
    'js-remove-datevalue-action': function (p) { return { actionType: 'removeDate', dateField: val(p, 'setdate-removedatefieldvalue') }; },
    'js-add-label-action': function (p) {
      return { actionType: val(p, 'label-action') === 'remove' ? 'removeLabel' : 'addLabel', labelId: val(p, 'label-id') };
    },
    'js-add-member-action': function (p) {
      return { actionType: val(p, 'member-action') === 'remove' ? 'removeMember' : 'addMember', username: val(p, 'member-name') };
    },
    'js-add-actinguser-member-action': function () { return { actionType: 'addMember', username: '_actinguser_' }; },
    // checklistActions.js
    'js-add-checklist-action': function (p) {
      return { actionType: val(p, 'check-action') === 'remove' ? 'removeChecklist' : 'addChecklist', checklistName: val(p, 'checklist-name') };
    },
    'js-add-checkall-action': function (p) {
      return { actionType: val(p, 'checkall-action') === 'uncheck' ? 'uncheckAll' : 'checkAll', checklistName: val(p, 'checklist-name2') };
    },
    'js-add-check-item-action': function (p) {
      return { actionType: val(p, 'check-item-action') === 'uncheck' ? 'uncheckItem' : 'checkItem',
        checkItemName: val(p, 'checkitem-name'), checklistName: val(p, 'checklist-name3') };
    },
    // WeKan's copyCard: a list and swimlane of the chosen board, or nothing.
    'js-copy-card-action': function (p) {
      var board = val(p, 'board-id-copy'), list = val(p, 'list-id-copy'), lane = val(p, 'swimlane-id-copy');
      return board && list && lane ? { actionType: 'copyCard', boardId: board, listId: list, swimlaneId: lane } : null;
    },
    // WeKan's mailActions; nothing without a recipient.
    'js-mail-action': function (p) {
      var to = val(p, 'email-to').trim();
      var discussion = p.querySelector('#email-discussion');
      var details = p.querySelector('#email-card-details');
      var files = p.querySelector('#email-attachments');
      return to ? { actionType: 'sendEmail', emailTo: to, emailSubject: val(p, 'email-subject'), emailMsg: val(p, 'email-msg'),
        includeCardDetails: !!(details && details.checked), includeChecklistsAndComments: !!(discussion && discussion.checked),
        includeAttachments: !!(files && files.checked) } : null;
    },
    'js-link-card-action': function (p) {
      return { actionType: 'linkCard', boardId: val(p, 'board-id-link'), listName: val(p, 'listName-link') || '*', swimlaneName: val(p, 'swimlaneName-link') || '*' };
    },
    'js-sort-list-action': function (p) {
      return { actionType: 'sortList', listName: val(p, 'sort-list-name') || '*', sortField: val(p, 'sort-field') };
    },
    'js-set-reldate-action': function (p) {
      return { actionType: 'setDateRelative', dateField: val(p, 'reldate-datefield'),
        days: parseInt(val(p, 'reldate-days'), 10) || 0, unit: val(p, 'reldate-unit') };
    },
    'js-add-checklist-items-action': function (p) {
      return { actionType: 'addChecklistWithItems', checklistName: val(p, 'checklist-name-3'), checklistItems: val(p, 'checklist-items') };
    },
  };
  function makerOf(el, makers) {
    for (var cls in makers) if (el.classList.contains(cls)) return makers[cls];
    return null;
  }
  up.on('click', '.rules-page .js-goto-action', function (event, el) {
    event.preventDefault();
    var make = makerOf(el, triggerMakers);
    if (!make) return;
    var page = rulesPage(el);
    var trigger = make(el.closest('.js-rules-tab'));
    if (!trigger) return; // a trigger that needs a value, without one
    trigger.desc = ruleDesc(el);
    ruleState.trigger = trigger;
    showPane(page, 'action');
  });
  up.on('click', '.rules-page .js-goto-rules', function (event, el) {
    event.preventDefault();
    var make = makerOf(el, actionMakers);
    if (!make || !ruleState.trigger) return;
    var page = rulesPage(el);
    var action = make(el.closest('.js-rules-tab'));
    if (!action) return; // an action that needs a value, without one
    action.desc = ruleDesc(el);
    var saved = ruleState.editing
      ? callAsync('rules.updateRule', ruleState.editing, ruleState.name, ruleState.trigger, action)
      : callAsync('rules.createRule', page.dataset.board, ruleState.name, ruleState.trigger, action);
    saved.then(function () {
      up.reload('.rules-page');
    }, function (e) { window.alert(e.reason || e.message); });
  });
  up.on('click', '.rules-page .js-delete-rule', function (event, el) {
    event.preventDefault();
    callAsync('rules.deleteRule', el.dataset.ruleId).then(function () { up.reload('.rules-page'); });
  });
  up.on('click', '.rules-page .js-toggle-rule-enabled', function (event, el) {
    event.preventDefault();
    var on = !!el.querySelector('.fa-toggle-off');
    callAsync('rules.setEnabled', el.dataset.ruleId, on).then(function () { up.reload('.rules-page'); });
  });

  // The Copy Card action offers the lists and swimlanes of the board chosen.
  function copyTargets(select) {
    var tab = select.closest('.js-rules-tab');
    if (!tab) return;
    ['list-id-copy', 'swimlane-id-copy'].forEach(function (id) {
      var target = tab.querySelector('#' + id);
      if (!target) return;
      var first = null;
      Array.prototype.forEach.call(target.options, function (o) {
        o.hidden = o.dataset.board !== select.value;
        if (!o.hidden && !first) first = o;
      });
      if (!target.selectedOptions[0] || target.selectedOptions[0].hidden) target.value = first ? first.value : '';
    });
  }
  up.compiler('#board-id-copy', function (select) { copyTargets(select); });
  document.addEventListener('change', function (event) {
    if (event.target.matches && event.target.matches('#board-id-copy')) copyTargets(event.target);
  });
  // #3195: the rule editor's "Insert variable" picker, as WeKan's
  // ruleVariablePicker: the token goes in at the caret of the text field last
  // focused in that editor, the caret after it, the field focused again.
  var RULE_TEXT_FIELD = 'input[type="text"], input:not([type]), textarea';
  document.addEventListener('focusin', function (event) {
    var body = event.target.closest && event.target.closest('.triggers-main-body');
    if (body && event.target.matches(RULE_TEXT_FIELD)) body.weddyLastField = event.target;
  });
  document.addEventListener('change', function (event) {
    var select = event.target;
    if (!select.matches || !select.matches('.js-rule-variable-picker')) return;
    var token = select.value;
    select.value = '';
    var body = select.closest('.triggers-main-body');
    if (!token || !body) return;
    var field = body.weddyLastField && body.contains(body.weddyLastField) && body.weddyLastField.offsetParent !== null ? body.weddyLastField : null;
    if (!field) {
      field = Array.prototype.find.call(body.querySelectorAll(RULE_TEXT_FIELD), function (f) { return f.offsetParent !== null; });
    }
    if (!field) return;
    var text = field.value || '', start = field.selectionStart, end = field.selectionEnd;
    var valid = typeof start === 'number' && typeof end === 'number' && start >= 0 && start <= end && end <= text.length;
    var from = valid ? start : text.length, to = valid ? end : text.length;
    field.value = text.slice(0, from) + token + text.slice(to);
    field.dispatchEvent(new Event('input', { bubbles: true }));
    field.focus();
    field.setSelectionRange(from + token.length, from + token.length);
  });

  // Admin Panel / Problems / Recovery: WeKan's emailRecoveryReports - one row a
  // recipient, paused, resumed, cancelled and retried. A request keeps its id
  // until it is confirmed, so trying it again never repeats an old cancel
  // against mail queued later.
  up.compiler('.email-recovery-reports', function (panel) {
    var strings = {};
    var tpl = panel.querySelector('template.js-recovery-strings');
    if (tpl) tpl.content.querySelectorAll('[data-k]').forEach(function (el) { strings[el.dataset.k] = el.textContent; });
    var say = function (key) { return strings[key] || key; };
    var state = { search: '', page: 1, total: 0, rows: [], busy: false, request: 0 };
    var commands = {};
    var errorEl = panel.querySelector('.js-recovery-error'), busyEl = panel.querySelector('.js-recovery-busy');
    var body = panel.querySelector('tbody'), empty = panel.querySelector('.table-page-empty');
    function message(text) { errorEl.textContent = text || ''; errorEl.hidden = !text; }
    function setBusy(on) { state.busy = on; busyEl.hidden = !on; render(); }
    function para(text, cls) {
      var p = document.createElement('p');
      if (cls) p.className = cls;
      p.textContent = text;
      return p;
    }
    function when(v) { return v ? new Date(v).toLocaleString() : '—'; }
    function render() {
      body.textContent = '';
      state.rows.forEach(function (row) {
        var tr = document.createElement('tr');
        tr.dataset.recipient = row.userId;
        var cells = [[para(row.username), para(row.userId, 'quiet')],
          [document.createTextNode(say(row.paused ? 'email-recovery-paused' : row.failed ? 'email-recovery-attention' : 'email-recovery-pending'))],
          [para(say('email-recovery-queued') + ': ' + row.queued), para(say('email-recovery-retrying') + ': ' + row.retrying),
            para(say('email-recovery-stopped') + ': ' + row.failed)]
            .concat((row.failures || []).map(function (f) { return para(say('email-failure-' + f.reason) + ': ' + f.count, 'quiet'); }))
            .concat([para(say('email-recovery-attempts') + ': ' + row.attempts)]),
          [para(say('email-recovery-oldest') + ': ' + when(row.oldest)), para(say('email-recovery-next') + ': ' + (!row.paused ? when(row.nextAttemptAt) : '—'))]
            .concat(row.changedAt ? [para(say('email-recovery-changed') + ': ' + when(row.changedAt)), para(row.changedBy || '', 'quiet')] : []),
          []];
        var actions = [[row.paused ? 'resume' : 'pause', true], ['retry', row.failed > 0], ['cancel', row.queued > 0]];
        actions.forEach(function (a) {
          if (!a[1]) return;
          var b = document.createElement('button');
          b.type = 'button'; b.className = 'js-email-queue-action'; b.dataset.action = a[0]; b.disabled = state.busy;
          b.textContent = say('email-recovery-' + a[0]); b.weddyRow = row;
          cells[4].push(b);
        });
        cells.forEach(function (children) {
          var td = document.createElement('td');
          children.forEach(function (c) { td.appendChild(c); });
          tr.appendChild(td);
        });
        body.appendChild(tr);
      });
      empty.hidden = state.rows.length > 0;
      var pages = Math.max(1, Math.ceil(state.total / 10));
      panel.querySelector('.table-page-page-info').textContent = state.page + ' / ' + pages;
      panel.querySelector('.table-page-total').textContent = state.total ? String(state.total) : '';
      panel.querySelector('.js-table-page-prev').classList.toggle('disabled', state.page <= 1);
      panel.querySelector('.js-table-page-next').classList.toggle('disabled', state.page >= pages);
    }
    function load(page) {
      var request = ++state.request;
      state.rows = []; state.page = page || 1;
      render();
      callAsync('emailRecoveryReport', { search: state.search, page: state.page }).then(function (result) {
        if (request !== state.request) return;
        state.rows = result.rows || []; state.total = result.total || 0; state.page = result.page || 1;
        render();
      }, function () { if (request === state.request) message(say('email-recovery-unavailable')); });
    }
    function random() {
      var a = new Uint8Array(24); crypto.getRandomValues(a);
      return Array.prototype.map.call(a, function (b) { return 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'[b % 62]; }).join('');
    }
    function onSearchKey(event) {
      event.stopPropagation();
      if (event.key !== 'Enter') return;
      event.preventDefault();
      state.search = event.target.value.trim().slice(0, 100);
      load(1);
    }
    function onClick(event) {
      var el = event.target.closest('button');
      if (!el || !panel.contains(el)) return;
      if (el.matches('.js-table-page-prev, .js-table-page-next')) {
        event.preventDefault();
        var pages = Math.max(1, Math.ceil(state.total / 10));
        var next = state.page + (el.matches('.js-table-page-next') ? 1 : -1);
        if (next >= 1 && next <= pages && next !== state.page) load(next);
        return;
      }
      if (el.matches('.js-table-page-action')) { event.preventDefault(); message(''); load(state.page); return; }
      if (!el.matches('.js-email-queue-action') || state.busy) return;
      event.preventDefault();
      var action = el.dataset.action, userId = el.weddyRow.userId;
      if (action === 'cancel' && !window.confirm(say('email-recovery-confirm-cancel'))) return;
      var key = userId + ':' + action;
      if (!commands[key]) commands[key] = random();
      setBusy(true); message('');
      callAsync('controlEmailRecovery', { userId: userId, action: action, requestId: commands[key] }).then(function (result) {
        setBusy(false);
        delete commands[key];
        if (result && result.status === 'superseded') message(say('email-recovery-superseded'));
        load(state.page);
      }, function (error) {
        setBusy(false);
        message(say(error && error.error === 'email-recovery-busy' ? 'email-recovery-busy' : 'email-recovery-failed'));
        load(state.page);
      });
    }
    var search = panel.querySelector('.js-table-page-search');
    search.addEventListener('keydown', onSearchKey);
    panel.addEventListener('click', onClick);
    load(1);
    return function () { search.removeEventListener('keydown', onSearchKey); panel.removeEventListener('click', onClick); };
  });

  // Admin Panel / Problems / Recovery: WeKan's activityNotificationRecoveryReports.
  // The table is the report of activityNotificationRecoveryReport, ten a page;
  // each row's buttons pause, resume, cancel and retry with the revision the
  // administrator saw, never a newer one.
  up.compiler('.activity-notification-recovery-reports', function (panel) {
    var strings = {};
    var tpl = panel.querySelector('template.js-recovery-strings');
    if (tpl) tpl.content.querySelectorAll('[data-k]').forEach(function (el) { strings[el.dataset.k] = el.textContent; });
    var say = function (key) {
      var args = Array.prototype.slice.call(arguments, 1), i = 0;
      return (strings[key] || key).replace(/%s/g, function () { return String(args[i++]); });
    };
    var state = { search: '', page: 1, total: 0, rows: [], busy: false, request: 0 };
    var errorEl = panel.querySelector('.js-recovery-error'), busyEl = panel.querySelector('.js-recovery-busy');
    var body = panel.querySelector('tbody'), empty = panel.querySelector('.table-page-empty');
    function message(text) { errorEl.textContent = text || ''; errorEl.hidden = !text; }
    function setBusy(on) { state.busy = on; busyEl.hidden = !on; render(); }
    function cell(tr, children) {
      var td = document.createElement('td');
      children.forEach(function (c) { if (c) td.appendChild(c); });
      tr.appendChild(td);
      return td;
    }
    function para(text, cls) {
      var p = document.createElement('p');
      if (cls) p.className = cls;
      p.textContent = text;
      return p;
    }
    function button(cls, label, disabled, row) {
      var b = document.createElement('button');
      b.type = 'button'; b.className = cls; b.textContent = label; b.disabled = disabled;
      b.weddyRow = row;
      return b;
    }
    function render() {
      body.textContent = '';
      state.rows.forEach(function (row) {
        var tr = document.createElement('tr');
        tr.dataset.intent = row.intentId;
        cell(tr, [para(row.activityId), row.cardId ? para(say('card') + ': ' + row.cardId, 'quiet') : null]);
        cell(tr, [document.createTextNode(row.boardId)]);
        cell(tr, [para(say('activity-recovery-status-' + row.status)),
          row.paused && row.status !== 'cancelled' ? para(say('activity-recovery-paused')) : null]);
        cell(tr, [document.createTextNode(row.createdAt ? new Date(row.createdAt).toLocaleString() : '—')]);
        var noControl = state.busy || !row.canControl;
        cell(tr, [button('js-cancel-activity-notification', say('activity-recovery-cancel'), noControl, row),
          button('js-control-activity-notification', say(row.paused ? 'activity-recovery-resume' : 'activity-recovery-pause'), noControl, row),
          button('js-retry-activity-notification', say('activity-recovery-retry'), state.busy || !row.canRetry, row)]);
        body.appendChild(tr);
      });
      empty.hidden = state.rows.length > 0;
      var pages = Math.max(1, Math.ceil(state.total / 10));
      panel.querySelector('.table-page-page-info').textContent = state.page + ' / ' + pages;
      panel.querySelector('.table-page-total').textContent = state.total ? String(state.total) : '';
      panel.querySelector('.js-table-page-prev').classList.toggle('disabled', state.page <= 1);
      panel.querySelector('.js-table-page-next').classList.toggle('disabled', state.page >= pages);
    }
    // WeKan's report is reactive: a row that recovery is delivering right now
    // ("processing", its lease held) turns back when the lease is released.
    // This one asks again, a second later, while a row is processing - else
    // its buttons stayed disabled until the page was reloaded.
    var again = null;
    function load(page, keepRows) {
      var request = ++state.request;
      clearTimeout(again); again = null;
      state.page = page || 1;
      if (!keepRows) { state.rows = []; render(); }
      callAsync('activityNotificationRecoveryReport', { search: state.search, page: state.page }).then(function (result) {
        if (request !== state.request) return;
        state.rows = result.rows || []; state.total = result.total || 0; state.page = result.page || 1;
        render();
        if (state.rows.some(function (row) { return row.status === 'processing'; }) && panel.isConnected) {
          again = setTimeout(function () { if (!state.busy) load(state.page, true); }, 1000);
        }
      }, function () {
        if (request !== state.request) return;
        message(say('activity-recovery-unavailable'));
      });
    }
    function known(error, codes, fallback) {
      return say(error && codes.indexOf(error.error) >= 0 ? error.error : fallback);
    }
    function onSearchKey(event) {
      event.stopPropagation();
      if (event.key !== 'Enter') return;
      event.preventDefault();
      state.search = event.target.value.trim().slice(0, 100);
      load(1);
    }
    function onClick(event) {
      var el = event.target.closest('button');
      if (!el || !panel.contains(el)) return;
      var row = el.weddyRow;
      if (el.matches('.js-table-page-prev, .js-table-page-next')) {
        event.preventDefault();
        var pages = Math.max(1, Math.ceil(state.total / 10));
        var next = state.page + (el.matches('.js-table-page-next') ? 1 : -1);
        if (next >= 1 && next <= pages && next !== state.page) load(next);
        return;
      }
      if (el.matches('.js-table-page-action')) {
        event.preventDefault(); message('');
        var done = function (result) {
          setBusy(false);
          message(say('activity-recovery-bulk-result', result.cancelled || 0, result.completed || 0, result.remaining || 0));
          load(state.page);
        };
        var failed = function () { setBusy(false); message(say('activity-recovery-control-failed')); load(state.page); };
        if (el.dataset.action === 'retry-all-activity-notifications') {
          setBusy(true);
          callAsync('retryAllActivityNotifications').then(done, failed);
        } else if (el.dataset.action === 'cancel-old-activity-notifications') {
          var answer = window.prompt(say('activity-recovery-cancel-older-prompt'), '7');
          var days = Number(answer);
          if (answer === null || !isFinite(days) || days < 0) return;
          setBusy(true);
          callAsync('cancelActivityNotificationsOlderThan', { days: days }).then(done, failed);
        } else load(state.page);
        return;
      }
      if (!row || state.busy) return;
      event.preventDefault();
      var controlCodes = ['activity-recovery-busy', 'activity-recovery-control-conflict'];
      var after = function (codes, fallback) {
        return [function () { setBusy(false); load(state.page); },
          function (error) { setBusy(false); message(known(error, codes, fallback)); load(state.page); }];
      };
      var random = function () {
        var a = new Uint8Array(24); crypto.getRandomValues(a);
        return Array.prototype.map.call(a, function (b) { return 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'[b % 62]; }).join('');
      };
      var then;
      if (el.matches('.js-cancel-activity-notification')) {
        if (!row.canControl || !window.confirm(say('activity-recovery-cancel-confirm'))) return;
        message(''); setBusy(true); then = after(controlCodes, 'activity-recovery-control-failed');
        callAsync('cancelActivityNotificationRecovery', { intentId: row.intentId, expectedRevision: row.controlRevision, requestId: random() })
          .then(then[0], then[1]);
      } else if (el.matches('.js-control-activity-notification')) {
        if (!row.canControl) return;
        message(''); setBusy(true); then = after(controlCodes, 'activity-recovery-control-failed');
        callAsync('controlActivityNotificationRecovery', { intentId: row.intentId, paused: !row.paused,
          expectedRevision: row.controlRevision, requestId: random() }).then(then[0], then[1]);
      } else if (el.matches('.js-retry-activity-notification')) {
        if (!row.canRetry) return;
        message(''); setBusy(true);
        then = after(['activity-recovery-busy', 'activity-recovery-denied', 'activity-recovery-source-unavailable',
          'activity-recovery-disabled', 'activity-recovery-paused', 'activity-recovery-status-cancelled'], 'activity-recovery-failed');
        callAsync('retryActivityNotification', { intentId: row.intentId }).then(then[0], then[1]);
      }
    }
    var search = panel.querySelector('.js-table-page-search');
    search.addEventListener('keydown', onSearchKey);
    panel.addEventListener('click', onClick);
    load(1);
    return function () { clearTimeout(again); search.removeEventListener('keydown', onSearchKey); panel.removeEventListener('click', onClick); };
  });

  // Ctrl+Z and Ctrl+Shift+Z (Cmd on a Mac) on a board: undo and redo one's
  // own last change (changeHistory.undoLast / redoLast), as WeKan's
  // keyboard shortcuts - never while typing in a field.
  document.addEventListener('keydown', function (e) {
    if (!(e.ctrlKey || e.metaKey) || e.altKey || (e.key || '').toLowerCase() !== 'z') return;
    var t = e.target;
    if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) return;
    var wrapper = document.querySelector('.board-wrapper[data-board]');
    if (!wrapper) return;
    e.preventDefault();
    historyKeystroke(wrapper.getAttribute('data-board'), e.shiftKey ? 'redo' : 'undo')
      .then(function () { up.reload('.board-wrapper'); }, function () {});
  });

  // WeKan's client/lib/historyKeyRequest.js: each keystroke's request id,
  // kept in sessionStorage until the server answers, so a lost reply's
  // retry repeats that request instead of undoing one more change. A row
  // the keyed path does not cover falls back to the ordinary call.
  var HISTORY_KEY = 'wekan-history-key-request', HISTORY_KEY_AGE = 10 * 60 * 1000;
  function newHistoryRequestId() {
    var a = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789', id = 'key-';
    for (var i = 0; i < 28; i++) id += a[Math.floor(Math.random() * a.length) % a.length];
    return id;
  }
  function storedHistoryRequest(now) {
    try {
      var v = JSON.parse(sessionStorage.getItem(HISTORY_KEY) || 'null');
      return v && typeof v === 'object' && isFinite(v.at) && v.at <= now && now - v.at <= HISTORY_KEY_AGE && typeof v.boardId === 'string' && v.boardId &&
        (v.direction === 'undo' || v.direction === 'redo') && /^[A-Za-z0-9_-]{16,128}$/.test(v.requestId || '') ? v : null;
    } catch (err) { return null; }
  }
  function storeHistoryRequest(v) {
    try { if (v) sessionStorage.setItem(HISTORY_KEY, JSON.stringify(v)); else sessionStorage.removeItem(HISTORY_KEY); } catch (err) { /* not retried after a reload */ }
  }
  // The keystroke's own call: the reply settles its request at once, with
  // none of callAsync's refresh of the user after it.
  function historyCall(name) {
    var args = Array.prototype.slice.call(arguments, 1);
    return fetch('/_methods/' + encodeURIComponent(name), {
      method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(args),
    }).then(function (r) {
      return r.json().then(function (d) {
        if (!r.ok) { var e = new Error(d.reason || d.error); e.error = d.error; e.reason = d.reason; throw e; }
        return d.result;
      });
    });
  }
  function historyKeystroke(boardId, direction) {
    var now = Date.now(), pending = storedHistoryRequest(now);
    if (pending && pending.boardId !== boardId) pending = null;
    var fresh = { boardId: boardId, direction: direction, requestId: newHistoryRequestId(), at: now };
    var calls = !pending ? [fresh] : pending.direction === direction ? [pending] : [pending, fresh];
    var run = function (i) {
      if (i >= calls.length) return Promise.resolve('settled');
      var req = calls[i], method = req.direction === 'undo' ? 'changeHistory.undoLast' : 'changeHistory.redoLast';
      storeHistoryRequest(req);
      return historyCall(method, req.boardId, req.requestId).then(function () { return 'done'; }, function (err) {
        var code = err && err.error;
        if (code === 'scrum-history-request-unsupported') return 'unsupported';
        if (code === undefined || code === null || code === 'too-many-requests' || code === 503 || code === 'timeout') return 'retry';
        return 'done';
      }).then(function (outcome) {
        if (outcome === 'retry') return 'pending';
        storeHistoryRequest(null);
        var next = function () { return run(i + 1); };
        if (outcome === 'unsupported') return historyCall(method, req.boardId).then(next, next);
        return next();
      });
    };
    return run(0);
  }

  window.weddyMultiDrag = { sorts: sorts }; // for the tests

  // WeKan's Board View button (boardChangeViewPopup): the views this board has.
  up.on('click', '.js-toggle-board-view', function (event) {
    event.preventDefault();
    openPopOver('board-view');
  });

  // WeKan's Scrum views (scrumView.js), drawn by the server (scrum_view.go):
  // what the view has chosen is in its address, and every change is the
  // method WeKan's view calls, then the view drawn again - or the error.
  function scrumView() { return document.querySelector('.scrum-view'); }
  function scrumLink(changes) {
    var url = new URL(scrumView().dataset.link, location.href);
    Object.keys(changes).forEach(function (k) {
      if (changes[k]) url.searchParams.set(k, changes[k]); else url.searchParams.delete(k);
    });
    return url.pathname + url.search;
  }
  function scrumGo(url) { return up.navigate({ url: url, target: '.scrum-view', history: true, cache: false }); }
  function scrumError(message) {
    var p = document.querySelector('.scrum-view .js-scrum-error');
    if (!p) return;
    p.textContent = message || '';
    p.hidden = !message;
  }
  var scrumBusy = false;
  function scrumMutate(method) {
    if (scrumBusy) return Promise.resolve(false);
    var v = scrumView();
    var args = [method, v.dataset.board].concat(Array.prototype.slice.call(arguments, 1));
    scrumBusy = true;
    scrumError('');
    return callAsync.apply(null, args).then(function (result) {
      scrumBusy = false;
      return up.reload('.scrum-view', { cache: false }).then(function () { return result || true; });
    }, function (e) {
      scrumBusy = false;
      scrumError(e.reason || e.message);
      return false;
    });
  }
  function scrumFields(form) {
    var out = {};
    new FormData(form).forEach(function (v, k) { if (!(k in out)) out[k] = v; });
    return out;
  }
  function nullable(v) { return v ? v : null; }
  function scrumRevision(el) { return el.dataset.revision === '' || el.dataset.revision === undefined ? null : Number(el.dataset.revision); }
  // A date and time as the field shows it, in the browser's time zone - and
  // back, the stored instant kept when the field was not changed.
  function localDateTime(iso) {
    if (!iso) return '';
    var d = new Date(iso);
    return new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 23).replace(/:00\.000$/, '').replace(/\.000$/, '');
  }
  function scrumTimestamp(value, original) {
    if (!value) return null;
    return value === localDateTime(original) ? original : new Date(value).toISOString();
  }
  up.compiler('.js-scrum-local-time', function (el) { el.value = localDateTime(el.dataset.iso); });
  up.on('click', '.js-scrum-refresh', function (event) { event.preventDefault(); up.reload('.scrum-view', { cache: false }); });
  up.on('change', '.js-scrum-sprint', function (event, el) { scrumGo(scrumLink({ sprint: el.value, event: '', limit: '' })); });
  up.on('change', '.js-scrum-release-select', function (event, el) { scrumGo(scrumLink({ release: el.value })); });
  up.on('change', '.js-scrum-event-select', function (event, el) { scrumGo(scrumLink({ event: el.value })); });
  up.on('click', '.js-scrum-new', function (event) { event.preventDefault(); scrumGo(scrumLink({ sprint: '', event: '' })); });
  up.on('change', '.js-scrum-chart-metric', function (event, el) {
    var metric = el.value === 'estimate' ? 'estimate' : 'count';
    var root = el.closest('.scrum-view') || document;
    root.querySelectorAll('.scrum-chart-series').forEach(function (sr) {
      var label = sr.querySelector('.scrum-chart-label');
      if (label) label.textContent = label.dataset.label + ': ' + sr.dataset[metric];
      var bar = sr.querySelector('.scrum-chart-bar');
      if (bar) bar.style.width = sr.dataset[metric === 'estimate' ? 'widthEstimate' : 'widthCount'] + '%';
    });
  });
  up.on('submit', '.js-scrum-settings', function (event, form) {
    event.preventDefault();
    var v = scrumFields(form), fd = new FormData(form);
    scrumMutate('scrum.configure', { enabled: v.enabled === 'on', productGoal: v.productGoal, definitionOfDone: v.definitionOfDone,
      estimateSource: v.estimateSource, estimateCustomFieldId: nullable(v.estimateCustomFieldId), estimateUnit: v.estimateUnit,
      completionPolicy: v.completionPolicy, productOwnerId: nullable(v.productOwnerId), scrumMasterId: nullable(v.scrumMasterId),
      developerIds: fd.getAll('developerIds'), workingDays: fd.getAll('workingDays').map(Number) },
    Number(scrumView().dataset.settingsRevision));
  });
  up.on('submit', '.js-scrum-sprint-form', function (event, form) {
    event.preventDefault();
    var v = scrumFields(form);
    scrumMutate('scrum.saveSprint', nullable(form.dataset.sprint), { name: v.name, goal: v.goal, plannedStart: nullable(v.plannedStart),
      plannedEnd: nullable(v.plannedEnd), capacity: v.capacity === '' ? null : Number(v.capacity), capacityUnit: v.capacityUnit }, scrumRevision(form))
      .then(function (saved) { if (saved && saved._id && !form.dataset.sprint) scrumGo(scrumLink({ sprint: saved._id })); });
  });
  up.on('click', '.js-scrum-start', function (event, el) {
    event.preventDefault();
    scrumMutate('scrum.startSprint', scrumView().dataset.sprint, Number(el.dataset.revision));
  });
  up.on('click', '.js-scrum-close', function (event, el) {
    event.preventDefault();
    if (!confirm(scrumView().dataset.confirmClose)) return;
    var rollover = document.querySelector('.scrum-view .js-scrum-rollover');
    scrumMutate('scrum.closeSprint', scrumView().dataset.sprint, Number(el.dataset.revision), nullable(rollover && rollover.value));
  });
  up.on('click', '.js-scrum-resume-close', function (event, el) {
    event.preventDefault();
    scrumMutate('scrum.closeSprint', scrumView().dataset.sprint, Number(el.dataset.revision), nullable(el.dataset.rollover));
  });
  up.on('click', '.js-scrum-cancel', function (event, el) {
    event.preventDefault();
    if (!confirm(scrumView().dataset.confirmCancel)) return;
    var reason = document.querySelector('.scrum-view .js-scrum-cancel-reason');
    scrumMutate('scrum.cancelSprint', scrumView().dataset.sprint, Number(el.dataset.revision), reason ? reason.value : '');
  });
  up.on('submit', '.js-scrum-card', function (event, form) {
    event.preventDefault();
    var v = scrumFields(form);
    scrumMutate('scrum.updateCard', form.dataset.cardId, { sprintId: nullable(v.sprintId),
      releaseIds: new FormData(form).getAll('releaseIds').filter(Boolean),
      backlogRank: v.backlogRank === '' ? null : Number(v.backlogRank), issueType: v.issueType, acceptanceCriteria: v.acceptanceCriteria },
    Number(form.dataset.revision || 0));
  });
  up.on('submit', '.js-scrum-release', function (event, form) {
    event.preventDefault();
    var v = scrumFields(form);
    var at = form.querySelector('[name="releasedAt"]');
    scrumMutate('scrum.saveRelease', nullable(form.dataset.release), { name: v.name, goal: v.goal, plannedStart: nullable(v.plannedStart),
      plannedEnd: nullable(v.plannedEnd), state: v.state, notes: v.notes, releasedAt: scrumTimestamp(v.releasedAt, at && at.dataset.iso) }, scrumRevision(form));
  });
  up.on('submit', '.js-scrum-event', function (event, form) {
    event.preventDefault();
    var v = scrumFields(form);
    var at = form.querySelector('[name="startsAt"]');
    scrumMutate('scrum.saveEvent', nullable(form.dataset.event), { name: v.name, kind: v.kind, startsAt: scrumTimestamp(v.startsAt, at && at.dataset.iso),
      timeboxMinutes: Number(v.timeboxMinutes), notes: v.notes, sprintId: scrumView().dataset.sprint,
      followUpCardIds: new FormData(form).getAll('followUpCardIds') }, scrumRevision(form));
  });

  // WeKan's scrumDailyHistory: a sprint's daily observations, asked for here
  // (scrum.getDailyHistory) and drawn - by cards or by estimate, each unit and
  // partiality on its own scale, missing days left out. An answer that comes
  // after the view moved on to another sprint is dropped.
  function esc(text) { var d = document.createElement('div'); d.textContent = text == null ? '' : String(text); return d.innerHTML; }
  function dailyTotal(sec, t) {
    return sec.dataset.total.replace('__count__', t.count).replace('__estimate__', t.estimate).replace('__unknown__', t.unknown);
  }
  function drawDaily(sec, history, metric) {
    var body = sec.querySelector('.js-scrum-daily-body');
    var d = sec.dataset, html = '';
    if (history.partial) html += '<p class="scrum-partial-report">' + esc(d.partial) + '</p>';
    if (history.truncated) html += '<p class="warning">' + esc(d.truncated) + '</p>';
    var rows = history.rows || [];
    if (!rows.length) { body.innerHTML = html + '<p>' + esc(d.empty) + '</p>'; return; }
    html += '<label>' + esc(d.cards) + ' / ' + esc(d.estimate) + '<select class="js-scrum-daily-metric">' +
      '<option value="count"' + (metric === 'count' ? ' selected' : '') + '>' + esc(d.cards) + '</option>' +
      '<option value="estimate"' + (metric === 'estimate' ? ' selected' : '') + '>' + esc(d.estimate) + '</option></select></label>';
    var groups = [], bySig = {};
    rows.forEach(function (row) {
      var sig = JSON.stringify([row.unit, !!row.partial]);
      if (!bySig[sig]) { bySig[sig] = { unit: row.unit, partial: !!row.partial, rows: [], max: 0 }; groups.push(bySig[sig]); }
      var g = bySig[sig];
      g.rows.push(row);
      ['scope', 'remaining', 'completed'].forEach(function (k) { g.max = Math.max(g.max, row[k][metric]); });
    });
    groups.forEach(function (g) {
      html += '<section class="scrum-chart"><p>' + esc(g.unit) + '</p>' + (g.partial ? '<p>' + esc(d.partialSnapshot) + '</p>' : '');
      g.rows.forEach(function (row) {
        var at = new Date(row.capturedAt).toISOString();
        html += '<figure class="scrum-chart-row scrum-daily-row"><figcaption><time datetime="' + at + '">' + at + '</time></figcaption>';
        ['scope', 'remaining', 'completed'].forEach(function (k) {
          var v = row[k][metric], width = g.max ? 100 * v / g.max : 0;
          html += '<div class="scrum-chart-series"><span class="scrum-chart-label">' + esc(d[k]) + ': ' + esc(v) + '</span>' +
            '<div class="scrum-chart-track" aria-hidden="true"><div class="scrum-chart-bar ' + k + '" style="width: ' + width + '%"></div></div>' +
            '<small>' + esc(dailyTotal(sec, row[k])) + '</small></div>';
        });
        html += '</figure>';
      });
      html += '</section>';
    });
    body.innerHTML = html;
  }
  up.compiler('.js-scrum-daily', function (sec) {
    var gone = false;
    sec.weddyMetric = 'count';
    window.Meteor.callAsync('scrum.getDailyHistory', sec.dataset.board, sec.dataset.sprint).then(function (history) {
      if (gone || !sec.isConnected) return;
      sec.weddyHistory = history;
      drawDaily(sec, history, sec.weddyMetric);
    }, function (e) {
      if (gone || !sec.isConnected) return;
      sec.querySelector('.js-scrum-daily-body').innerHTML = '<p class="warning" role="alert">' + esc(e.reason || e.message) + '</p>';
    });
    return function () { gone = true; };
  });
  up.on('change', '.js-scrum-daily-metric', function (event, el) {
    var sec = el.closest('.js-scrum-daily');
    sec.weddyMetric = el.value === 'estimate' ? 'estimate' : 'count';
    if (sec.weddyHistory) drawDaily(sec, sec.weddyHistory, sec.weddyMetric);
  });
  // WeKan's exportChartPopup: the chart's PDF and Excel, each link carrying a
  // download token of its own, as WeKan's carry the login token.
  up.on('click', '.js-export-chart', function (event, el) {
    event.preventDefault();
    var board = (el.closest('[data-board]') || document.querySelector('[data-board]')).dataset.board;
    callAsync('exportDownloadToken').then(function (token) {
      closePopOvers();
      var p = document.createElement('div');
      p.className = 'pop-over js-pop-over';
      p.setAttribute('role', 'dialog');
      var list = '<ul class="pop-over-list">';
      [['exportPDF', 'fa-file-pdf-o', 'PDF'], ['exportExcel', 'fa-file-excel-o', 'Excel']].forEach(function (f) {
        var q = new URLSearchParams({ authToken: token });
        if (el.dataset.sprintId) q.set('sprintId', el.dataset.sprintId);
        list += '<li><a href="/api/boards/' + encodeURIComponent(board) + '/charts/' + encodeURIComponent(el.dataset.chartKey) + '/' + f[0] + '?' + q.toString() +
          '" download><i class="fa ' + f[1] + '"></i> ' + f[2] + '</a></li>';
      });
      p.innerHTML = '<div class="header"><a class="close-btn js-close-pop-over" href="#">×</a></div><div class="content">' + list + '</ul></div>';
      document.body.appendChild(p);
    }, function (e) { scrumError(e.reason || e.message); });
  });
  document.addEventListener('keydown', function (event) { if (event.key === 'Escape' && document.querySelector('.js-pop-over')) closePopOvers(); });

  // WeKan's scrumScopeHistory: a point per change, each bar on the sprint's
  // largest scope; a History that does not reproduce the sprint says so.
  up.compiler('.js-scrum-scope', function (sec) {
    var gone = false, d = sec.dataset, body = sec.querySelector('.js-scrum-scope-body');
    window.Meteor.callAsync('scrum.getScopeHistory', d.board, d.sprint).then(function (h) {
      if (gone || !sec.isConnected) return;
      var html = '';
      if (h.partial) html += '<p class="scrum-partial-report">' + esc(d.partial) + '</p>';
      if (h.consistent === false) html += '<p class="warning js-scrum-scope-inconsistent">' + esc(d.inconsistent) + '</p>';
      if (h.truncated) html += '<p class="warning">' + esc(d.truncated) + '</p>';
      var points = h.points || [];
      if (!points.length) { body.innerHTML = html + '<p>' + esc(d.empty) + '</p>'; return; }
      var max = Math.max.apply(null, [1].concat(points.map(function (p) { return p.scope; })));
      var cause = function (c) {
        var key = 'cause' + String(c).replace(/^./, function (x) { return x.toUpperCase(); });
        return d[key] || ('scrum-scope-cause-' + c);
      };
      html += '<section class="scrum-chart"><p>' + esc(h.unit) + '</p>';
      points.forEach(function (p) {
        var at = new Date(p.at).toISOString();
        html += '<figure class="scrum-chart-row scrum-scope-row"><figcaption><time datetime="' + at + '">' + at + '</time> ' + esc(cause(p.cause)) + '</figcaption>';
        ['scope', 'remaining', 'completed'].forEach(function (k) {
          html += '<div class="scrum-chart-series"><span class="scrum-chart-label">' + esc(d[k]) + ': ' + esc(p[k]) + '</span>' +
            '<div class="scrum-chart-track" aria-hidden="true"><div class="scrum-chart-bar ' + k + '" style="width: ' + Math.round(p[k] / max * 100) + '%"></div></div></div>';
        });
        html += '</figure>';
      });
      body.innerHTML = html + '</section>';
    }, function (e) {
      if (gone || !sec.isConnected) return;
      body.innerHTML = '<p class="warning" role="alert">' + esc(e.reason || e.message) + '</p>';
    });
    return function () { gone = true; };
  });

  // WeKan's import-into-board panel (scrumView.js importTransfer): the file
  // reduced to what the import reads (scrumTransferFileFields), a dry run,
  // then the import - the panel stays as it is, showing what happened.
  function transferFields(file) {
    if (!file || typeof file !== 'object' || Array.isArray(file)) throw new Error('Invalid Scrum transfer: expected a JSON object');
    if (typeof file.format === 'string') return { scrumTransfer: file, scrumTransferLosses: [], cards: [], lists: [], customFields: [] };
    if (!file.scrumTransfer || typeof file.scrumTransfer !== 'object') throw new Error('Invalid Scrum transfer: this file has no Scrum planning');
    var rows = function (list, pick) {
      return (Array.isArray(list) ? list : []).filter(function (r) { return r && typeof r._id === 'string' && r._id && r._id.length <= 200; }).map(pick);
    };
    var out = { scrumTransfer: file.scrumTransfer, scrumTransferLosses: Array.isArray(file.scrumTransferLosses) ? file.scrumTransferLosses : [],
      cards: rows(file.cards, function (r) { var o = { _id: r._id }; if (typeof r.title === 'string') o.title = r.title.slice(0, 10000); if (Number.isSafeInteger(r.cardNumber)) o.cardNumber = r.cardNumber; return o; }),
      lists: rows(file.lists, function (r) { var o = { _id: r._id }; if (typeof r.title === 'string') o.title = r.title.slice(0, 10000); return o; }),
      customFields: rows(file.customFields, function (r) { var o = { _id: r._id }; if (typeof r.name === 'string') o.name = r.name.slice(0, 10000); if (typeof r.type === 'string') o.type = r.type.slice(0, 100); return o; }) };
    if (typeof file._id === 'string' && file._id.length <= 500) out._id = file._id;
    return out;
  }
  function transferChanges(p) {
    return !!(p.sprints.created.length || p.releases.created.length || p.events.created || p.cards.updated || p.dailyObservations);
  }
  function drawTransfer(panel, p) {
    var d = panel.dataset, fill = function (text, values) {
      Object.keys(values).forEach(function (k) { text = text.split('__' + k + '__').join(values[k]); });
      return text;
    };
    var html = '<div class="scrum-transfer-preview" role="status">';
    if (p.changed) html += '<p class="js-scrum-transfer-done">' + esc(d.done) + '</p>';
    ['sprints', 'releases'].forEach(function (k) {
      html += '<p>' + esc(fill(d[k], { created: p[k].created.length, matched: p[k].matched.length, skipped: p[k].skipped.length })) + '</p>';
    });
    html += '<p>' + esc(fill(d.cards, { updated: p.cards.updated, unchanged: p.cards.unchanged, unmatched: p.cards.unmatched })) + '</p>';
    if (p.dryRun && !transferChanges(p)) html += '<p class="js-scrum-transfer-nothing">' + esc(d.nothing) + '</p>';
    (p.losses || []).forEach(function (l) {
      var key = 'loss' + String(l.reason || '').replace(/(^|-)([a-z])/g, function (m, a, c) { return c.toUpperCase(); });
      html += '<p class="scrum-transfer-loss">' + esc(fill(d[key] || d.lossOmitted, { reference: l.sourceId })) + '</p>';
    });
    if (p.dryRun && transferChanges(p)) html += '<button class="primary js-scrum-transfer-apply" type="button">' + esc(d.import) + '</button>';
    panel.querySelector('.js-scrum-transfer-result').innerHTML = html + '</div>';
  }
  function runTransfer(panel, dryRun) {
    if (!panel.weddyFile) { scrumError(panel.dataset.choose); return; }
    scrumError('');
    window.Meteor.callAsync('scrum.importIntoBoard', panel.dataset.board, panel.weddyFile, { dryRun: dryRun })
      .then(function (p) { drawTransfer(panel, p); }, function (e) { scrumError(e.reason || e.message); });
  }
  up.on('change', '.js-scrum-transfer-file', function (event, el) {
    var panel = el.closest('.scrum-transfer-import'), file = el.files && el.files[0];
    panel.weddyFile = null;
    panel.querySelector('.js-scrum-transfer-result').innerHTML = '';
    scrumError('');
    if (!file) return;
    file.text().then(function (text) {
      try { panel.weddyFile = transferFields(JSON.parse(text)); }
      catch (e) { scrumError(e instanceof SyntaxError ? panel.dataset.invalid : e.message); }
    });
  });
  up.on('click', '.js-scrum-transfer-preview', function (event, el) { event.preventDefault(); runTransfer(el.closest('.scrum-transfer-import'), true); });
  up.on('click', '.js-scrum-transfer-apply', function (event, el) { event.preventDefault(); runTransfer(el.closest('.scrum-transfer-import'), false); });

  // WeKan's scrumMetadata form: the card's Scrum fields through
  // scrum.updateCard - every chosen release, none when cleared.
  up.on('click', '.minicard .scrum-metadata', function (event) { event.stopPropagation(); });
  up.on('submit', '.js-scrum-metadata', function (event, form) {
    event.preventDefault();
    var fd = new FormData(form), meta = {};
    fd.forEach(function (v, k) { if (!(k in meta)) meta[k] = v; });
    ['sprintId', 'releaseId'].forEach(function (k) { if (k in meta) meta[k] = meta[k] || null; });
    if (form.querySelector('select[name="releaseIds"]')) meta.releaseIds = fd.getAll('releaseIds').filter(Boolean);
    if ('backlogRank' in meta) meta.backlogRank = meta.backlogRank === '' ? null : Number(meta.backlogRank);
    callAsync('scrum.updateCard', form.dataset.board, form.dataset.card, meta, Number(form.dataset.revision || 0))
      .then(function () { reloadBoard(); }, function (e) {
        var p = form.querySelector('.warning') || form.insertBefore(document.createElement('p'), form.firstChild);
        p.className = 'warning'; p.setAttribute('role', 'alert'); p.textContent = e.reason || e.message;
      });
  });

  // WeKan's Scrum views open the board's History in a pop-over, at its
  // Scrum changes: the History page's own table, its Restore included.
  up.on('click', '.js-scrum-history', function (event, el) {
    event.preventDefault();
    fetch(el.getAttribute('href'), { credentials: 'same-origin' }).then(function (r) { return r.text(); }).then(function (html) {
      var table = new DOMParser().parseFromString(html, 'text/html').querySelector('.history-table');
      if (!table) return;
      closePopOvers();
      var p = document.createElement('div');
      p.className = 'pop-over js-pop-over';
      p.setAttribute('role', 'dialog');
      p.innerHTML = '<div class="header"><a class="close-btn js-close-pop-over" href="#">×</a></div><div class="content"></div>';
      p.querySelector('.content').appendChild(document.importNode(table, true));
      document.body.appendChild(p);
      up.hello(p);
    });
  });

  // The stopped Scrum History operation's notice: its board admin rolls it
  // back or, confirmed, discards it (scrum.resolveHistoryCheckpoint).
  up.on('click', '.js-history-checkpoint-rollback, .js-history-checkpoint-discard', function (event, el) {
    event.preventDefault();
    var notice = el.closest('.js-history-checkpoint-notice');
    var action = el.classList.contains('js-history-checkpoint-rollback') ? 'rollback' : 'discard';
    if (action === 'discard' && !confirm(notice.dataset.confirm)) return;
    el.disabled = true;
    callAsync('scrum.resolveHistoryCheckpoint', notice.dataset.board, notice.dataset.key, action).then(function () {
      notice.remove();
      reloadBoard();
    }, function (e) {
      el.disabled = false;
      var p = notice.querySelector('.js-history-checkpoint-error');
      p.textContent = e.reason || e.message;
      p.hidden = false;
    });
  });

  // An interrupted Scrum import, finished or discarded by a board admin
  // (scrum.resumeImport, scrum.discardImport).
  up.on('click', '.js-scrum-resume-import, .js-scrum-discard-import', function (event, el) {
    event.preventDefault();
    scrumMutate(el.classList.contains('js-scrum-resume-import') ? 'scrum.resumeImport' : 'scrum.discardImport');
  });

  // While a rollover or a large undo runs in the background, the Scrum view
  // looks again every few seconds; nothing is polled otherwise.
  up.compiler('.scrum-view[data-polling="true"]', function () {
    var t = setTimeout(function () { up.reload('.scrum-view', { cache: false }); }, 3000);
    return function () { clearTimeout(t); };
  });
})();
