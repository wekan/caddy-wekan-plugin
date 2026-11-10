// Writes <source>-<input>.parsed.golden: Node.js WeKan's own parser
// (models/lib/externalParsers.js) given each input, which
// TestImportParsersAreWeKans compares Weddy's with:
//   TZ=UTC node web/testdata/import/generate-parsers.mjs /path/to/wekan
// (at UTC, as WeKan's server runs: V8 reads some dates in the local zone)
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate-parsers.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const { EXTERNAL_PARSERS: SHARED } = await import(pathToFileURL(path.join(wekan, 'models/lib/externalParsers.js')).href);
const TEXT_SOURCES = ['markdown', 'todotxt', 'taskwarrior', 'nullboard', 'opml', 'leo', 'obsidian', 'orgmode', 'todoist', 'meistertask', 'linear', 'ticktick', 'clickup', 'pivotal', 'redmine', 'quire', 'focalboard', 'superproductivity', 'vikunja'];
const EXTERNAL_PARSERS = { ...SHARED };
// The server-only parsers, not in EXTERNAL_PARSERS: their XML parser stays
// out of the client bundle.
const lib = name => pathToFileURL(path.join(wekan, 'models/lib', name)).href;
EXTERNAL_PARSERS.opml = (await import(lib('opmlOutline.js'))).parseOpml;
EXTERNAL_PARSERS.leo = (await import(lib('leoOutline.js'))).parseLeo;
const { importedPeople } = await import(lib('importMembersMode.js'));
// people.golden: the people WeKan's import makes users of, for each parsed
// file (TestImportPeopleAreWeKans).
const people = {};
const cases = JSON.parse(fs.readFileSync(path.join(here, 'parser-cases.json'), 'utf8'));
for (const [source, inputs] of Object.entries(cases)) {
  for (const input of inputs) {
    const text = fs.readFileSync(path.join(here, input), 'utf8');
    // A text source's parser reads the text; every other reads its JSON.
    const parsed = EXTERNAL_PARSERS[source](TEXT_SOURCES.includes(source) ? text : JSON.parse(text));
    const name = path.basename(input).replace(/\.[^.]+$/, '');
    fs.writeFileSync(path.join(here, `${source}-${name}.parsed.golden`), `${JSON.stringify(parsed, null, 2)}\n`);
    people[`${source}-${name}`] = importedPeople(parsed.tasks);
  }
}
fs.writeFileSync(path.join(here, 'people.golden'), `${JSON.stringify(people, null, 2)}\n`);
