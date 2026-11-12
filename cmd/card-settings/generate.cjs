'use strict';
// Writes web/card_settings.json from WeKan's own tables of Board Settings /
// Card (models/lib/cardSettingsRows.js, models/lib/cardSettingsSections.js):
//
//   node cmd/card-settings/generate.cjs /path/to/wekan > web/card_settings.json
//
// Each side of each row gets the BOARD field its checkbox writes - the field
// WeKan's click handler ($set in client/components/sidebar/sidebar.js) sets,
// which is the row's own field but for one - and what a board that never set
// it reads: the row's `default`, else the Boards schema's defaultValue.
// web/card_settings_test.go runs this again when WeKan is beside the plugin,
// so the copy cannot drift from WeKan's.
const fs = require('fs');
const path = require('path');

const wekan = path.resolve(process.argv[2] || '../../..');
const { CARD_SETTINGS_ROWS } = require(path.join(wekan, 'models/lib/cardSettingsRows.js'));
const { SECTIONS, DEFAULT_SECTION_ORDER, labelInSection, sectionOf } = require(path.join(wekan, 'models/lib/cardSettingsSections.js'));
const sidebar = fs.readFileSync(path.join(wekan, 'client/components/sidebar/sidebar.js'), 'utf8');
const schema = fs.readFileSync(path.join(wekan, 'models/boards.js'), 'utf8');

const setBy = {};
const handlerRe = /'click \.(js-[\w-]+)'\s*\([^)]*\)\s*\{([\s\S]*?)\n  \},/g;
for (let m; (m = handlerRe.exec(sidebar));) {
  const set = m[2].match(/\$set:\s*\{\s*(\w+):/);
  if (set && !setBy[m[1]]) setBy[m[1]] = set[1];
}
const schemaDefault = field => {
  const at = schema.indexOf(`\n    ${field}: {`);
  if (at < 0) return null;
  const body = schema.slice(at, schema.indexOf('\n    },', at));
  const d = body.match(/defaultValue:\s*(true|false)/);
  return d ? d[1] === 'true' : null;
};

const keys = new Set();
const rows = CARD_SETTINGS_ROWS.map(r => {
  const section = sectionOf(r.key);
  const label = labelInSection(r, section);
  label.forEach(k => keys.add(k));
  const out = { key: r.key, icons: r.icons, label, labelSeparator: r.labelSeparator || ' ' };
  for (const side of ['card', 'minicard']) {
    const s = r[side];
    if (!s) continue;
    const spec = {};
    if (s.after) spec.after = s.after;
    if (s.needsCard) spec.needsCard = true;
    if (s.scrum) {
      spec.kind = 'scrum';
      spec.scrum = s.scrum;
    } else if (s.personal) {
      spec.kind = 'personal';
      spec.field = s.field;
    } else {
      spec.kind = 'board';
      spec.field = s.flag ? s.field : setBy[s.toggle];
      if (!spec.field) throw new Error(`${r.key}/${side}: no board field for ${s.toggle}`);
      const d = s.flag ? s.default : schemaDefault(spec.field);
      spec.default = d === null ? false : d;
    }
    out[side] = spec;
  }
  return out;
});
SECTIONS.forEach(s => keys.add(s.labelKey));
const sections = SECTIONS.map(s => ({ id: s.id, icon: s.icon, labelKey: s.labelKey, general: !!s.general, keys: s.keys }));
process.stdout.write(JSON.stringify({
  rows, sections,
  order: { card: DEFAULT_SECTION_ORDER.card, minicard: DEFAULT_SECTION_ORDER.minicard },
  i18nKeys: [...keys].sort(),
}, null, 1) + '\n');
