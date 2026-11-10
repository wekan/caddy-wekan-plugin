// Writes split.golden: Node.js WeKan's "One board per project"
// (models/lib/importSplit.js splitBySwimlane) of every parsed fixture and of
// split/*.json, which TestImportSplitIsWeKans compares Weddy's with:
//   TZ=UTC node web/testdata/import/generate-split.mjs /path/to/wekan
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate-split.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const { splitBySwimlane } = await import(pathToFileURL(path.join(wekan, 'models/lib/importSplit.js')).href);
const inputs = [
  ...fs.readdirSync(here).filter(name => name.endsWith('.parsed.golden')).sort(),
  ...fs.readdirSync(path.join(here, 'split')).filter(name => name.endsWith('.json')).sort().map(name => `split/${name}`),
];
const golden = {};
for (const name of inputs) {
  const parsed = JSON.parse(fs.readFileSync(path.join(here, name), 'utf8'));
  try {
    const parts = splitBySwimlane(parsed);
    golden[name] = parts.length > 1 ? parts : 'whole';
  } catch (error) {
    golden[name] = { error: error.message };
  }
}
fs.writeFileSync(path.join(here, 'split.golden'), `${JSON.stringify(golden, null, 2)}\n`);
