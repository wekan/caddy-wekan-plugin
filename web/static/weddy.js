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
  function closePopOvers() {
    document.querySelectorAll('.js-pop-over').forEach(function (p) { p.classList.add('is-hidden'); });
  }
  function openPopOver(selector) {
    closePopOvers();
    var p = document.querySelector(selector);
    if (p) p.classList.remove('is-hidden');
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
    openPopOver('.js-board-menu-pop-over');
  });
  up.on('click', '.js-export-board', function (event) {
    event.preventDefault();
    openPopOver('.js-export-pop-over');
  });
  up.on('click', '.js-close-pop-over', function (event) {
    event.preventDefault();
    closePopOvers();
  });
  up.on('keydown', function (event) {
    if (event.key === 'Escape') closePopOvers();
  });

  // The "+" of a list opens its card composer; × closes it.
  up.on('click', '.js-add-card', function (event, el) {
    event.preventDefault();
    var form = el.closest('.js-list').querySelector('.js-card-composer');
    if (!form) return;
    form.classList.remove('is-hidden');
    form.querySelector('textarea').focus();
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
