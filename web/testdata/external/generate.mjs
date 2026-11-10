// Writes <format>.golden from collected.json with Node.js WeKan's own
// formatters, which TestExternalFormatsAreWeKans compares Weddy's with:
//   node web/testdata/external/generate.mjs /path/to/wekan
// A text format is the formatter's string; a JSON document is what WeKan's
// sendJsonResult writes, JSON.stringify of it.
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const collected = JSON.parse(fs.readFileSync(path.join(here, 'collected.json'), 'utf8'));
const { formatters } = await import(pathToFileURL(path.join(wekan, 'models/lib/externalExportFormatters.js')).href);
// TickTick's backup is dated: at this fixed time, as the Go test gives it.
const ticktickNow = new Date('2026-11-10T12:00:00.000Z');
const { formatTickTickCsv } = await import(pathToFileURL(path.join(wekan, 'models/lib/ticktickCsvFormat.js')).href);
fs.writeFileSync(path.join(here, 'ticktick.golden'), formatTickTickCsv(structuredClone(collected), ticktickNow));
const { formatFocalboard } = await import(pathToFileURL(path.join(wekan, 'models/lib/focalboardFormat.js')).href);
fs.writeFileSync(path.join(here, 'focalboard.golden'), formatFocalboard(structuredClone(collected), { now: ticktickNow.getTime() }));
const { formatTasksOrgBackup } = await import(pathToFileURL(path.join(wekan, 'models/lib/tasksorgFormat.js')).href);
fs.writeFileSync(path.join(here, 'tasksorg.golden'), JSON.stringify(formatTasksOrgBackup(structuredClone(collected), ticktickNow)));
const { formatSuperProductivity } = await import(pathToFileURL(path.join(wekan, 'models/lib/superProductivityFormat.js')).href);
fs.writeFileSync(path.join(here, 'superproductivity.golden'), formatSuperProductivity(structuredClone(collected), { now: ticktickNow.getTime() }));
for (const format of ['markdown', 'opml', 'todotxt', 'taskwarrior', 'zenkit', 'nullboard', 'obsidian', 'orgmode', 'kanri',
  'deck', 'kanboard', 'openproject', 'github', 'gitea', 'forgejo', 'gitlab', 'trello', 'asana',
  'leo', 'meistertask', 'todoist', 'linear', 'clickup', 'pivotal', 'quire', 'redmine', 'notion']) {
  const out = formatters[format](structuredClone(collected));
  fs.writeFileSync(path.join(here, `${format}.golden`), typeof out === 'string' ? out : JSON.stringify(out));
}
