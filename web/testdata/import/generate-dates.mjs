// Writes dates.golden: what V8's new Date(text).toISOString() gives at UTC
// (WeKan's server's zone) for date-times-cases.json, which TestJSDateTime
// compares Weddy's jsDateTime with:
//   TZ=UTC node web/testdata/import/generate-dates.mjs
import fs from 'node:fs';
import path from 'node:path';

const here = path.dirname(new URL(import.meta.url).pathname);
const cases = JSON.parse(fs.readFileSync(path.join(here, 'date-times-cases.json'), 'utf8'));
const out = {};
for (const text of cases) {
  const date = new Date(text);
  out[text] = Number.isNaN(date.getTime()) ? null : date.toISOString();
}
fs.writeFileSync(path.join(here, 'dates.golden'), `${JSON.stringify(out, null, 2)}\n`);
