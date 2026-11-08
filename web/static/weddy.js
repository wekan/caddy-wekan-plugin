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

  window.weddyMultiDrag = { sorts: sorts }; // for the tests
})();
