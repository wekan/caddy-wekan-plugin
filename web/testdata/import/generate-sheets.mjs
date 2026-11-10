// Writes sheets/sheets.xlsx and sheets/sheets-none.xlsx with Node.js WeKan's
// ExcelJS, and sheets.golden: the boards WeKan's Excel import finds in each
// (models/lib/csvImportMapping.js excelSheetsToBoards over worksheetRows,
// with every language's header names as server/lib/importHeaderNames.js
// gathers them) and the mapping it guesses for each board's header -
// which TestExcelSheetBoardsAreWeKans compares Weddy's with:
//   TZ=UTC node web/testdata/import/generate-sheets.mjs /path/to/wekan
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate-sheets.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const require = createRequire(path.join(wekan, 'server/lib/plannerWorkbook.js'));
const ExcelJS = require('@wekanteam/exceljs');
const { worksheetRows, excelSheetsToBoards, guessCsvMapping, pickHeaderTranslations } = await import(
  pathToFileURL(path.join(wekan, 'models/lib/csvImportMapping.js')).href);

// importHeaderNames: languages.js's entries in order, each its file's names.
const languagesJs = fs.readFileSync(path.join(wekan, 'imports/i18n/languages.js'), 'utf8');
const files = Object.fromEntries([...languagesJs.matchAll(/"([^"]+)": \(\) => import\("\.\/data\/([^"/]+\.i18n\.json)"\)/g)]
  .map(m => [m[1], m[2]]));
const translations = {};
for (const [, key] of languagesJs.matchAll(/^ {2}\["([^"]+)", "[^"]*", "[^"]+", /gm)) {
  if (!files[key]) continue;
  translations[key] = pickHeaderTranslations(JSON.parse(fs.readFileSync(path.join(wekan, 'imports/i18n/data', files[key]), 'utf8')));
}
const fi = JSON.parse(fs.readFileSync(path.join(wekan, 'imports/i18n/data/fi.i18n.json'), 'utf8'));

const workbook = new ExcelJS.Workbook();
// A WeKan Excel export's shape: the title in A1, the board's details above
// the table, the header in row 7 - here in Finnish.
const one = workbook.addWorksheet('Board One');
one.getCell('A1').value = '  Board One title  ';
one.mergeCells('A1:H1');
one.getCell('A3').value = fi.description;
one.getCell('B3').value = 'What the board is for';
one.mergeCells('B3:H3');
one.getCell('A5').value = fi.members;
one.getCell('F5').value = 'ann bob';
one.mergeCells('F5:H5');
one.getRow(7).values = [fi.number, fi.title, fi.description, fi.list, fi.swimlane, fi['card-due'], fi.labels, 'CustomField-Size-text'];
one.getRow(8).values = [1, { richText: [{ text: 'Rich ' }, { text: 'card' }] }, 'First', 'To do', 'Default', new Date(Date.UTC(2026, 2, 4, 5, 6, 7)), 'red', 'L'];
one.getRow(8).getCell(6).numFmt = 'yyyy-mm-dd hh:mm';
one.getRow(10).values = [2, { text: 'Linked', hyperlink: 'https://x.test/' }, { formula: 'C8&"!"', result: 'First!' }, 'Done', '', 45000.5, '', ''];
one.getRow(10).getCell(6).numFmt = 'yyyy-mm-dd';
one.getRow(11).values = [3, true, { error: '#N/A' }, 1.25, -0, 1e21];
// WeKan's Activity sheet: skipped.
const activity = workbook.addWorksheet('Activity');
activity.addRow(['Activity', 'Card', 'Date']);
activity.addRow(['Moved', 'Rich card', new Date(Date.UTC(2026, 2, 5))]);
// A second board, its header in row 1, in English.
const two = workbook.addWorksheet('Second');
two.addRow(['Title', 'Description', 'List', 'Labels']);
two.addRow(['Two A', 'Text', 'Doing', 'blue']);
two.addRow([]);
two.addRow(['Two B', '', 'Doing', '']);
// Notes: a stray "Title" below row 1 is not a header.
const notes = workbook.addWorksheet('Notes');
notes.addRow(['Some notes about this workbook']);
notes.addRow([]);
notes.addRow(['Title', 'of nothing']);
workbook.addWorksheet('Empty');
// A board with other columns, by the names other tools use.
const other = workbook.addWorksheet('Other columns');
other.addRow(['Name', 'Status', 'Due date', 'Assigned to', 'Tags']);
other.addRow(['Three', 'Backlog', '2026-04-01', 'ann', 'x']);
// A board with the same header as Board One, its own title in A1.
const same = workbook.addWorksheet('Same header');
same.getCell('A1').value = 'Same header board';
same.getRow(7).values = [fi.number, fi.title, fi.description, fi.list, fi.swimlane, fi['card-due'], fi.labels, 'CustomField-Size-text'];
same.getRow(8).values = [1, 'Copied', '', 'To do', 'Default', '', '', 'S'];
fs.writeFileSync(path.join(here, 'sheets', 'sheets.xlsx'), Buffer.from(await workbook.xlsx.writeBuffer()));

