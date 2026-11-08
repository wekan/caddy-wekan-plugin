// Weddy's WeKan in the browser: Unpoly for the pages, Server-Sent Events for what
// other people change, and interact.js for MultiDrag - every finger, or the mouse,
// drags its own card at the same time, and a card dragged while others are
// selected (Ctrl/Cmd-click) takes them along. From wekan/wami's public/multidrag.
(function () {
  'use strict';

  var dragging = 0;   // drags in progress, over all pointers
  var stale = false;  // the board changed during a drag: reload when the last ends

  function reloadBoard() {
    if (dragging > 0) { stale = true; return; }
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
    document.querySelectorAll('.js-pop-over').forEach(function (p) { p.remove(); });
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
  up.on('click', '.js-toggle-page-sidebar', function (event) {
    event.preventDefault();
    var sb = document.querySelector('.board-sidebar');
    if (sb) sb.classList.toggle('is-open');
  });
  up.on('click', '.js-close-sidebar', function (event, el) {
    event.preventDefault();
    el.closest('.board-sidebar').classList.remove('is-open');
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
  up.on('up:fragment:inserted', function (event, el) {
    if (el && el.matches && el.matches('.board-wrapper')) closePopOvers();
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
        return refreshUser().then(function () { return d.result; });
      });
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
  up.on('submit', '.js-pop-over form[action$="/remove"]', function () { keepSidebarOpen = true; });
  up.on('click', '.js-manage-board-members', function (event) {
    event.preventDefault();
    openPopOver('add-member');
    var input = document.querySelector('.js-pop-over .js-search-member-input');
    if (input) input.focus();
  });
  var memberSearch = null;
  // The sidebar stays open across the board's reload after a member change.
  var keepSidebarOpen = false;
  up.compiler('.board-sidebar', function (sb) {
    if (keepSidebarOpen) {
      sb.classList.add('is-open');
      keepSidebarOpen = false;
    }
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
      .then(function () { closePopOvers(); keepSidebarOpen = true; up.reload('.board-wrapper'); });
  });

  // The card's actions menu (cardDetailsActionsPopup) and Archive's confirmation.
  up.on('click', '.js-open-card-details-menu', function (event) {
    event.preventDefault();
    openPopOver('card-actions');
  });
  up.on('click', '.js-pop-over a.js-archive', function (event) {
    event.preventDefault();
    openPopOver('card-archive');
  });

  up.on('click', '.js-edit-board-title', function (event) {
    event.preventDefault();
    openPopOver('board-title');
  });
  up.on('click', '.js-open-archives', function (event) {
    event.preventDefault();
    openPopOver('archives');
  });

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

  // The "+" of a list opens its card composer; × closes it.
  up.on('click', '.js-add-card', function (event, el) {
    event.preventDefault();
    var form = el.closest('.js-list').querySelector('.js-card-composer');
    if (!form) return;
    form.classList.remove('is-hidden');
    form.querySelector('textarea').focus();
  });
  // An item's Edit or Add opens its inlined form, beside it (cardDetails.jade).
  up.on('click', '.card-details-person-controls .js-open-inlined-form', function (event, el) {
    event.preventDefault();
    var form = el.closest('.card-details-item').querySelector('.js-inlined-form');
    if (!form) return;
    form.classList.remove('is-hidden');
    var input = form.querySelector('input[type=text]');
    if (input) input.focus();
  });
  up.on('click', '.js-close-inlined-form', function (event, el) {
    event.preventDefault();
    el.closest('.js-inlined-form').classList.add('is-hidden');
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
      el.classList.toggle('is-selected');
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

  window.weddyMultiDrag = { sorts: sorts }; // for the tests
})();
