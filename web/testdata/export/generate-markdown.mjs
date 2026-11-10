// Writes markdown.golden: Node.js WeKan's export markdown
// (models/lib/exportMarkdown.js) of each case in markdown-cases.json - its
// blocks, its one-cell runs and its plain text - which
// TestExportMarkdownIsWeKans compares Weddy's with:
//   node web/testdata/export/generate-markdown.mjs /path/to/wekan
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate-markdown.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const require = createRequire(path.join(wekan, 'models/lib/exportMarkdown.js'));
const { markdownBlocks, markdownRuns, markdownPlainText } = require(path.join(wekan, 'models/lib/exportMarkdown.js'));
const cases = JSON.parse(fs.readFileSync(path.join(here, 'markdown-cases.json'), 'utf8'));
const golden = cases.map(source => ({
  source, blocks: markdownBlocks(source), runs: markdownRuns(source), plain: markdownPlainText(source),
}));
fs.writeFileSync(path.join(here, 'markdown.golden'), `${JSON.stringify(golden, null, 2)}\n`);
