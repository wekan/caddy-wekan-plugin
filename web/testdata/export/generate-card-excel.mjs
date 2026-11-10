// Writes card-excel.golden: Node.js WeKan's renderCardDocumentExcel
// (models/server/renderCardDocumentExcel.js) drawing each document of
// card-documents.golden onto an ExcelJS worksheet from row 3 - every cell's
// value and style, the merged ranges, the row heights and the page breaks -
// which TestCardExcelIsWeKans compares Weddy's renderer with:
//   node web/testdata/export/generate-card-excel.mjs /path/to/wekan
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate-card-excel.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const require = createRequire(path.join(wekan, 'server/lib/plannerWorkbook.js'));
const ExcelJS = require('@wekanteam/exceljs');
const { renderCardDocumentExcel } = await import(path.join(wekan, 'models/server/renderCardDocumentExcel.js'));
const revive = v => {
  if (Array.isArray(v)) return v.map(revive);
  if (v && typeof v === 'object') return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, revive(x)]));
  // A meta pair's third value is the date of the cell.
  return typeof v === 'string' && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$/.test(v) ? new Date(v) : v;
};
const documents = JSON.parse(fs.readFileSync(path.join(here, 'card-documents.golden'), 'utf8'));
const plain = v => JSON.parse(JSON.stringify(v === undefined ? null : v));
const golden = {};
for (const [name, document] of Object.entries(documents)) {
  // Only a pair's third value is a date: the texts stay text.
  const doc = document.map(block => block.type === 'meta'
    ? { ...block, pairs: block.pairs.map(p => (p.length > 2 ? [p[0], p[1], new Date(p[2])] : p)) } : block);
  const workbook = new ExcelJS.Workbook();
  const ws = workbook.addWorksheet('Card');
  const result = await renderCardDocumentExcel(ws, workbook, 3, doc, {
    fontName: 'Arial', progressColor: '2E7D32',
    attachmentHeadings: ['#', 'Name', 'Size', 'Type', 'Uploaded at', 'Uploaded by'],
  });
  const cells = {};
  const heights = {};
  ws.eachRow({ includeEmpty: true }, (row, number) => {
    if (row.height !== undefined) heights[number] = row.height;
    row.eachCell({ includeEmpty: true }, cell => {
      if (cell.isMerged && cell.master.address !== cell.address) return;
      const value = cell.value instanceof Date ? { date: cell.value.toISOString() } : cell.value;
      const style = {};
      for (const k of ['font', 'fill', 'border', 'alignment', 'numFmt']) if (cell.style && cell.style[k] !== undefined) style[k] = cell.style[k];
      cells[cell.address] = plain({ value, ...style });
    });
  });
  golden[name] = { cells, merges: ws.model.merges.slice().sort(), heights, row: result.row, pageBreakRows: result.pageBreakRows };
}
fs.writeFileSync(path.join(here, 'card-excel.golden'), `${JSON.stringify(golden, null, 1)}\n`);
