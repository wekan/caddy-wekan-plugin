'use strict';
// Writes web/board_views.json from WeKan's BOARD_VIEWS
// (models/lib/boardViewSettings.js): each view's key, icon and name.
//
//   node cmd/board-views/generate.cjs /path/to/wekan > web/board_views.json
//
// web/board_view_settings_test.go runs this again when WeKan is beside the
// plugin, so the copy cannot drift from WeKan's.
const path = require('path');
const wekan = path.resolve(process.argv[2] || '../../..');
const { BOARD_VIEWS } = require(path.join(wekan, 'models/lib/boardViewSettings.js'));
const views = BOARD_VIEWS.map(v => ({ view: v.view, labelKey: v.labelKey, icon: v.icon }));
process.stdout.write(JSON.stringify({ views, i18nKeys: [...new Set(views.map(v => v.labelKey))].sort() }, null, 1) + '\n');
