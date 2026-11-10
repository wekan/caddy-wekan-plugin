// Writes notion/<name>.zip and notion/<name>.csv, Notion exports, and
// notion-<name>.parsed.golden: Node.js WeKan's readNotionImport
// (server/lib/notionArchive.js) and parseNotionExport
// (models/lib/notionFormat.js) on each, which TestImportNotionIsWeKans
// compares Weddy's with:
//   TZ=UTC node web/testdata/import/generate-notion.mjs /path/to/wekan
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate-notion.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const require = createRequire(path.join(wekan, 'server/lib/notionArchive.js'));
const { zipSync, strToU8 } = require('fflate');
const { readNotionImport } = require(path.join(wekan, 'server/lib/notionArchive.js'));
const { EXTERNAL_PARSERS } = await import(pathToFileURL(path.join(wekan, 'models/lib/externalParsers.js')).href);
const { importedPeople } = await import(pathToFileURL(path.join(wekan, 'models/lib/importMembersMode.js')).href);
const id = n => String(n).repeat(32).slice(0, 32);
const zip = files => Buffer.from(zipSync(Object.fromEntries(Object.entries(files).map(([k, v]) => [k, typeof v === 'string' ? strToU8(v) : v]))));
const tasksCsv = [
  'Name,Status,Assignee,Tags,Due,Start,Created,Last edited time,Done,Points,Notes,Related,Priority,constructor,Owner',
  `Order valves,In progress,"Ann, Bob","shop, urgent","October 8, 2026 3:00 PM → 5:00 PM",2026/10/01,"Sept 1, 2026",2026-10-02,Yes,3,Two of them,"Fit (Fit ${id(7)}.md)",High,x,Carl`,
  `A very long title that Notion shortened in its file name,Done,,,10/13/2026 (GMT+3),,,,No,4.5,,,Low,,`,
  ',Todo,,,,,,,,,,,,,',
  'Third,,Dee,,"Oct 9, 2026 → Oct 12, 2026",,,,No,x,,,Mid,,',
].join('\n');
const cases = {
  export: zip({
    [`Tasks ${id(1)}_all.csv`]: '﻿' + tasksCsv,
    [`Tasks ${id(1)}.csv`]: tasksCsv.split('\n').slice(0, 2).join('\n'),
    [`Tasks ${id(1)}/Order valves ${id(2)}.md`]: '# Order valves\n\nStatus: In progress\nAssignee: Ann\n\nThe body [file](Order%20valves/a.png) and [web](https://x.test).\n\n',
    [`Tasks ${id(1)}/A very long title that Notion shor ${id(3)}.md`]: 'Notes only, no heading\r\nline two',
    [`Tasks ${id(1)}/Unmatched ${id(4)}.md`]: '# Nobody',
    [`Tasks ${id(1)}/Order valves/a.png`]: new Uint8Array([1, 2, 3]),
    [`Tasks ${id(1)}/Order valves/Sub ${id(5)}.md`]: '# Sub',
    [`Other page ${id(6)}.md`]: '# Elsewhere',
    [`Ideas ${id(8)}.csv`]: 'Title,Stage,When\nIdea one,New,2026-11-01\nIdea two,Old,bad\nIdea three,New,',
    '__MACOSX/._junk': 'x',
    '._hidden.csv': 'x',
  }),
  nested: zip({ 'part-1.zip': zip({ [`DB ${id(9)}.csv`]: 'Name,Kind\nOne,A\nTwo,B\nThree,A' }), 'part-2.zip': zip({ 'readme.md': '# x' }) }),
  'no-csv': zip({ 'readme.md': '# nothing' }),
};
for (const [name, buffer] of Object.entries(cases)) fs.writeFileSync(path.join(here, 'notion', `${name}.zip`), buffer);
fs.writeFileSync(path.join(here, 'notion', 'single.csv'), 'Task,Phase,Members,Members 2\nOne,Plan,"Ann, Bob",Cy\nTwo,Plan,,\nThree,Build,Eve,');
fs.writeFileSync(path.join(here, 'notion', 'weddy-export.csv'), fs.readFileSync(path.join(here, '..', 'external', 'notion.golden')));
const people = {};
for (const file of fs.readdirSync(path.join(here, 'notion')).sort()) {
  const raw = fs.readFileSync(path.join(here, 'notion', file));
  const input = file.endsWith('.zip') ? { zipBase64: raw.toString('base64') } : raw.toString('utf8');
  const name = file.replace(/\.[^.]+$/, '');
  let parsed;
  try { parsed = EXTERNAL_PARSERS.notion(await readNotionImport(input)); } catch (error) { parsed = { error: error.message }; }
  fs.writeFileSync(path.join(here, `notion-${name}.parsed.golden`), `${JSON.stringify(parsed, null, 2)}\n`);
  if (!parsed.error) people[`notion-${name}`] = importedPeople(parsed.tasks);
}
fs.writeFileSync(path.join(here, 'notion-people.golden'), `${JSON.stringify(people, null, 2)}\n`);
