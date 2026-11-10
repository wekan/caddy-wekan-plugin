// Writes card-documents.golden: Node.js WeKan's card document
// (models/lib/cardExportDocument.js and cardDocument.js) of each case in
// card-documents.json, which TestCardDocumentIsWeKans compares Weddy's with:
//   node web/testdata/export/generate-card-documents.mjs /path/to/wekan
// A {"$date": iso} is a Date. Dates are written as ISO text with " UTC" and
// a custom field's value as it is, the same on both sides.
import fs from 'node:fs';
import path from 'node:path';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate-card-documents.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const { buildExportCardDocument } = await import(path.join(wekan, 'models/lib/cardExportDocument.js'));
const en = JSON.parse(fs.readFileSync(path.join(wekan, 'imports/i18n/data/en.i18n.json'), 'utf8'));
const revive = v => {
  if (Array.isArray(v)) return v.map(revive);
  if (v && typeof v === 'object') {
    if (Object.keys(v).length === 1 && v.$date) return new Date(v.$date);
    return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, revive(x)]));
  }
  return v;
};
const cases = JSON.parse(fs.readFileSync(path.join(here, 'card-documents.json'), 'utf8'));
const golden = {};
for (const c of cases) {
  golden[c.name] = buildExportCardDocument(revive(c.data), {
    fields: c.fields,
    userName: id => c.users[id] || id || '',
    formatDate: v => (v instanceof Date ? `${v.toISOString()} UTC` : ''),
    customFieldValue: (definition, value) => value,
    translate: (key, fallback) => en[key] || fallback,
  });
}
fs.writeFileSync(path.join(here, 'card-documents.golden'), `${JSON.stringify(golden, null, 2)}\n`);
