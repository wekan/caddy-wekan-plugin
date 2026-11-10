// Writes pdf.golden: Node.js WeKan's PDF (models/lib/pdfDocument.js) of each
// card document in card-documents.golden and of pdf-extra documents - the
// lines documentToLines makes and the bytes buildPdfBuffer writes - with
// encodeWinAnsi, flattenMarkdown and wrapTextBlock cases and a PNG's pixels,
// which TestPdfDocumentIsWeKans compares Weddy's with:
//   node web/testdata/export/generate-pdf.mjs /path/to/wekan
import fs from 'node:fs';
import path from 'node:path';
import zlib from 'node:zlib';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate-pdf.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const pdf = await import(path.join(wekan, 'models/lib/pdfDocument.js'));
const docs = JSON.parse(fs.readFileSync(path.join(here, 'card-documents.golden'), 'utf8'));
// A JPEG: SOI, a SOF0 saying 32 x 16, EOI - the PDF embeds its bytes as they are.
const jpeg = Buffer.from([0xFF, 0xD8, 0xFF, 0xC0, 0x00, 0x11, 0x08, 0x00, 0x10, 0x00, 0x20, 0x03, 0x01, 0x22, 0x00, 0x02,
  0x11, 0x01, 0x03, 0x11, 0x01, 0xFF, 0xD9, 0x00, 0x00, 0x00, 0x00, 0x00]);
const extra = JSON.parse(fs.readFileSync(path.join(here, 'pdf-extra.json'), 'utf8'));
for (const [name, doc] of Object.entries(extra)) {
  docs[name] = doc.map(b => (b.type === 'images' ? { ...b, images: b.images.map(im => ({ ...im, data: jpeg })) } : b));
}
const headings = ['#', 'Name', 'Size', 'Type', 'Uploaded at', 'Uploaded by'];
const strip = (key, value) => (key === 'data' ? undefined : value);
const golden = { documents: {}, encode: {}, flatten: {}, wrap: {} };
for (const [name, doc] of Object.entries(docs)) {
  const lines = pdf.documentToLines(doc, { attachmentHeadings: headings });
  golden.documents[name] = {
    lines: JSON.parse(JSON.stringify(lines, strip)),
    pdf: pdf.buildPdfBuffer(lines).toString('base64'),
  };
}
for (const s of ['Grüße ä ö ü €', 'ā ő ł đ ß æ ẞ ﬁ −', 'Привет 日本 😀', 'tab\there', 'a b​c﻿d', 'Ǆ ǅ', '“quotes” – — … •']) {
  golden.encode[s] = Buffer.from(pdf.encodeWinAnsi(s), 'latin1').toString('base64');
}
for (const s of ['**bold** *it* ***both*** `code` ~~gone~~', 'snake_case _it_ __b__ ___bi___', '# Head\n> quote\n* bullet\n---\n```js\ncode\n```',
  '![alt](http://x/i.png) [text](http://x "t") [](http://y)', '<b>html</b>\ttab  \nline', '**unclosed and * lone']) {
  golden.flatten[s] = pdf.flattenMarkdown(s);
}
const long = 'word '.repeat(30) + 'x'.repeat(120) + ' end';
for (const [s, indent] of [[long, ''], [long, '    '], ['Grüße 😀 '.repeat(20), '  '], ['', ''], ['a\n\nb', '']]) {
  golden.wrap[`${indent}|${s}`] = pdf.wrapTextBlock(s, indent);
}
golden.inline = {};
golden.rich = {};
for (const s of ['**bold with *italic* inside** and ***both***', 'snake_case _it_ __b__ x__y__z', '** spaced ** and `code` ~~s~~', '*a*b* unclosed *', '😀 *é*']) {
  golden.inline[s] = pdf.inlineRuns(s);
}
for (const [s, indent] of [['A **bold** word and ' + 'long '.repeat(25) + '*it* end', ''], ['# Head\n- item **b**\n' + 'y'.repeat(100), '  '], ['', '']]) {
  golden.rich[`${indent}|${s}`] = pdf.wrapRichTextBlock(s, indent);
}
// A PNG of 2 x 2 RGBA with every filter, its pixels as the PDF has them.
const raw = Buffer.from([1, 255, 0, 0, 255, 0, 255, 0, 128, 3, 0, 0, 255, 64, 255, 255, 255, 0]);
const ihdr = Buffer.from([0, 0, 0, 2, 0, 0, 0, 2, 8, 6, 0, 0, 0]);
const crc = buf => { let c, crcTable = []; for (let n = 0; n < 256; n++) { c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xEDB88320 ^ (c >>> 1) : c >>> 1; crcTable[n] = c >>> 0; } let x = 0xFFFFFFFF; for (const b of buf) x = crcTable[(x ^ b) & 0xFF] ^ (x >>> 8); return (x ^ 0xFFFFFFFF) >>> 0; };
const chunk = (type, data) => { const len = Buffer.alloc(4); len.writeUInt32BE(data.length); const td = Buffer.concat([Buffer.from(type), data]); const c = Buffer.alloc(4); c.writeUInt32BE(crc(td)); return Buffer.concat([len, td, c]); };
const png = Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
const image = pdf.preparePdfImage({ data: png, ext: 'png' });
golden.png = { file: png.toString('base64'), width: image.width, height: image.height, rgb: zlib.inflateSync(image.data).toString('base64') };
fs.writeFileSync(path.join(here, 'pdf.golden'), `${JSON.stringify(golden, null, 1)}\n`);
