// vaultd UI.
//
// Plain DOM, no framework and no build step (see web/embed.go). Everything is
// built with createElement and textContent rather than innerHTML: the data on
// these screens is target names, error strings and stderr tails from database
// clients, none of which vaultd controls.

'use strict';

const TOKEN_KEY = 'vaultd_token';

const app = document.getElementById('app');
const nav = document.querySelectorAll('[data-nav]');

// --- helpers --------------------------------------------------------------

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === null || value === undefined || value === false) continue;
    if (key === 'class') node.className = value;
    else if (key === 'text') node.textContent = value;
    else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, value);
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

function token() {
  try { return localStorage.getItem(TOKEN_KEY) || ''; } catch { return ''; }
}

function setToken(value) {
  try {
    if (value) localStorage.setItem(TOKEN_KEY, value);
    else localStorage.removeItem(TOKEN_KEY);
  } catch { /* private browsing: the session still works, it just won't persist */ }
}

// api performs one request, sending the token and turning an error response
// into an Error carrying the server's own message.
async function api(path, options) {
  const opts = Object.assign({ headers: {} }, options || {});
  const value = token();
  if (value) opts.headers['Authorization'] = 'Bearer ' + value;

  const response = await fetch(path, opts);
  if (response.status === 401) {
    setToken('');
    throw Object.assign(new Error('unauthorized'), { unauthorized: true });
  }

  const body = await response.json().catch(() => null);
  if (!response.ok && response.status !== 409) {
    throw new Error((body && body.error) || response.statusText || 'request failed');
  }
  return { status: response.status, body };
}

// --- formatting -----------------------------------------------------------

function bytes(n) {
  if (!n) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0, value = Number(n);
  while (value >= 1024 && i < units.length - 1) { value /= 1024; i++; }
  return (i === 0 ? value : value.toFixed(1)) + ' ' + units[i];
}

function duration(seconds) {
  if (seconds === null || seconds === undefined) return '—';
  const s = Math.max(0, Math.floor(seconds));
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm';
  if (s < 86400) return Math.floor(s / 3600) + 'h ' + Math.floor((s % 3600) / 60) + 'm';
  return Math.floor(s / 86400) + 'd ' + Math.floor((s % 86400) / 3600) + 'h';
}

function ago(iso) {
  if (!iso || iso.startsWith('0001-')) return '—';
  return duration((Date.now() - new Date(iso).getTime()) / 1000) + ' ago';
}

function when(iso) {
  if (!iso || iso.startsWith('0001-')) return '—';
  const d = new Date(iso);
  return d.toISOString().slice(0, 16).replace('T', ' ') + 'Z';
}

function ms(value) {
  if (!value) return '—';
  if (value < 1000) return Math.round(value) + 'ms';
  if (value < 60000) return (value / 1000).toFixed(1) + 's';
  return duration(value / 1000);
}

// --- shell ----------------------------------------------------------------

function setNav(name) {
  nav.forEach((a) => a.classList.toggle('on', a.dataset.nav === name));
}

function render(...nodes) {
  app.replaceChildren(...nodes.flat().filter(Boolean));
}

function heading(title, sub, right) {
  return el('div', { class: 'spread' },
    el('div', {},
      el('h1', { text: title }),
      sub ? el('p', { class: 'sub', text: sub }) : null),
    right || null);
}

function failure(err) {
  return el('div', { class: 'error', text: err.message || String(err) });
}

// gate asks for the token. It is shown whenever a request comes back 401,
// which is also what happens when a token is rotated under a browser that has
// the page open.
function gate(message) {
  setNav('');

  const input = el('input', {
    type: 'password',
    placeholder: 'server.auth.token',
    autocomplete: 'current-password',
  });
  const submit = () => { setToken(input.value.trim()); route(); };

  input.addEventListener('keydown', (e) => { if (e.key === 'Enter') submit(); });

  render(el('div', { class: 'gate' },
    el('h1', { text: 'vaultd' }),
    el('p', { class: 'sub', text: message || 'This vaultd needs its API token.' }),
    input,
    el('button', { class: 'primary', onclick: submit, text: 'Unlock' })));

  input.focus();
}

// --- overview -------------------------------------------------------------

