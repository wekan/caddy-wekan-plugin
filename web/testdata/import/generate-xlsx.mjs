// Writes xlsx/values.xlsx with Node.js WeKan's ExcelJS (@wekanteam/exceljs)
// and xlsx-values.golden: the cell values ExcelJS reads back from it, as
// WeKan's workbook readers (server/lib/*Workbook.js) hand them to the
// parsers - holes, strings, numbers, booleans, Dates, rich text, hyperlinks,
// formulas and errors - which TestXlsxValuesAreExcelJS compares Weddy's
// typed reader with:
//   TZ=UTC node web/testdata/import/generate-xlsx.mjs /path/to/wekan
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate-xlsx.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const require = createRequire(path.join(wekan, 'server/lib/plannerWorkbook.js'));
const ExcelJS = require('@wekanteam/exceljs');

const workbook = new ExcelJS.Workbook();
const values = workbook.addWorksheet('Values');
values.addRow(['text', '  padded  ', 42, -0.5, 1e21, true, false]);
const dates = values.addRow([new Date(Date.UTC(2026, 9, 10)), new Date(Date.UTC(2026, 9, 10, 14, 30, 15, 250)), 45000.5, 0.75, 3.5, 1234.5678]);
dates.getCell(1).numFmt = 'yyyy-mm-dd';
dates.getCell(2).numFmt = 'dd/mm/yyyy hh:mm:ss';
dates.getCell(3).numFmt = '[h]:mm:ss';
dates.getCell(4).numFmt = 'mm:ss';
dates.getCell(5).numFmt = '0.00';
dates.getCell(6).numFmt = '"Dated" #,##0.00';
values.addRow([{ richText: [{ text: 'Rich ', font: { bold: true } }, { text: 'text' }] },
  { text: 'A link', hyperlink: 'https://x.test/a?b=1' },
  { formula: 'C1*2', result: 84 },
  { formula: 'A1&"!"', result: 'text!' },
  { formula: 'A2+1', result: new Date(Date.UTC(2026, 9, 11)) },
  { error: '#N/A' },
  { formula: 'NOW()' }]);
values.getRow(3).getCell(5).numFmt = 'd-mmm-yy';
const gaps = values.getRow(5);
gaps.getCell(2).value = 'second';
gaps.getCell(5).value = 'fifth';
gaps.getCell(3).numFmt = '0.00'; // styled, no value
values.getRow(7).getCell(1).value = { sharedFormula: 'C3', result: 11 };
values.getRow(8).getCell(1).value = '';
values.getRow(8).getCell(2).value = null;
values.getRow(9).getCell(4).value = 'only D9';
const tasks = workbook.addWorksheet('Tasks');
tasks.addRow(['Task Name', 'Bucket Name']);
tasks.addRow(['One', 'To do']);
fs.writeFileSync(path.join(here, 'xlsx', 'values.xlsx'), Buffer.from(await workbook.xlsx.writeBuffer()));

// Read back as WeKan's readers do.
const read = new ExcelJS.Workbook();
await read.xlsx.load(fs.readFileSync(path.join(here, 'xlsx', 'values.xlsx')));
const plain = value => {
  if (value === undefined) return { hole: true };
  if (value instanceof Date) return { date: Number.isNaN(value.getTime()) ? null : value.toISOString() };
  if (value && typeof value === 'object') {
    return Object.fromEntries(Object.entries(value).filter(([k]) => k !== 'font').map(([k, v]) => [k, k === 'richText' ? v.map(p => ({ text: p.text })) : plain(v)]));
  }
  return value;
};
const out = read.worksheets.map(sheet => {
  const rows = [];
  sheet.eachRow((row, number) => {
    while (rows.length < number - 1) rows.push([]);
    rows.push(Array.from(row.values.slice(1), plain));
  });
  return { name: sheet.name, rows };
});
fs.writeFileSync(path.join(here, 'xlsx-values.golden'), `${JSON.stringify(out, null, 2)}\n`);

