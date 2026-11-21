// The streams page: one row per stream, polled every 5 s.
import { useCallback, useEffect, useState } from 'preact/hooks';
import { api } from './api.js';
import { html, Link, Badge, ErrorBox, useNow, useAction, clock, duration, ladder, streamPath } from './ui.js';

// details reads what a row of a running or ended stream shows besides the
// stream itself: position, next break, active rules and recent late segments.
// ponytail: four calls per stream every 5 s, fine for a handful of streams;
// a summary field on GET /api/streams if lists grow long.
async function details(id) {
  const p = streamPath(id);
  const from = new Date(Date.now() - 60_000).toISOString();
  const [pos, breaks, faults, late] = await Promise.all([
    api(p + '/status'),
    api(p + '/breaks').catch(() => []),
    api(p + '/faults'),
    api(`${p}/events?types=segment.late&from=${encodeURIComponent(from)}&limit=1000`),
  ]);
  const next = breaks.find((b) => !b.cancelled && b.startSegment > (pos.liveEdge ?? -1));
  return { pos, next, faults: faults.length, late: late.events.length };
}

export function Streams({ me }) {
  const [rows, setRows] = useState(null);
  const [error, setError] = useState(null);
  const action = useAction();
  const now = useNow(1000);
  const load = useCallback(async () => {
    try {
      const streams = await api('/streams');
      setRows(await Promise.all(streams.map(async (s) => ({ s, ...(s.status.state === 'stopped' ? {} : await details(s.id)) }))));
      setError(null);
    } catch (e) {
      setError(e);
    }
  }, []);
  useEffect(() => {
    load();
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  }, [load]);
  const act = (id, verb) => action.run(async () => {
    await api(`${streamPath(id)}/${verb}`, { method: 'POST', body: {} });
    await load();
  });
  if (!rows) return html`<${ErrorBox} error=${error} />${!error && html`<p class="muted">Loading…</p>`}`;
  return html`
    <h1>Streams</h1>
    ${me.role === 'write' && html`<p><${Link} href="/ui/streams/new" class="button">New stream</${Link}></p>`}
    <${ErrorBox} error=${error || action.error} />
    ${rows.length === 0 && html`<p class="muted">No streams yet.</p>`}
    <table class="streams">
      <thead><tr><th>Stream</th><th>State</th><th>Uptime</th><th>Formats</th><th>Ladder</th><th>Live edge</th><th>Break</th><th>Health</th>${me.role === 'write' && html`<th></th>`}</tr></thead>
      <tbody>
        ${rows.map(({ s, pos, next, faults, late }) => {
          const epoch = s.status.epoch;
          const active = pos?.activeBreak;
          return html`<tr key=${s.id}>
            <td><${Link} href=${'/ui/streams/' + encodeURIComponent(s.id)}>${s.name || s.id}</${Link}><div class="muted">${s.id}</div></td>
            <td><${Badge} state=${s.status.state} />${s.status.pendingChanges && html` <span class="tag warn" title="The spec changed since the start">restart to apply</span>`}</td>
            <td>${s.status.state === 'running' && epoch ? duration(now - Date.parse(epoch.ast)) : ''}</td>
            <td>${s.spec.formats.join(', ')}</td>
            <td>${ladder(epoch?.spec ?? s.spec)}</td>
            <td>${pos?.liveEdge ?? ''}</td>
            <td>${active ? html`<span class="tag ad">ad #${active.id} until ${clock(active.end)}</span>` : next ? html`next #${next.id} at ${clock(next.start)}` : ''}</td>
            <td>${pos && html`<span class=${late ? 'warn' : 'muted'}>${late} late/min</span> · <span class=${faults ? 'warn' : 'muted'}>${faults} fault rules</span>`}</td>
            ${me.role === 'write' && html`<td>${s.status.state === 'running'
              ? html`<button disabled=${action.busy} onClick=${() => act(s.id, 'stop')}>Stop</button>`
              : html`<button disabled=${action.busy} onClick=${() => act(s.id, 'start')}>Start</button>`}</td>`}
          </tr>`;
        })}
      </tbody>
    </table>`;
}
