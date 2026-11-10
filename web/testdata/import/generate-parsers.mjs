// Writes <source>-<input>.parsed.golden: Node.js WeKan's own parser
// (models/lib/externalParsers.js) given each input, which
// TestImportParsersAreWeKans compares Weddy's with:
//   node web/testdata/import/generate-parsers.mjs /path/to/wekan
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate-parsers.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const { EXTERNAL_PARSERS } = await import(pathToFileURL(path.join(wekan, 'models/lib/externalParsers.js')).href);
const cases = JSON.parse(fs.readFileSync(path.join(here, 'parser-cases.json'), 'utf8'));
for (const [source, inputs] of Object.entries(cases)) {
  for (const input of inputs) {
    const text = fs.readFileSync(path.join(here, input), 'utf8');
    const parsed = EXTERNAL_PARSERS[source](JSON.parse(text));
    const name = path.basename(input).replace(/\.[^.]+$/, '');
    fs.writeFileSync(path.join(here, `${source}-${name}.parsed.golden`), `${JSON.stringify(parsed, null, 2)}\n`);
  }
}
