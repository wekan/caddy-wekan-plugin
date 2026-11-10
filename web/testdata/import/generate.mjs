// Writes plan.golden from plan-input.json with Node.js WeKan's own
// models/lib/importedTaskPlan.js, which TestImportPlanIsWeKans compares
// Weddy's with:  node web/testdata/import/generate.mjs /path/to/wekan
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const wekan = process.argv[2];
if (!wekan) throw new Error('usage: generate.mjs /path/to/wekan');
const here = path.dirname(new URL(import.meta.url).pathname);
const input = JSON.parse(fs.readFileSync(path.join(here, 'plan-input.json'), 'utf8'));
const plan = await import(pathToFileURL(path.join(wekan, 'models/lib/importedTaskPlan.js')).href);
const fields = plan.planImportedCustomFields(input.tasks);
const out = {
  tasks: input.tasks.map(task => plan.planImportedTask(task, input)),
  labels: plan.planImportedLabels(input.tasks, input.allowedColors),
  customFields: fields,
  customFieldValues: input.tasks.map(task => plan.importedCustomFieldValues(task, fields.fields)),
  links: plan.planImportedLinks(input.tasks),
};
fs.writeFileSync(path.join(here, 'plan.golden'), `${JSON.stringify(out, null, 2)}\n`);
