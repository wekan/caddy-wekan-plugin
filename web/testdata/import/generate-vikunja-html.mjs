// Writes vikunja-html.golden: Node.js WeKan's vikunjaHtmlToText
// (models/lib/vikunjaFormat.js) for vikunja-html-cases.json, with and
// without checklists, which TestVikunjaHTMLIsWeKans compares Weddy's with:
//   TZ=UTC node web/testdata/import/generate-vikunja-html.mjs /path/to/wekan
// (at UTC, as WeKan's server runs: V8 reads a date with a space before its
// time in the local zone)
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate-vikunja-html.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const { vikunjaHtmlToText } = await import(pathToFileURL(path.join(wekan, 'models/lib/vikunjaFormat.js')).href);
const cases = JSON.parse(fs.readFileSync(path.join(here, 'vikunja-html-cases.json'), 'utf8'));
const out = cases.map(html => ({ html, with: vikunjaHtmlToText(html), without: vikunjaHtmlToText(html, { checklists: false }) }));
fs.writeFileSync(path.join(here, 'vikunja-html.golden'), `${JSON.stringify(out, null, 2)}\n`);

// vikunja-archive.golden: parseVikunjaExport on what server/lib/vikunjaArchive.js
// reads from a .zip - its VERSION, data.json, file and filter counts - which
// TestVikunjaArchiveIsWeKans compares with Weddy reading such a .zip.
const { parseVikunjaExport } = await import(pathToFileURL(path.join(wekan, 'models/lib/vikunjaFormat.js')).href);
const data = fs.readFileSync(path.join(here, 'vikunja-edge.json'), 'utf8');
const archives = {};
for (const version of ['v0.24.1', 'dev', '0.20.1', '0.20.0', 'v1.0.0-rc1', 'junk']) {
  try { archives[version] = parseVikunjaExport({ version, data, files: 2, filters: 3 }); } catch (error) { archives[version] = { error: error.message }; }
}
fs.writeFileSync(path.join(here, 'vikunja-archive.golden'), `${JSON.stringify(archives, null, 2)}\n`);
