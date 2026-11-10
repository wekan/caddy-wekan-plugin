// Writes sanitize.golden: Node.js WeKan's import boundary
// (models/lib/importExportBoundary.js with server/lib/inputSanitizer.js)
// given sanitize-cases.json, which TestImportBoundaryIsWeKans compares
// Weddy's with:
//   node web/testdata/import/generate-sanitize.mjs /path/to/wekan
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate-sanitize.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const url = name => pathToFileURL(path.join(wekan, name)).href;
const { sanitizeInput } = await import(url('server/lib/inputSanitizer.js'));
const { sanitizeTransferValue } = await import(url('models/lib/importExportBoundary.js'));
const cases = JSON.parse(fs.readFileSync(path.join(here, 'sanitize-cases.json'), 'utf8'));
const out = {
  strings: cases.strings.map(s => sanitizeTransferValue(s, { direction: 'import', sanitizeHtml: sanitizeInput }).value),
  transfer: sanitizeTransferValue(cases.transfer, { direction: 'import', sanitizeHtml: sanitizeInput }),
};
fs.writeFileSync(path.join(here, 'sanitize.golden'), `${JSON.stringify(out, null, 2)}\n`);