async function viewOverview() {
  setNav('overview');

  const [{ body: targets }, status] = await Promise.all([
    api('/api/targets'),
    api('/api/status').catch(() => ({ body: { jobs: [] } })),
  ]);

  const nextByTarget = {};
  for (const job of (status.body && status.body.jobs) || []) {
    if (job.kind !== 'backup') continue;
    nextByTarget[job.target] = job.next;
  }

  const counts = { red: 0, amber: 0, green: 0, unknown: 0 };
  for (const t of targets) counts[t.health] = (counts[t.health] || 0) + 1;

  const summary = counts.red
    ? `${counts.red} target${counts.red > 1 ? 's need' : ' needs'} attention`
    : counts.amber
      ? `${counts.amber} target${counts.amber > 1 ? 's' : ''} with something to look at`
      : 'everything is backed up';

  render(
    heading('Overview', summary, el('div', { class: 'row' },
      el('span', { class: 'pill ok' }, el('span', { class: 'dot green' }), String(counts.green || 0)),
      el('span', { class: 'pill warn' }, el('span', { class: 'dot amber' }), String(counts.amber || 0)),
      el('span', { class: 'pill bad' }, el('span', { class: 'dot red' }), String(counts.red || 0)))),
    targets.length
      ? el('div', { class: 'grid' }, targets.map((t) => targetCard(t, nextByTarget[t.target || t.name])))
      : el('div', { class: 'empty', text: 'This config declares no targets.' }));
}

function targetCard(t, next) {
  return el('a', { class: 'card ' + t.health, href: '#/t/' + encodeURIComponent(t.name) },
    el('div', { class: 'top' },
      el('span', { class: 'dot ' + t.health }),
      el('span', { class: 'name', text: t.name }),
      el('span', { class: 'engine', text: t.engine })),
    el('div', { class: 'reason', text: t.error || t.reason }),
    el('div', { class: 'facts' },
      fact('last backup', ago(t.last_backup_at)),
      fact('size', bytes(t.bytes)),
      fact('kept', t.backups ? `${t.backups} (${bytes(t.total_bytes)})` : '—'),
      fact('next run', next ? when(next) : (t.schedule || 'manual')),
      fact('verify', verifyLabel(t)),
      fact('schedule', t.schedule || 'manual')));
}

function fact(k, v) {
  return el('div', { class: 'fact' },
    el('div', { class: 'k', text: k }),
    el('div', { class: 'v', text: v }));
}

function verifyLabel(t) {
  if (!t.verify_level) return 'off';
  if (t.verify_ok === true) return t.verify_level + ' ✓ ' + ago(t.verified_at);
  if (t.verify_ok === false) return t.verify_level + ' ✗ failed';
  return t.verify_level + ', never run';
}

// --- target ---------------------------------------------------------------

async function viewTarget(name) {
  setNav('overview');

  const { body: detail } = await api('/api/targets/' + encodeURIComponent(name));
  const entries = detail.entries || [];
  const succeeded = entries.filter((e) => e.outcome === 'succeeded');

  render(
    el('div', {}, el('a', { href: '#/', text: '← overview', class: 'muted' })),
    heading(detail.name, detail.error || detail.reason,
      el('span', { class: 'pill ' + pillClass(detail.health) },
        el('span', { class: 'dot ' + detail.health }), detail.health)),
    actionBar(detail),
    el('div', { id: 'run-panel' }),

    succeeded.length > 1 ? el('div', {},
      el('h2', { text: 'Size' }),
      el('div', { class: 'panel' }, sparkline(succeeded.slice().reverse().map((e) => e.bytes)))) : null,

    el('h2', { text: 'Backups' }),
    el('div', { class: 'panel' }, entries.length ? timeline(detail.name, entries)
      : el('div', { class: 'empty', text: 'Nothing has run yet.' })),

    detail.retention ? el('div', {},
      el('h2', { text: 'Retention, as it stands' }),
      el('div', { class: 'panel' }, retentionView(detail.retention))) : null);
}

function pillClass(health) {
  return health === 'green' ? 'ok' : health === 'amber' ? 'warn' : health === 'red' ? 'bad' : '';
}

function actionBar(detail) {
  const bar = el('div', { class: 'actions' });

  const backup = el('button', {
    class: 'primary',
    text: 'Back up now',
    onclick: () => startRun(detail.name, 'backup', backup),
  });
  bar.append(backup);

  if (detail.verify_level) {
    const verify = el('button', {
      text: 'Verify now (' + detail.verify_level + ')',
      onclick: () => startRun(detail.name, 'verify', verify),
    });
    bar.append(verify);
  }

  bar.append(el('button', { text: 'Prune…', onclick: () => previewPrune(detail.name) }));
  return bar;
}

