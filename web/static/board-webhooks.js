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
  const update = { url: fields.get('url') };
  if (id && !removing) {
    update.title = fields.get('title');
    update.enabled = fields.has('enabled');
    if (fields.get('token')) update.token = fields.get('token');
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
