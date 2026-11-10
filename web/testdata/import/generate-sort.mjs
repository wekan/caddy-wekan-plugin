// Writes sort.golden: arrays of { id, key } sorted by V8's Array.prototype.sort
// with (a, b) => Number(a.key) - Number(b.key), where a null key is NaN - the
// inconsistent comparator WeKan's Focalboard import gives content blocks
// without a createAt. TestJSSort compares Weddy's jsSort with it:
//   node web/testdata/import/generate-sort.mjs
import fs from 'node:fs';
import path from 'node:path';

const here = path.dirname(new URL(import.meta.url).pathname);
let seed = 7;
const random = () => { seed = (seed * 1103515245 + 12345) % 2147483648; return seed / 2147483648; };
const cases = [];
for (let n = 0; n < 600; n += 1) {
  const length = 1 + Math.floor(random() * 63);
  const input = Array.from({ length }, (_, id) => ({ id, key: random() < 0.3 ? null : Math.floor(random() * 10) }));
  const sorted = [...input].sort((a, b) => (a.key === null ? NaN : a.key) - (b.key === null ? NaN : b.key));
  cases.push({ input: input.map(e => e.key), output: sorted.map(e => e.id) });
}
fs.writeFileSync(path.join(here, 'sort.golden'), `${JSON.stringify(cases)}\n`);