// startRun kicks a manual run off and then polls it, which is the only way to
// watch something that can take hours.
async function startRun(name, kind, button) {
  const panel = document.getElementById('run-panel');
  button.disabled = true;

  try {
    const { status, body } = await api(`/api/targets/${encodeURIComponent(name)}/${kind}`, { method: 'POST' });
    if (status === 409) panel.replaceChildren(el('div', { class: 'error', text: 'A run is already going on this target.' }));
    await followRun(body.id, panel);
  } catch (err) {
    panel.replaceChildren(failure(err));
  } finally {
    button.disabled = false;
  }
}

async function followRun(id, panel) {
  for (;;) {
    const { body: run } = await api('/api/runs/' + encodeURIComponent(id));
    panel.replaceChildren(runPanel(run));
    if (run.state !== 'running') return;
    await new Promise((resolve) => setTimeout(resolve, 1500));
  }
}

function runPanel(run) {
  const state = run.state === 'succeeded' ? 'ok' : run.state === 'failed' ? 'bad' : run.state === 'running' ? '' : 'warn';

  return el('div', { class: 'panel' },
    el('div', { class: 'spread' },
      el('div', { class: 'row' },
        el('span', { class: 'pill ' + state, text: run.state }),
        el('strong', { text: run.kind }),
        el('span', { class: 'muted', text: 'started ' + when(run.started_at) })),
      run.finished_at ? el('span', { class: 'muted', text: ms(new Date(run.finished_at) - new Date(run.started_at)) }) : null),
    run.error ? el('div', { class: 'error', text: run.error }) : null,
    run.detail ? el('p', { class: 'muted', text: run.detail }) : null,
    run.log && run.log.length ? el('div', { class: 'log' }, run.log.map(logLine)) : null);
}

function logLine(line) {
  const fields = Object.entries(line.fields || {})
    .map(([k, v]) => ` ${k}=${typeof v === 'object' ? JSON.stringify(v) : v}`)
    .join('');

  return el('div', {},
    el('span', { class: 'muted', text: when(line.at).slice(11) + ' ' }),
    el('span', { class: 'lvl ' + line.level, text: line.level.padEnd(5) + ' ' }),
    line.message + fields);
}

function timeline(name, entries) {
  const table = el('table', {},
    el('thead', {}, el('tr', {},
      el('th', { text: '' }),
      el('th', { text: 'finished' }),
      el('th', { class: 'num', text: 'stored' }),
      el('th', { class: 'num', text: 'dumped' }),
      el('th', { text: 'verify' }),
      el('th', { text: 'id' }))));

  const body = el('tbody');
  for (const e of entries) {
    const ok = e.outcome === 'succeeded';
    body.append(el('tr', {},
      el('td', {}, el('span', { class: 'dot ' + (ok ? 'green' : 'red') })),
      el('td', {}, el('div', { text: when(e.finished_at) }),
        el('div', { class: 'muted', text: ok ? ago(e.finished_at) : (e.phase ? 'failed in ' + e.phase : 'failed') })),
      el('td', { class: 'num', text: ok ? bytes(e.bytes) : '—' }),
      el('td', { class: 'num', text: ok ? bytes(e.plaintext_bytes) : '—' }),
      el('td', {}, verifyCell(e)),
      el('td', {}, ok
        ? el('a', { class: 'mono', href: `#/t/${encodeURIComponent(name)}/b/${encodeURIComponent(e.id)}`, text: e.id })
        : el('span', { class: 'muted', text: (e.error || '').slice(0, 90) }))));
  }

  table.append(body);
  return table;
}

function verifyCell(e) {
  if (e.outcome !== 'succeeded') return el('span', { class: 'muted', text: '—' });
  if (e.verify_ok === true) return el('span', { class: 'pill ok', text: e.verify_level });
  if (e.verify_ok === false) return el('span', { class: 'pill bad', text: e.verify_level + ' failed' });
  return el('span', { class: 'muted', text: 'not verified' });
}