// No sheet with a header: the first sheet with a value, from its first row.
const none = new ExcelJS.Workbook();
none.addWorksheet('Blank');
const plain = none.addWorksheet('Plain');
plain.addRow([]);
plain.addRow(['alpha', 'beta']);
plain.addRow(['gamma', 'delta']);
const later = none.addWorksheet('Later');
later.addRow(['epsilon']);
fs.writeFileSync(path.join(here, 'sheets', 'sheets-none.xlsx'), Buffer.from(await none.xlsx.writeBuffer()));

const golden = {};
for (const name of ['sheets.xlsx', 'sheets-none.xlsx']) {
  const read = new ExcelJS.Workbook();
  await read.xlsx.load(fs.readFileSync(path.join(here, 'sheets', name)));
  const sheets = read.worksheets.map(worksheet => ({ name: worksheet.name, rows: worksheetRows(worksheet) }));
  const result = excelSheetsToBoards(sheets, translations);
  golden[name] = {
    ...result,
    mappings: result.boards.map(board => guessCsvMapping(board.rows[0], translations)),
  };
}
// Headers on their own: WeKan's own CSV and Excel layouts read by position
// (exportLayoutOf) in English, Finnish and Latvian - which translates two of
// the keys with one word - and headers where a Number column must not be
// taken for the title.
const csvLayout = ['title', 'description', 'list', 'swimlane', 'owner', 'requested-by', 'assigned-by', 'members',
  'assignee', 'labels', 'card-start', 'card-due', 'card-end', 'overtime-hours', 'spent-time-hours', 'createdAt',
  'last-modified-at', 'last-activity', 'voting', 'archived'];
const excelLayout = ['number', 'title', 'description', 'parent-card', 'owner', 'createdAt', 'last-modified-at',
  'card-received', 'card-start', 'card-due', 'card-end', 'list', 'swimlane', 'assignee', 'members',
  'requested-by', 'assigned-by', 'labels', 'overtime-hours', 'spent-time-hours'];
const data = lang => JSON.parse(fs.readFileSync(path.join(wekan, 'imports/i18n/data', `${lang}.i18n.json`), 'utf8'));
const headers = [];
for (const lang of ['en', 'fi', 'lv', 'de']) {
  const t = data(lang);
  headers.push(csvLayout.map(k => t[k]), excelLayout.map(k => t[k]), [...excelLayout.map(k => t[k]), 'CustomField-Size-text'],
    csvLayout.map(k => t[k]).reverse());
}
headers.push([data('fi').number, 'Something', data('fi').list], ['Number', 'Voting', 'Task'], ['  Due\u00a0Date: ', '\u00a0Name\u00a0'],
  ['İSSUE', 'Status'], ['', 'customfield-x', 'Card name']);
golden.headers = headers.map(header => ({ header, mapping: guessCsvMapping(header, translations) }));
fs.writeFileSync(path.join(here, 'sheets.golden'), `${JSON.stringify(golden, null, 2)}\n`);
