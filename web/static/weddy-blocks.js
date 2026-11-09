// The Rules page's Blocks view, as Node.js WeKan's client/components/rules/
// rulesBlocks.js and blocks/editor.js, with models/lib/rulesBlocks.js and
// rulesBlocksDefinition.js: a rule is a "When" block holding its trigger
// joined to a "Then" block holding its action; Blockly is only the editor -
// what is saved is an ordinary rule, through rules.createRule or
// rules.updateRule. Its data - the rules, the palette, the words - is the
// page's (rules_blocks_view.go).
(function () {
  'use strict';
  if (!window.Blockly || !window.up) return;
  var Blockly = window.Blockly;
  var BLOCK_FIELDS = {
    listName: 'List', oldListName: 'Previous list', swimlaneName: 'Swimlane', oldSwimlaneName: 'Previous swimlane',
    cardTitle: 'Card title', checklistName: 'Checklist', itemName: 'Checklist item', username: 'Username', atTime: 'Time',
    text: 'Text', comment: 'Comment', emailTo: 'Email recipient', emailSubject: 'Email subject', emailMsg: 'Email message',
  };

  function ruleDocument(doc) {
    if (!doc || typeof doc !== 'object' || Array.isArray(doc)) throw new Error('Rule data is unavailable.');
    var out = JSON.parse(JSON.stringify(doc));
    ['_id', 'createdAt', 'modifiedAt', 'updatedAt', 'boardId'].forEach(function (k) { delete out[k]; });
    return out;
  }
  function blockState(kind, doc) {
    var d = ruleDocument(doc), key = kind === 'trigger' ? 'activityType' : 'actionType';
    if (typeof d[key] !== 'string' || !d[key]) throw new Error('The rule has no valid trigger or action.');
    return { type: 'wekan_' + kind, extraState: { document: d } };
  }
  function ruleWorkspace(trigger, action) {
    var block = blockState('trigger', trigger);
    block.x = 35; block.y = 35;
    block.next = { block: blockState('action', action) };
    return { blocks: { languageVersion: 0, blocks: [block] } };
  }
  function workspaceRule(ws) {
    var roots = ws.getTopBlocks(false), trigger = roots[0], action = trigger && trigger.getNextBlock();
    if (roots.length !== 1 || !trigger || trigger.type !== 'wekan_trigger' || !action || action.type !== 'wekan_action' ||
        action.getNextBlock() || ws.getAllBlocks(false).length !== 2) {
      throw new Error('Connect exactly one trigger to one action.');
    }
    return { trigger: trigger.ruleDocument(), action: action.ruleDocument() };
  }
  function readable(v) { return String(v || '').replace(/([a-z])([A-Z])/g, '$1 $2').replace(/[-_]/g, ' '); }

  function register(data) {
    var words = data.words || {}, fields = data.fields || {};
    var palette = (data.triggers || []).concat(data.actions || []);
    function typeLabel(doc) {
      for (var i = 0; i < palette.length; i++) {
        var e = palette[i].doc;
        if (doc.activityType ? e.activityType === doc.activityType : e.actionType === doc.actionType) return palette[i].label;
      }
      return readable(doc.activityType || doc.actionType);
    }
    ['trigger', 'action'].forEach(function (kind) {
      var heading = kind === 'trigger' ? (words['r-when'] || 'When') : (words['r-action'] || 'Then');
      Blockly.Blocks['wekan_' + kind] = {
        init: function () {
          this.document = {};
          this.appendDummyInput('heading').appendField(heading, 'heading');
          this.setColour(kind === 'trigger' ? 145 : 225);
          if (kind === 'trigger') this.setNextStatement(true, 'WEKAN_ACTION');
          else this.setPreviousStatement(true, 'WEKAN_ACTION');
        },
        saveExtraState: function () { return { document: this.ruleDocument() }; },
        loadExtraState: function (state) {
          this.document = ruleDocument(state.document);
          this.setFieldValue(heading + ': ' + typeLabel(this.document), 'heading');
          this.setTooltip(this.document.desc || words['r-edit-rule-trigger-action'] || '');
          for (var key in BLOCK_FIELDS) {
            if (this.getInput(key)) this.removeInput(key);
            if (typeof this.document[key] === 'string') {
              this.appendDummyInput(key).appendField(fields[key] || BLOCK_FIELDS[key])
                .appendField(new Blockly.FieldTextInput(this.document[key]), key);
            }
          }
        },
        ruleDocument: function () {
          var doc = ruleDocument(this.document), changed = false, key;
          for (key in BLOCK_FIELDS) {
            if (!this.getField(key)) continue;
            var value = this.getFieldValue(key);
            if (value !== doc[key]) changed = true;
            doc[key] = value;
          }
          if (changed) {
            var parts = [];
            for (key in BLOCK_FIELDS) if (typeof doc[key] === 'string') parts.push((fields[key] || BLOCK_FIELDS[key]) + ' ' + doc[key]);
            doc.desc = typeLabel(doc) + ': ' + parts.join(', ');
          }
          return doc;
        },
      };
    });
  }

  up.compiler('.js-rules-blocks', function (root) {
    var data = JSON.parse(root.getAttribute('data-blocks') || '{}');
    var words = data.words || {};
    var rules = {};
    (data.rules || []).forEach(function (r) { rules[r.ID] = r; });
    Blockly.setLocale(data.messages || {});
    register(data);
    var contents = function (list, kind) {
      return list.map(function (e) { var s = blockState(kind, e.doc); return { kind: 'block', type: s.type, extraState: s.extraState }; });
    };
    var host = root.querySelector('.js-rules-blocks-workspace');
    var loading = false, dirty = false, selected = '';
    var ws = Blockly.inject(host, {
      toolbox: { kind: 'categoryToolbox', contents: [
        { kind: 'category', name: words['r-trigger'] || 'Trigger', colour: 145, contents: contents(data.triggers || [], 'trigger') },
        { kind: 'category', name: words['r-action'] || 'Action', colour: 225, contents: contents(data.actions || [], 'action') },
      ] },
      renderer: 'zelos', rtl: !!data.rtl, sounds: false, trashcan: true, disable: false, comments: false, collapse: true,
      media: '/static/vendor/blockly-media/',
      move: { scrollbars: true, drag: true, wheel: true },
      zoom: { controls: true, wheel: true, startScale: 0.85, maxScale: 1.5, minScale: 0.4 },
    });
    var show = function (sel, on) { var n = root.querySelector(sel); if (n) n.hidden = !on; };
    var error = function (text) { var n = root.querySelector('.js-blocks-error'); n.textContent = text || ''; n.hidden = !text; };
    var setDirty = function (on) { dirty = on; show('.js-blocks-dirty', on); if (on) show('.js-blocks-saved', false); };
    ws.addChangeListener(function (event) {
      if (!loading && !event.isUiEvent && event.type !== Blockly.Events.FINISHED_LOADING && event.type !== Blockly.Events.VIEWPORT_CHANGE) setDirty(true);
    });
    var title = root.querySelector('.js-blocks-title');
    function load() {
      var rule = selected ? rules[selected] : null;
      loading = true;
      Blockly.Events.disable();
      try {
        ws.clear();
        if (rule) {
          if (!rule.Trigger || !rule.Action) throw new Error(words['r-blocks-unavailable'] || 'Rule data is unavailable.');
          Blockly.serialization.workspaces.load(ruleWorkspace(rule.Trigger, rule.Action), ws);
        }
        ws.clearUndo();
        error('');
      } catch (e) {
        error(e.message);
      } finally {
        Blockly.Events.enable();
        loading = false;
      }
      title.value = rule ? rule.Title || '' : '';
      setDirty(false);
    }
    function discard() { return !dirty || window.confirm(words['r-blocks-discard'] || 'Discard?'); }
    var choose = root.querySelector('.js-blocks-rule');
    choose.addEventListener('change', function () {
      if (!discard()) { choose.value = selected; return; }
      selected = choose.value;
      load();
    });
    root.querySelector('.js-blocks-new').addEventListener('click', function () {
      if (!discard()) return;
      selected = ''; choose.value = '';
      load();
    });
    root.querySelector('.js-blocks-reload').addEventListener('click', function () { if (discard()) load(); });
    title.addEventListener('input', function () { setDirty(true); });
    root.querySelector('.js-blocks-save').addEventListener('click', function () {
      var name = title.value.trim();
      if (!name) { error(words['r-rule-title-required'] || 'A title is required.'); return; }
      var docs;
      try { docs = workspaceRule(ws); } catch (e) { error(words['r-blocks-invalid'] || e.message); return; }
      var call = selected
        ? Meteor.callAsync('rules.updateRule', selected, name, docs.trigger, docs.action)
        : Meteor.callAsync('rules.createRule', root.getAttribute('data-board'), name, docs.trigger, docs.action);
      call.then(function (result) {
        var id = selected || (result && result._id);
        rules[id] = { ID: id, Title: name, Trigger: docs.trigger, Action: docs.action };
        if (!selected && id) {
          var o = document.createElement('option');
          o.value = id; o.textContent = name;
          choose.appendChild(o);
          selected = id; choose.value = id;
        } else if (choose.selectedOptions[0]) {
          choose.selectedOptions[0].textContent = name;
        }
        error('');
        setDirty(false);
        show('.js-blocks-saved', true);
      }, function (e) { error((e && (e.reason || e.error)) || String(e)); });
    });
    load();
    var observer = new ResizeObserver(function () { Blockly.svgResize(ws); });
    observer.observe(host);
    return function () { observer.disconnect(); ws.dispose(); };
  });
})();