function retentionView(view) {
  const rows = [
    ...view.delete.map((r) => ({ ...r, action: 'delete' })),
    ...view.keep.map((r) => ({ ...r, action: 'keep' })),
  ].sort((a, b) => new Date(b.at) - new Date(a.at));

  const table = el('table', {},
    el('thead', {}, el('tr', {},
      el('th', { text: 'next prune' }),
      el('th', { text: 'taken' }),
      el('th', { class: 'num', text: 'size' }),
      el('th', { text: 'why' }))));

  const body = el('tbody');
  for (const r of rows) {
    body.append(el('tr', {},
      el('td', {}, el('span', { class: 'pill ' + (r.action === 'keep' ? 'ok' : 'bad'), text: r.action })),
      el('td', { text: when(r.at) }),
      el('td', { class: 'num', text: bytes(r.bytes) }),
      el('td', { class: 'muted', text: r.reason })));
  }
  table.append(body);

  return el('div', {},
    view.blocked ? el('div', { class: 'error', text: 'Nothing will be deleted: ' + view.blocked }) : null,
    view.delete.length
      ? el('p', { class: 'muted', text: `${view.delete.length} backup(s), ${bytes(view.freed)}, would be deleted.` })
      : el('p', { class: 'muted', text: 'Nothing is due for deletion.' }),
    table);
}

// previewPrune shows the plan and only then offers to apply it. The token
// comes from the preview, so "apply" can only ever mean the plan that was
// actually shown (SPEC §13).
async function previewPrune(name) {
  const panel = document.getElementById('run-panel');
  panel.replaceChildren(el('div', { class: 'panel', text: 'Working out what the policy keeps…' }));

  try {
    const { body } = await api(`/api/targets/${encodeURIComponent(name)}/prune`, { method: 'POST' });
    const apply = el('button', {
      class: 'danger',
      text: `Delete ${body.plan.delete.length} backup(s), ${bytes(body.plan.freed)}`,
      onclick: async () => {
        apply.disabled = true;
        try {
          const done = await api(
            `/api/targets/${encodeURIComponent(name)}/prune?token=${encodeURIComponent(body.token)}`,
            { method: 'POST' });
          panel.replaceChildren(el('div', { class: 'panel' },
            el('p', { text: `Deleted ${done.body.objects} object(s).` })));
          route();
        } catch (err) {
          panel.append(failure(err));
        }
      },
    });

    panel.replaceChildren(el('div', { class: 'panel' },
      el('div', { class: 'spread' }, el('strong', { text: 'Dry run' }),
        body.plan.delete.length && !body.plan.blocked ? apply : null),
      retentionView(body.plan)));
  } catch (err) {
    panel.replaceChildren(failure(err));
  }
}

// --- backup ---------------------------------------------------------------

async function viewBackup(name, id) {
  setNav('overview');

  const { body: detail } = await api(
    `/api/backups/${encodeURIComponent(name)}/${encodeURIComponent(id)}`);
  const m = detail.manifest;

  render(
    el('div', {}, el('a', { href: '#/t/' + encodeURIComponent(name), text: '← ' + name, class: 'muted' })),
    heading(m.id, `${m.engine} ${m.server_version}, taken ${when(m.finished_at)}`),

    el('div', { class: 'actions' },
      el('button', { text: 'Download', onclick: (e) => download(name, m.id, 'data', e.target) }),
      el('button', { text: 'Download manifest', onclick: (e) => download(name, m.id, 'manifest', e.target) }),
      m.globals ? el('button', { text: 'Download globals', onclick: (e) => download(name, m.id, 'globals', e.target) }) : null,
      el('button', { text: 'Copy restore command', onclick: (e) => copy(detail.restore_command, e.target) })),
    el('div', { id: 'run-panel' }),

    el('div', { class: 'panel' },
      el('div', { class: 'facts' },
        fact('stored', bytes(m.object.bytes)),
        fact('dumped', bytes(m.plaintext.bytes)),
        fact('duration', ms(m.duration_ms)),
        fact('consistency', m.consistency),
        fact('pipeline', `${m.pipeline.compression}, ${m.pipeline.encryption}`),
        fact('dumper', m.pipeline.dumper),
        fact('tier recorded', m.tier || '—'),
        fact('tables', String((m.tables || []).length)))),

    m.warnings && m.warnings.length ? el('div', {},
      el('h2', { text: 'Warnings' }),
      el('div', { class: 'panel' }, el('ul', {}, m.warnings.map((wtext) => el('li', { text: wtext })))))
      : null,

    m.verify ? el('div', {},
      el('h2', { text: 'Verification' }),
      el('div', { class: 'panel' }, verifyPanel(m.verify))) : null,

    el('h2', { text: 'Manifest' }),
    el('pre', { class: 'doc', text: JSON.stringify(m, null, 2) }));
}

