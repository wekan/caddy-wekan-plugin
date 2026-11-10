// Writes plane/<name>.{json,csv,xlsx,zip}, Plane issue exports, and
// plane-<name>.parsed.golden: Node.js WeKan's readPlaneImport
// (server/lib/planeArchive.js) and parsePlaneExport (models/lib/planeFormat.js)
// on each as WeKan's page sends it - a .zip as zipBase64, a workbook as
// xlsxBase64, else its text - which TestImportPlaneIsWeKans compares Weddy's
// with:
//   TZ=UTC node web/testdata/import/generate-plane.mjs /path/to/wekan
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate-plane.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const require = createRequire(path.join(wekan, 'server/lib/planeArchive.js'));
const { zipSync, strToU8 } = require('fflate');
const ExcelJS = require('@wekanteam/exceljs');
const { readPlaneImport } = require(path.join(wekan, 'server/lib/planeArchive.js'));
const { EXTERNAL_PARSERS } = await import(pathToFileURL(path.join(wekan, 'models/lib/externalParsers.js')).href);
const { importedPeople } = await import(pathToFileURL(path.join(wekan, 'models/lib/importMembersMode.js')).href);
const issues = [
  { project_name: 'Garden', project_identifier: 'GAR', identifier: 'GAR-1', sequence_id: 1, name: " Order valves ", state_name: 'In Progress',
    priority: 'High', assignees: ['Ann', 'Bob', 'Ann'], subscribers: ['Cy'], created_by_name: 'Carl', start_date: '2026-10-01',
    target_date: '2026-10-10T14:30:00+0300', completed_at: '', created_at: '2026-09-01 08:00:00.123', updated_at: 'x', archived_at: '',
    estimate: '3', labels: ['shop', 'shop', ' urgent '], cycles: ['Sprint 1'], modules: ['Pipes', 'Pumps'],
    links: [{ url: 'https://x.test/a b', title: 'Spec [v2]' }, 'ftp://no', { url: 'mailto:a@x.test' }],
    relations: [{ type: 'blocked_by', issue: 'GAR-2' }, { type: 'relates_to', issue: 'GAR-2', direction: 'incoming' }, { type: 'start_before', issue: 'GAR-2' }],
    comments: [{ comment: 'Looks good', created_by: 'Ann', created_at: '2026-09-02T09:00:00Z' }, { comment: '  ' }, 5],
    sub_issues_count: 0, link_count: 2, attachment_count: 2, is_draft: 'TRUE', custom_thing: 1 },
  { project_name: 'Garden', project_identifier: 'GAR', sequence_id: 2, name: 'Second', state_name: 'Done', priority: 'constructor',
    relations: [{ type: 'blocked_by', issue: 'GAR-1', direction: 'incoming' }, { type: 'relates_to', issue: 'GAR-1' }],
    archived_at: '2026-02-30', estimate: 'big', parent: 'GAR-1' },
  { project_name: 'Garden', project_identifier: 'G2', name: 'Other garden', state_name: '', priority: 'none' },
  { name: '', state_name: 'x' },
  'not an issue',
];
const csvRows = [
  ['Project Name', 'Project Identifier', 'Name', 'State Name', 'Priority', 'Assignees', 'Labels', 'Links', 'Target Date', "'=Formula Col"],
  ['Beds', 'BED', "'=SUM(1)", 'Todo', 'urgent', '["Ann"]', '[broken', '[{"url": "https://y.test"}]', '10/10/2026', 'x'],
  ['Beds', 'BED', 'Water', '', 'Medium', '', '["a","b"]', '', '2026-10-11', ''],
  ['', '', '', '', '', '', '', '', '', ''],
];
const csv = csvRows.map(r => r.map(c => /[",\n]/.test(c) ? `"${c.replace(/"/g, '""')}"` : c).join(',')).join('\n');
const workbook = async () => {
  const wb = new ExcelJS.Workbook();
  const s = wb.addWorksheet('Issues');
  s.addRow(['Project Name', 'Name', 'State Name', 'Assignees', 'Labels', 'Relations', 'Start Date', 'Estimate', 'Comments']);
  const r = s.addRow(['Lawn', 'Mow', 'Backlog', 'Ann, Bob', 'green, ', "[{'type': 'blocked_by'}]", new Date(Date.UTC(2026, 9, 1)), 2, '']);
  r.getCell(7).numFmt = 'yyyy-mm-dd';
  s.addRow([]);
  s.addRow(['Lawn', { richText: [{ text: 'Rake ' }, { text: 'leaves' }] }, 'Backlog', '', '', '', '', { formula: '1+1', result: 2 }]);
  return Buffer.from(await wb.xlsx.writeBuffer());
};
const files = {
  'issues-json.json': JSON.stringify(issues),
  'issues-csv.csv': csv,
  'workbook.xlsx': await workbook(),
  'export.zip': Buffer.from(zipSync({ 'export/issues.json': strToU8(JSON.stringify(issues.slice(0, 2))), 'export/issues.csv': strToU8(csv),
    'export/issues.xlsx': new Uint8Array(await workbook()), 'export/readme.txt': strToU8('x'), '__MACOSX/._x.json': strToU8('x'), '.hidden/a.json': strToU8('[]') })),
  'empty.zip': Buffer.from(zipSync({ 'readme.txt': strToU8('x') })),
  'not-a-list.json': '{"name": "x"}',
};
const people = {};
for (const [file, content] of Object.entries(files)) {
  fs.writeFileSync(path.join(here, 'plane', file), content);
  const raw = fs.readFileSync(path.join(here, 'plane', file));
  const input = file.endsWith('.zip') ? { zipBase64: raw.toString('base64') } : file.endsWith('.xlsx') ? { xlsxBase64: raw.toString('base64') } : raw.toString('utf8');
  const name = file.replace(/\.[^.]+$/, '');
  let parsed;
  try { parsed = EXTERNAL_PARSERS.plane(await readPlaneImport(input)); } catch (error) { parsed = { error: error.message }; }
  fs.writeFileSync(path.join(here, `plane-${name}.parsed.golden`), `${JSON.stringify(parsed, null, 2)}\n`);
  if (!parsed.error) people[`plane-${name}`] = importedPeople(parsed.tasks);
}
fs.writeFileSync(path.join(here, 'plane-people.golden'), `${JSON.stringify(people, null, 2)}\n`);
