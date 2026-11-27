document.addEventListener('change', (event) => {
  const field = event.target;
  const picker = field.closest('.js-webhook-activities');
  if (!picker) return;
  picker.dataset.changed = 'true';
  const all = picker.querySelector('[name="allActivities"]');
  const groups = [...picker.querySelectorAll('[name="activityGroup"]')];
  if (field === all && all.checked) groups.forEach(group => { group.checked = false; });
  if (field.name === 'activityGroup') all.checked = !groups.some(group => group.checked);
});

document.addEventListener('submit', async (event) => {
  const form = event.target.closest('.js-board-hook');
  if (!form) return;
  event.preventDefault();

  const page = form.closest('.board-webhooks');
  const error = page.querySelector('.js-webhook-error');
  const fields = new FormData(form);
  const id = form.dataset.hook;
  const removing = event.submitter?.value === 'delete';
  const method = removing ? 'DELETE' : id ? 'PUT' : 'POST';
  const endpoint = page.dataset.integrations + (id ? '/' + encodeURIComponent(id) : '');
  const update = { url: fields.get('url'), title: fields.get('title'), type: fields.get('type') };
  if (fields.get('token')) update.token = fields.get('token');
  if (id && !removing) {
    update.enabled = fields.has('enabled');
    if (form.querySelector('.js-webhook-activities[data-changed]')) {
      const groups = fields.getAll('activityGroup');
      update.activities = fields.has('allActivities') || groups.length === 0
        ? ['all']
        : [...new Set(groups.flatMap(group => group.split(' ')))];
    }
  }

  error.hidden = true;
  const buttons = form.querySelectorAll('button');
  buttons.forEach(button => { button.disabled = true; });
  try {
    const response = await fetch(endpoint, {
      method,
      headers: { 'Content-Type': 'application/json' },
      body: removing ? undefined : JSON.stringify(update),
    });
    if (!response.ok) throw new Error();
    window.location.reload();
  } catch {
    error.hidden = false;
    buttons.forEach(button => { button.disabled = false; });
  }
});