function verifyPanel(v) {
  const details = v.details || {};
  const assertions = details.assertions || [];

  return el('div', {},
    el('div', { class: 'row' },
      el('span', { class: 'pill ' + (v.ok ? 'ok' : 'bad'), text: v.ok ? 'passed' : 'failed' }),
      el('strong', { text: v.level }),
      el('span', { class: 'muted', text: when(v.at) })),
    Array.isArray(details.problems) && details.problems.length
      ? el('ul', {}, details.problems.map((p) => el('li', { class: 'muted', text: p })))
      : null,
    assertions.length
      ? el('ul', { class: 'mono' }, assertions.map((a) => el('li', { text: typeof a === 'string' ? a : JSON.stringify(a) })))
      : null);
}

// download asks for a short-lived URL and follows it. The daemon never proxies
// the bytes, and the browser never sees a bucket credential.
async function download(name, id, which, button) {
  button.disabled = true;
  try {
    const query = which === 'data' ? '' : '?object=' + which;
    const { body } = await api(`/api/backups/${encodeURIComponent(name)}/${encodeURIComponent(id)}/download${query}`);
    window.location.href = body.url;
  } catch (err) {
    document.getElementById('run-panel').replaceChildren(failure(err));
  } finally {
    button.disabled = false;
  }
}

async function copy(text, button) {
  const original = button.textContent;
  try {
    await navigator.clipboard.writeText(text);
    button.textContent = 'Copied';
  } catch {
    // Clipboard access needs a secure context; showing the command is the
    // fallback that always works.
    window.prompt('Restore command', text);
  }
  setTimeout(() => { button.textContent = original; }, 1500);
}

// --- config ---------------------------------------------------------------

async function viewConfig() {
  setNav('config');

  const { body: cfg } = await api('/api/config');

  render(
    heading('Config', cfg.path + ' — as vaultd resolved it, with every secret redacted.',
      el('button', { text: 'Run doctor', onclick: (e) => runDoctor(e.target) })),
    el('div', { id: 'doctor' }),
    el('pre', { class: 'doc', text: cfg.yaml }));

  runDoctor(null, true);
}

async function runDoctor(button, cached) {
  const panel = document.getElementById('doctor');
  if (button) button.disabled = true;
  panel.replaceChildren(el('div', { class: 'panel', text: 'Checking clients, databases, buckets and notifiers…' }));

  try {
    const { body: report } = await api('/api/doctor' + (cached ? '' : '?refresh=1'));
    panel.replaceChildren(doctorPanel(report));
  } catch (err) {
    panel.replaceChildren(failure(err));
  } finally {
    if (button) button.disabled = false;
  }
}

function doctorPanel(report) {
  const table = el('table', {},
    el('thead', {}, el('tr', {},
      el('th', { text: '' }),
      el('th', { text: 'check' }),
      el('th', { text: 'detail' }))));

  const body = el('tbody');
  for (const check of report.checks || []) {
    const dot = check.status === 'ok' ? 'green' : check.status === 'warn' ? 'amber' : check.status === 'fail' ? 'red' : 'unknown';
    body.append(el('tr', {},
      el('td', {}, el('span', { class: 'dot ' + dot })),
      el('td', {}, el('div', { text: check.name }), el('div', { class: 'muted', text: check.group })),
      el('td', {}, el('div', { text: check.detail }),
        check.hint ? el('div', { class: 'muted', text: '→ ' + check.hint }) : null)));
  }
  table.append(body);

  return el('div', { class: 'panel' },
    el('p', { class: 'muted', text: `Checked ${when(report.at)} on the ${report.variant} build.` }),
    table);
}

// --- routing --------------------------------------------------------------

async function route() {
  const hash = window.location.hash.replace(/^#/, '') || '/';
  const parts = hash.split('/').filter(Boolean).map(decodeURIComponent);

  try {
    if (parts[0] === 'config') await viewConfig();
    else if (parts[0] === 't' && parts[2] === 'b') await viewBackup(parts[1], parts[3]);
    else if (parts[0] === 't') await viewTarget(parts[1]);
    else await viewOverview();
  } catch (err) {
    if (err.unauthorized) { gate(token() ? 'That token was not accepted.' : null); return; }
    render(heading('vaultd'), failure(err));
  }
}

async function boot() {
  try {
    const { body } = await api('/api/version');
    document.getElementById('version').textContent = body.version;
  } catch (err) {
    if (err.unauthorized) { gate(); return; }
  }
  route();
}

window.addEventListener('hashchange', route);
boot();