// The workbook imports: xlsx/<source>-<name>.xlsx, written by WeKan's own
// workbook writers from Weddy's exports (testdata/external) or built cell by
// cell for the edges, and <source>-<name>.parsed.golden, WeKan's reader and
// parser on it - which TestImportWorkbooksAreWeKans compares Weddy's with.
const { pathToFileURL } = await import('node:url');
const { EXTERNAL_PARSERS } = await import(pathToFileURL(path.join(wekan, 'models/lib/externalParsers.js')).href);
const lib = name => require(path.join(wekan, 'server/lib', name));
const external = name => JSON.parse(fs.readFileSync(path.join(here, '..', 'external', `${name}.golden`), 'utf8'));
const book = async build => { const wb = new ExcelJS.Workbook(); await build(wb); return Buffer.from(await wb.xlsx.writeBuffer()); };
const WORKBOOKS = {
  planner: {
    read: buffer => lib('plannerWorkbook.js').readPlannerWorkbook(buffer.toString('base64')),
    files: {
      planner: () => lib('plannerWorkbook.js').writePlannerWorkbook(external('planner')),
      // A real "Export plan to Excel" workbook, WeKan's fixture.
      real: () => fs.readFileSync(path.join(wekan, 'tests/fixtures/planner/plannr-test_plan.xlsx')),
      edge: () => book(wb => {
        wb.addWorksheet('Other').addRow(['not this sheet']);
        const s = wb.addWorksheet('Tasks');
        s.addRow(['Plan name', 'Edge plan']);
        s.addRow(['Plan ID', 42]);
        s.addRow([]);
        s.addRow(['Task ID', 'Task Name', 'Bucket Name', 'Progress', 'Priority', 'Assigned To', 'Created By', 'Created Date',
          'Start Date', 'Due Date', 'Late', 'Completed Date', 'Completed By', 'Description', 'Completed Checklist Items',
          'Checklist Items', 'Labels', 'Is Recurring']);
        const r1 = s.addRow(['t1', { richText: [{ text: 'Order ' }, { text: 'valves' }] }, 'To do', 'In progress', 'Urgent',
          'Ann; Bob ;', ' Carl ', new Date(Date.UTC(2026, 8, 1, 12)), '13.10.2026', '2026-10-20', 'false', '', '', 'Two of them',
          '1/3', 'Measure; Call;;Pay', 'shop; urgent', 'Yes']);
        r1.getCell(8).numFmt = 'yyyy-mm-dd hh:mm';
        s.addRow(['t2', { text: 'Linked', hyperlink: 'https://x.test' }, '', 'Done?', 'Whatever', '', '', '2/30/2026',
          '10/13/2026', { formula: 'TODAY()', result: new Date(Date.UTC(2026, 9, 1)) }, '', '2026-10-05T10:00:00Z', 'Dee']);
        s.getRow(6).getCell(10).numFmt = 'dd/mm/yyyy';
        s.addRow([]);
        s.addRow(['t3', '', 'Bucket']);
        s.addRow(['t4', 42, 'Bucket', 'Completed', 'Low', 'Eve', '', '', '', '13/13/2026', '', '', '', '', '0/2', 'a;b']);
      }),
    },
  },
  monday: {
    read: buffer => lib('mondayWorkbook.js').readMondayWorkbook(buffer.toString('base64')),
    files: {
      monday: () => lib('mondayWorkbook.js').writeMondayWorkbook(external('monday')),
      edge: () => book(wb => {
        const s = wb.addWorksheet('Edge board sheet');
        s.addRow(['Garden board']);
        s.addRow([]);
        s.addRow(['Spring']);
        s.addRow(['Name', 'Person', 'Status', 'Date', 'Timeline', 'Tags', 'Item ID', 'Notes', 'Budget', 'Ratio', 'Link']);
        const r = s.addRow(['Order valves', 'Ann, Bob, ', 'Working on it', new Date(Date.UTC(2026, 9, 10)), '2026-10-01 - 2026-10-20',
          'shop, urgent,', 101, 'Two of them', '1200', '-0.5', { text: 'site', hyperlink: 'https://x.test' }]);
        r.getCell(4).numFmt = 'yyyy-mm-dd';
        s.addRow(['', 'Subitems', 'Owner', 'Status', 'Date']);
        s.addRow(['', 'Measure', 'Cy', 'Done', '2026-02-30']);
        s.addRow(['', 'Call', ',', '', '10/May/2026 03:26:42 PM']);
        s.addRow(['', '', '', '3 items']);
        s.addRow(['Fit', '', 'Stuck', 'tomorrow', '2026-10-01 to 2026-10-02', '', 102]);
        s.addRow([]);
        s.addRow(['Summer']);
        s.addRow(['Name', 'Group', 'Due Date']);
        s.addRow(['Water', 'Lane X', '2026-07-01']);
        s.addRow(['', 'Subitems', 'Status']);
        s.addRow(['', 'Orphan', 'x']);
        const u = wb.addWorksheet('Edge board sheet-updates');
        u.addRow(['Item ID', 'Item Name', 'User', 'Created At', 'Update Content']);
        u.addRow([101, 'Order valves', 'Dee', '27/May/2025 03:26:42 PM', 'Ordered']);
        u.addRow(['', 'Water', '', 'bad date', 'Watered']);
        u.addRow(['999', 'Nothing', 'Eve', '', 'Lost update']);
        u.addRow(['', 'Fit', '', '', '']);
      }),
      'no-header': () => book(wb => { wb.addWorksheet('S').addRow(['Title', 'Status']); }),
    },
  },
  wrike: {
    read: buffer => lib('wrikeWorkbook.js').readWrikeWorkbook(buffer.toString('base64')),
    files: {
      wrike: () => lib('wrikeWorkbook.js').writeWrikeWorkbook(external('wrike')),
      edge: () => book(wb => {
        const s = wb.addWorksheet('Tasks');
        s.addRow(['Wrike import template']);
        s.addRow(['Key', 'Parent Task', 'Title', 'Workflow', 'Status', 'Custom Status', 'Priority', 'Assigned To', 'Start Date',
          'Duration', 'End Date', 'Depends On', 'Start Date Constraint', 'Description', 'Default task workflow', 'Budget', 'constructor', 'Ratio']);
        s.addRow([1, '', '/Garden/ Beds /', '', 'Active', '', '', 'Ann', '', '', '', '', '', 'folder text']);
        s.addRow([2, 1, 'Order valves', 'Garden flow', 'Completed', 'In review', 'High', 'Ann Lee <ann@x.test>, <bob@x.test>, , Cy', 46305,
          '3d', '2026-10-20T00:00:00.000Z', '3FS; 4 ss, 1, x9', '2026-10-01', 'Two of them', 'Default', '1200', 'proto', '-0.25']);
        const r = s.addRow([3, 2, 'Fit', 'Garden flow', 'Deferred', 'Waiting', '', '', new Date(Date.UTC(2026, 9, 3)), '', '2026-02-30']);
        r.getCell(9).numFmt = 'dd.mm.yyyy';
        s.addRow([3, '', 'Fit again']);
        s.addRow(['', '', 'No key', '', '', '', '', '', '9999999']);
        s.addRow(['abc', '', 'Bad key', 'Other flow']);
        s.addRow([]);
        s.addRow(['5', '', '']);
        s.addRow([6, '', '/Late folder/']);
        s.addRow([7, 6, 'In late folder', '', 'Cancelled', 'Cancelled']);
      }),
    },
  },
  teamwork: {
    read: buffer => lib('teamworkWorkbook.js').readTeamworkWorkbook(buffer.toString('base64')),
    files: {
      teamwork: () => lib('teamworkWorkbook.js').writeTeamworkWorkbook(external('teamwork')),
      edge: () => book(wb => {
        const s = wb.addWorksheet('Garden tasks');
        s.addRow(['Teamwork import']);
        s.addRow(['Task list', 'Task', 'Description', 'Assign To', 'Start date', 'Due date', 'Priority', 'Estimated time', 'Tags', 'Status', 'Extra col']);
        const r = s.addRow(['Spring', 'Order valves', 'Two of them', 'Ann, Bob,', new Date(Date.UTC(2026, 9, 1, 15)), '13/10/2026', 'HIGH',
          new Date(Date.UTC(1899, 11, 30, 1, 30)), 'shop, urgent', 'Complete', 'x']);
        r.getCell(5).numFmt = 'yyyy-mm-dd';
        r.getCell(8).numFmt = 'hh:mm';
        s.addRow(['', '- Measure', '', '', '', '', 'Low', 25.6, '', 'Active']);
        s.addRow(['Other', '-- Deep', '', '', '', '', '', '1h 15m']);
        s.addRow(['', '---- Too deep', '', '', '', '', '', '2 hours']);
        s.addRow(['', 'Fit', '', '', '2026-02-30', '', 'urgent', 'soon', '', 'Paused']);
        s.addRow(['Summer', '#']);
        s.addRow(['', 'Water', '', '', '', '01/02/2026', '', '01:45']);
        s.addRow([]);
        s.addRow(['', '', 'no task']);
      }),
      'orphan-sub': () => book(wb => {
        const s = wb.addWorksheet('Sheet1');
        s.addRow(['Tasklist', 'Task']);
        s.addRow(['L', '>Orphan']);
        s.addRow(['', 'Top', '', '', '', '', '', 0]);
      }),
    },
  },
  businessmap: {
    read: buffer => lib('businessmapWorkbook.js').readBusinessmapWorkbook(buffer.toString('base64')),
    files: {
      businessmap: () => lib('businessmapWorkbook.js').writeBusinessmapWorkbook(external('businessmap')),
      edge: () => book(wb => {
        wb.addWorksheet('Notes').addRow(['some', 'notes']);
        const s = wb.addWorksheet('Garden cards');
        s.addRow(['Exported from Businessmap']);
        s.addRow(['Card ID', 'Custom Card ID', 'Title', 'Description', 'Priority', 'Owner', 'Co-Owners', 'Color', 'Size', 'Tags',
          'Deadline', 'Template', 'Type Name', 'Column Name', 'Lane Name', 'Workflow Name', 'Board Name', 'Board ID', 'Comment 1',
          'Comments 2', 'Links', 'Parent Card ID', 'Created At', 'Archived At', 'Start Date', 'End Date', 'Planned Start', 'Track', 'Budget', '7']);
        const r = s.addRow([101, 'C-1', { richText: [{ text: ' Order ' }, { text: 'valves ' }] }, 'Two of them', 'High', 'ann', 'bob; cy', '#067DB7', '3',
          'shop urgent', new Date(Date.UTC(2026, 9, 10, 15)), 'Std', 'Task', 'Doing', 'Lane A', 'Flow', 'Garden', 9, 'Looks good', 'Second',
          'parent: 103; children: 102, 999; relatives: 102; predecessors: 104; successors: 102 105; siblings: 1', '103',
          '10/01/2026', '', '01-10-2026', '2026-02-30', 'Q4', 'T1', '1200', 'seven']);
        r.getCell(11).numFmt = 'yyyy-mm-dd';
        s.addRow([102, '', 'Child', '', 'urgent', '', 'dee,eve', 'blue', '-1', 'a,b', '1/1/1969', '', '', '', '', 'Flow2', 'Other board', '', '', '',
          'relatives: 101; predecessors: 101', '', { formula: 'TODAY()', result: new Date(Date.UTC(2026, 0, 2)) }, '2026-01-05']);
        s.getRow(4).getCell(23).numFmt = 'yyyy-mm-dd';
        s.addRow([103, '', '', '', '', '', '', '', '', '', '12/31/2037']);
        s.addRow(['', 'C-4', 'Custom only', '', '', '', '', '', 'x', '', '1/1/38', '', '', '', '', '', '', '', '', '', 'children: 102']);
        s.addRow(['', '', '']);
        s.addRow(['', '', '', 'no title or id']);
        s.addRow([105, '', 'Last', '', '', '', '', '', '', '', '', '', '', '', '', '', '', '', '', '', 'parents: 101; parents: 103']);
      }),
      'no-header': () => book(wb => { wb.addWorksheet('Export').addRow(['Name', 'State']); }),
    },
  },
};
const { importedPeople } = await import(pathToFileURL(path.join(wekan, 'models/lib/importMembersMode.js')).href);
const people = {};
for (const [source, { read, files }] of Object.entries(WORKBOOKS)) {
  for (const [name, make] of Object.entries(files)) {
    const buffer = Buffer.from(await make());
    fs.writeFileSync(path.join(here, 'xlsx', `${source}-${name}.xlsx`), buffer);
    let parsed;
    try { parsed = EXTERNAL_PARSERS[source](await read(buffer)); } catch (error) { parsed = { error: error.message }; }
    if (parsed && !parsed.error) { delete parsed.scrumTransfer; delete parsed.scrumLosses; }
    fs.writeFileSync(path.join(here, `${source}-${name}.parsed.golden`), `${JSON.stringify(parsed, null, 2)}\n`);
    if (!parsed.error) people[`${source}-${name}`] = importedPeople(parsed.tasks);
  }
}
// workbook-people.golden: the people WeKan's import makes users of, for each
// workbook (TestImportPeopleAreWeKans).
fs.writeFileSync(path.join(here, 'workbook-people.golden'), `${JSON.stringify(people, null, 2)}\n`);
