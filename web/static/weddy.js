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
  var stale = false;  // the board changed during a drag: reload when the last ends

  var staleUnderPopOver = false; // the board changed while a pop-over was open
  function reloadBoard() {
    if (dragging > 0) { stale = true; return; }
    // An open pop-over is not swept away by somebody else's change: the board
    // catches up when it closes.
    if (document.querySelector('.js-pop-over')) { staleUnderPopOver = true; return; }
    // A change of this page's own - a form still on its way - is announced
    // before its answer arrives: reload after it, or the older page wins.
    if (up.network.isBusy()) { setTimeout(reloadBoard, 150); return; }
    stale = false;
    if (document.querySelector('.board-wrapper')) up.reload('.board-wrapper');
  }

  // Other users' changes arrive as "board-changed".
  up.compiler('.board-wrapper[data-live]', function (el) {
    if (typeof EventSource === 'undefined') return;
    var es = new EventSource(el.getAttribute('data-live'));
    es.addEventListener('board-changed', reloadBoard);
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
  up.on('click', '.js-open-board-menu', function (event) {
    event.preventDefault();
    openPopOver('board-menu');
  });
  up.on('click', '.js-export-board', function (event) {
    event.preventDefault();
    openPopOver('export');
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
    if (event.key === 'Escape') closePopOvers();
  });

  // A section heading with a caret (WeKan's client/lib/sectionCaret.js): it
  // shows or hides the section after it, and the caret says which.
  up.on('click', '.js-toggle-card-section, .js-toggle-show-activities', function (event, el) {
    event.preventDefault();
    var section = el.nextElementSibling;
    if (!section) return;
    var open = section.classList.toggle('is-hidden') === false;
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
  // Enter adds the comment; Shift+Enter is a new line.
  up.on('keydown', '.js-new-comment-input', function (event, el) {
    if (event.key === 'Enter' && !event.shiftKey && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      up.submit(el.form);
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
    up.submit(el.closest('form'));
  });

  // A card full-screen, or not: the user's own choice (toggleCardMaximized).
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
  // The card title's editor: open on its leading part, Ctrl/Cmd+Enter saves.
  up.on('click', '.card-details-title-edit-zone', function (event, el) {
    event.preventDefault();
    var form = el.closest('.card-details-header').querySelector('.js-card-details-title');
    if (!form) return;
    form.classList.remove('is-hidden');
    form.querySelector('textarea').focus();
  });
  up.on('keydown', '.js-card-details-title textarea', function (event, el) {
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      up.submit(el.form);
    }
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
    list.querySelectorAll('.js-card-composer').forEach(function (f) { f.remove(); });
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
  var multiSelection = { on: false, cards: {} };
  function showMultiSelection(wrapper) {
    if (!multiSelection.on) return;
    var sb = wrapper.querySelector('.board-sidebar');
    if (sb) sb.classList.add('is-open');
    var view = wrapper.querySelector('.js-multiselection-view');
    if (view) view.classList.remove('is-hidden');
    wrapper.querySelectorAll('.js-minicard').forEach(function (c) {
      c.classList.toggle('is-selected', !!multiSelection.cards[c.dataset.card]);
    });
  }
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
      multiSelection = { on: false, cards: {} };
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
  up.on('click', '.js-open-list-menu', function (event, el) {
    event.preventDefault();
    menuList = el.closest('.js-list');
    openPopOver('list-menu');
    var w = document.querySelector('.js-pop-over .js-toggle-watch-list');
    if (w) w.textContent = menuList.dataset.watching ? w.dataset.unwatch : w.dataset.watch;
  });
  up.on('click', '.js-add-card', function (event, el) {
    event.preventDefault();
    var inMenu = el.closest('.js-pop-over');
    var list = inMenu ? menuList : el.closest('.js-list');
    if (inMenu) closePopOvers();
    openComposer(list, el.classList.contains('list-header-plus-bottom') ? 'bottom' : 'top');
  });
  up.on('click', '.js-close-inlined-form', function (event, el) {
    event.preventDefault();
    var form = el.closest('.js-inlined-form');
    if (form.classList.contains('js-card-composer')) form.remove();
    else form.classList.add('is-hidden');
  });
  // Enter adds the card, as in WeKan; Shift+Enter is a new line.
  up.on('keydown', '.js-card-composer textarea', function (event, el) {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault();
      up.submit(el.form);
    }
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
  // Export the ticked rules, or all of them.
  up.on('click', '.js-rules-export-json, .js-rules-export-csv', function (event, el) {
    var page = document.querySelector('.rules-page');
    if (!page) return;
    var ids = Array.prototype.map.call(page.querySelectorAll('.js-rule-select:checked'), function (b) { return b.dataset.ruleId; });
    if (ids.length) el.href = el.href.split('&ids=')[0] + '&ids=' + ids.join(',');
  });
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
    callAsync('rules.createRule', page.dataset.board, ruleState.name, ruleState.trigger, action).then(function () {
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
    function load(page) {
      var request = ++state.request;
      state.rows = []; state.page = page || 1;
      render();
      callAsync('activityNotificationRecoveryReport', { search: state.search, page: state.page }).then(function (result) {
        if (request !== state.request) return;
        state.rows = result.rows || []; state.total = result.total || 0; state.page = result.page || 1;
        render();
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
    return function () { search.removeEventListener('keydown', onSearchKey); panel.removeEventListener('click', onClick); };
  });

  window.weddyMultiDrag = { sorts: sorts }; // for the tests
})();
