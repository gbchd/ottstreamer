// The stream detail page: header, playback URLs, player, timeline, break and
// fault panels and the event table, kept current by the event stream.
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import { api, clockOffset, followEvents } from './api.js';
import { html, Link, Badge, ErrorBox, useNow, useAction, clock, ladder, liveEdge, hex, streamPath } from './ui.js';
import { Player } from './player.js';
import { Timeline, LANES, lane } from './timeline.js';

// ponytail: the page keeps the last 5000 events and /timeline returns at most
// 1000 per range, so a busy epoch zoomed out shows only part of its events;
// page with eventsNext if that matters.
const MAX_EVENTS = 5000;

export function Detail({ id, me }) {
  const p = streamPath(id);
  const canWrite = me.role === 'write';
  const [stream, setStream] = useState(null);
  const [error, setError] = useState(null);
  const [offset, setOffset] = useState(0);
  const [meta, setMeta] = useState(null);
  const [events, setEvents] = useState([]);
  const [breaks, setBreaks] = useState(new Map());
  const [rules, setRules] = useState([]);
  const [pick, setPick] = useState(null);
  const now = useNow(500, offset);
  const range = useRef(null);
  const since = useRef(null); // null until the first timeline answer

  const loadStream = useCallback(() => api(p).then(setStream, setError), [p]);
  const loadFaults = useCallback(() => api(p + '/faults').then(setRules, () => {}), [p]);
  const mergeBreaks = useCallback((list, reset = false) => setBreaks((old) => {
    const m = new Map(reset ? [] : old);
    for (const b of list) m.set(b.id, b);
    return m;
  }), []);
  const loadBreaks = useCallback(() => api(p + '/breaks').then((l) => mergeBreaks(l), () => {}), [p, mergeBreaks]);
  const mergeEvents = useCallback((list) => setEvents((old) => {
    if (list.length === 0) return old;
    const m = new Map(old.map((e) => [e.id, e]));
    for (const e of list) m.set(e.id, e);
    return [...m.values()].sort((a, b) => (a.id < b.id ? -1 : 1)).slice(-MAX_EVENTS);
  }), []);
  const loadTimeline = useCallback(async (from, to) => {
    range.current = [from, to];
    try {
      const t = await api(`${p}/timeline?from=${new Date(from).toISOString()}&to=${new Date(to).toISOString()}`);
      setMeta(t);
      mergeEvents(t.events);
      mergeBreaks(t.breaks);
      if (since.current === null) since.current = t.events.at(-1)?.id ?? '';
    } catch (e) {
      setError(e);
    }
  }, [p, mergeEvents, mergeBreaks]);

  useEffect(() => {
    loadStream();
    loadFaults();
    clockOffset().then(setOffset, () => {});
  }, [loadStream, loadFaults]);

  // Follow the event stream once the first timeline is in, so that it
  // resumes right after the last event that timeline returned.
  const [following, setFollowing] = useState(false);
  useEffect(() => {
    if (following || since.current === null) return;
    setFollowing(true);
  });
  useEffect(() => {
    if (!following) return undefined;
    const ac = new AbortController();
    followEvents(id, since.current, (e) => {
      mergeEvents([e]);
      const g = e.type.split('.')[0];
      if (g === 'break' || g === 'signal') loadBreaks();
      if (g === 'faults') loadFaults();
      if (g === 'stream') {
        loadStream();
        if (e.type === 'stream.started') mergeBreaks([], true);
        if (range.current) loadTimeline(...range.current);
      }
    }, ac.signal);
    return () => ac.abort();
  }, [following, id, mergeEvents, loadBreaks, loadFaults, loadStream, loadTimeline, mergeBreaks]);

  if (!stream) return html`<${ErrorBox} error=${error} />${!error && html`<p class="muted">Loading…</p>`}`;
  const epoch = stream.status.epoch;
  const playing = epoch && stream.status.state !== 'stopped';
  const m = playing && meta?.ast ? markers(meta, epoch, now) : null;
  const spec = epoch?.spec ?? stream.spec;

  return html`
    <p><${Link} href="/ui/">← Streams</${Link}></p>
    <${Header} stream=${stream} canWrite=${canWrite} onChange=${loadStream} />
    <${ErrorBox} error=${error} />
    ${playing && html`<${Playback} id=${id} spec=${spec} />`}
    <div class="grid">
      <${BreakPanel} p=${p} breaks=${breaks} m=${m} canWrite=${canWrite && stream.status.state === 'running'} pick=${pick} onPick=${setPick} onChange=${loadBreaks} />
      <${FaultPanel} p=${p} rules=${rules} now=${now} canWrite=${canWrite} spec=${spec} onChange=${setRules} />
    </div>
    <section>
      <h2>Timeline</h2>
      <${Timeline} m=${m} now=${now} events=${events} breaks=${[...breaks.values()]} onRange=${loadTimeline} onPick=${setPick} />
      <${Picked} pick=${pick} breaks=${breaks} />
    </section>
    <${EventTable} events=${events} onPick=${setPick} />`;
}

// markers advances the timeline markers of t to now, from the local clock.
function markers(t, epoch, now) {
  const ast = Date.parse(t.ast);
  const seg = t.segmentSeconds * 1000;
  const edge = liveEdge(ast, t.segmentSeconds, t.endSegment, now);
  const lead = t.frozenHorizon - t.liveEdge;
  const end = t.endSegment ?? Infinity;
  const dvrSegs = Math.ceil((epoch.spec.window?.dvrSeconds ?? 30) / t.segmentSeconds - 1e-9);
  return { ast, seg, edge, horizon: Math.min(edge + lead, end), dvrStart: Math.max(0, edge - dvrSegs + 1) };
}

function Header({ stream, canWrite, onChange }) {
  const { status } = stream;
  const action = useAction();
  const act = (verb) => action.run(async () => {
    await api(`${streamPath(stream.id)}/${verb}`, { method: 'POST', body: {} });
    onChange();
  });
  return html`<header class="detail">
    <h1>${stream.name || stream.id} <${Badge} state=${status.state} /></h1>
    <p class="muted">${stream.id} · ${ladder(stream.spec)} · ${stream.spec.formats.join(', ')}
      ${status.epoch && html` · epoch ${status.epoch.number} · AST ${status.epoch.ast}${status.epoch.endSegment !== null ? ` · ends after segment ${status.epoch.endSegment}` : ''}`}</p>
    ${status.pendingChanges && html`<div class="banner">The spec changed since this epoch started: restart to apply.</div>`}
    ${canWrite && html`<p class="inline">${status.state === 'running'
      ? html`<button disabled=${action.busy} onClick=${() => act('stop')}>Stop</button>`
      : html`<button disabled=${action.busy} onClick=${() => act('start')}>Start</button>`}
      <${Link} href=${`/ui/streams/${encodeURIComponent(stream.id)}/edit`} class="button">Edit</${Link}></p>`}
    <${ErrorBox} error=${action.error} />
  </header>`;
}

const STYLES = { cues: ['daterange', 'cueout', 'oatcls', 'adobe'], dashcues: ['eventstream-bin', 'eventstream-xml'] };
const NUMBERS = [
  ['faultPercent', 'Fault percent'],
  ['delay', 'Delay (ms)'],
  ['throttle', 'Throttle (kbit/s)'],
  ['truncate', 'Truncate (%)'],
  ['stale', 'Stale (s)'],
];

// query builds the manifest query from the builder's options, in a fixed
// order and without the empty ones.
function query(o) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(o)) {
    const s = Array.isArray(v) ? v.join(',') : v === true ? '1' : v === false ? '' : String(v ?? '');
    if (s !== '') q.set(k, s);
  }
  const s = q.toString().replaceAll('%2C', ',');
  return s ? '?' + s : '';
}

function Playback({ id, spec }) {
  const [o, setO] = useState({});
  const [applied, setApplied] = useState('');
  const set = (k, v) => setO((old) => ({ ...old, [k]: v }));
  const toggle = (k, v) => set(k, (o[k] ?? []).includes(v) ? o[k].filter((x) => x !== v) : [...(o[k] ?? []), v]);
  const q = query(o);
  const base = `${location.origin}/live/${encodeURIComponent(id)}/`;
  const urls = (s) => ({ hls: base + 'master.m3u8' + s, dash: base + 'manifest.mpd' + s });
  const shown = urls(q);
  const hls = spec.formats.includes('hls');
  const dash = spec.formats.includes('dash');
  const field = (k, label) => html`<label>${label} <input type="number" min="0" value=${o[k] ?? ''} onInput=${(e) => set(k, e.target.value)} /></label>`;
  return html`<section>
    <h2>Playback</h2>
    ${spec.formats.map((f) => html`<div class="url"><code>${shown[f]}</code>
      <button onClick=${() => navigator.clipboard.writeText(shown[f])}>Copy</button></div>`)}
    <details>
      <summary>Query options</summary>
      <div class="builder">
        ${dash && html`<label>Addressing <select value=${o.addressing ?? ''} onChange=${(e) => set('addressing', e.target.value)}>
          <option value="">stream default</option><option>number</option><option>timeline-time</option><option>timeline-number</option>
        </select></label>
        <label>Periods <select value=${o.periods ?? ''} onChange=${(e) => set('periods', e.target.value)}>
          <option value="">stream default</option><option value="true">multi-period</option><option value="false">single period</option>
        </select></label>`}
        ${field('dvr', 'DVR window (s)')}
        ${Object.entries(STYLES).filter(([k]) => (k === 'cues' ? hls : dash)).map(([k, all]) => html`<fieldset><legend>${k === 'cues' ? 'HLS cue styles' : 'DASH cue styles'}</legend>
          ${all.map((s) => html`<label><input type="checkbox" checked=${(o[k] ?? []).includes(s)} onChange=${() => toggle(k, s)} /> ${s}</label>`)}
        </fieldset>`)}
        <fieldset><legend>URL faults</legend>
          <label>Status <select value=${o.fault ?? ''} onChange=${(e) => set('fault', e.target.value)}>
            <option value="">none</option>${[404, 410, 500, 503, 504].map((s) => html`<option>${s}</option>`)}
          </select></label>
          <label>On ${['segment', 'init', 'playlist', 'mpd', 'master'].map((k) => html`<label><input type="checkbox" checked=${(o.faultOn ?? []).includes(k)} onChange=${() => toggle('faultOn', k)} /> ${k}</label>`)}</label>
          <label>Renditions <input value=${o.faultRenditions ?? ''} placeholder="720p,en" onInput=${(e) => set('faultRenditions', e.target.value)} /></label>
          <label>Mode <select value=${o.faultMode ?? ''} onChange=${(e) => set('faultMode', e.target.value)}><option value="">sticky</option><option>transient</option></select></label>
          ${NUMBERS.map(([k, label]) => field(k, label))}
          <label><input type="checkbox" checked=${Boolean(o.reset)} onChange=${(e) => set('reset', e.target.checked)} /> reset</label>
        </fieldset>
      </div>
    </details>
    <p><button disabled=${q === applied} onClick=${() => setApplied(q)}>Play with these options</button></p>
    <${Player} urls=${urls(applied)} formats=${spec.formats} />
  </section>`;
}

// state of break b at live edge edge: upcoming, active or done.
function breakState(b, edge) {
  if (b.cancelled) return 'cancelled';
  if (b.startSegment > edge) return 'upcoming';
  return b.endSegment > edge ? 'active' : 'done';
}

function BreakPanel({ p, breaks, m, canWrite, pick, onPick, onChange }) {
  const [form, setForm] = useState({ duration: 30, when: 'now', in: 10, at: '', replaceRecurring: false });
  const action = useAction();
  const set = (k, v) => setForm((f) => ({ ...f, [k]: v }));
  const edge = m?.edge ?? -1;
  const insert = (e) => {
    e.preventDefault();
    const body = { duration: Number(form.duration), replaceRecurring: form.replaceRecurring };
    if (form.when === 'in') body.in = `${Number(form.in)}s`;
    if (form.when === 'at') body.at = new Date(form.at).toISOString();
    action.run(async () => {
      onPick({ break: await api(p + '/breaks', { method: 'POST', body }) });
      onChange();
    });
  };
  const end = (b) => action.run(async () => {
    onPick({ break: await api(`${p}/breaks/${encodeURIComponent(b.id)}`, { method: 'DELETE' }) });
    onChange();
  });
  const list = [...breaks.values()].filter((b) => breakState(b, edge) !== 'done' || b.endSegment > edge - 30)
    .sort((a, b) => a.startSegment - b.startSegment);
  return html`<section class="panel">
    <h2>Breaks</h2>
    ${canWrite && html`<form class="inline" onSubmit=${insert}>
      <label>Duration (s) <input type="number" min="1" step="any" required value=${form.duration} onInput=${(e) => set('duration', e.target.value)} /></label>
      <label><input type="radio" checked=${form.when === 'now'} onChange=${() => set('when', 'now')} /> now</label>
      <label><input type="radio" checked=${form.when === 'in'} onChange=${() => set('when', 'in')} /> in
        <input type="number" min="0" class="short" value=${form.in} onInput=${(e) => set('in', e.target.value)} /> s</label>
      <label><input type="radio" checked=${form.when === 'at'} onChange=${() => set('when', 'at')} /> at
        <input type="datetime-local" step="1" value=${form.at} onInput=${(e) => set('at', e.target.value)} /></label>
      <label><input type="checkbox" checked=${form.replaceRecurring} onChange=${(e) => set('replaceRecurring', e.target.checked)} /> replace recurring</label>
      <button type="submit" disabled=${action.busy}>Insert break</button>
    </form>`}
    <${ErrorBox} error=${action.error} />
    <table>
      <thead><tr><th>Break</th><th>Start</th><th>Segments</th><th>Duration</th><th>State</th>${canWrite && html`<th></th>`}</tr></thead>
      <tbody>${list.map((b) => {
        const st = breakState(b, edge);
        return html`<tr key=${b.id} class=${pick?.break?.id === b.id ? 'picked' : ''} onClick=${() => onPick({ break: b })}>
          <td>${b.id}${b.recurring ? ' ↻' : ''}</td><td>${clock(b.start)}</td><td>${b.startSegment}-${b.endSegment - 1}</td>
          <td>${b.durationSeconds} s</td><td><span class=${'tag ' + st}>${st}</span></td>
          ${canWrite && html`<td>${st === 'upcoming' && html`<button disabled=${action.busy} onClick=${(e) => { e.stopPropagation(); end(b); }}>Cancel</button>`}
            ${st === 'active' && html`<button disabled=${action.busy} onClick=${(e) => { e.stopPropagation(); end(b); }}>End early</button>`}</td>`}
        </tr>`;
      })}</tbody>
    </table>
    ${list.length === 0 && html`<p class="muted">No breaks in this epoch yet.</p>`}
  </section>`;
}

// presets are starting points for the rule editor; each removes itself
// after ten minutes unless its ttlSeconds is taken out.
function presets(spec) {
  const v = spec.video[0]?.id ?? '720p';
  const out = [
    ['5 % 503 on segments', { id: 'p-503', match: { kind: 'segment' }, percent: 5, action: { status: 503 } }],
    [`2 s delay on ${v}`, { id: `delay-${v}`, match: { kind: 'segment', renditions: [v] }, action: { delayMs: 2000 } }],
    ['10 % gaps', { id: 'gaps', match: { kind: 'segment' }, percent: 10, action: { gap: true } }],
    ['Throttle segments to 500 kbit/s', { id: 'throttle', match: { kind: 'segment' }, action: { throttleKbps: 500 } }],
    ['Truncate 10 % of segments', { id: 'truncate', match: { kind: 'segment' }, percent: 10, action: { truncatePercent: 50 } }],
    ['Clock skew +30 s', { id: 'skew', match: { kind: 'time' }, action: { clockSkewSeconds: 30 } }],
  ];
  if (spec.formats.includes('hls')) out.push(['Stale playlists 10 s', { id: 'stale-hls', match: { kind: 'playlist' }, action: { staleSeconds: 10 } }]);
  if (spec.formats.includes('dash')) out.push(['Stale MPD 10 s', { id: 'stale-mpd', match: { kind: 'mpd' }, action: { staleSeconds: 10 } }]);
  return out.map(([label, rule]) => [label, { ...rule, ttlSeconds: 600 }]);
}

function FaultPanel({ p, rules, now, canWrite, spec, onChange }) {
  const [draft, setDraft] = useState('');
  const action = useAction();
  const put = (next) => action.run(async () => onChange(await api(p + '/faults', { method: 'PUT', body: next })));
  const save = () => {
    let rule;
    try {
      rule = JSON.parse(draft);
    } catch (e) {
      action.setError(new Error(`Not JSON: ${e.message}`));
      return;
    }
    put([...rules.filter((r) => r.id !== rule.id).map(keep), keep(rule)]);
  };
  // keep turns an active rule back into one to PUT: a PUT restarts every
  // ttlSeconds, so a rule that expires keeps the time it has left.
  const keep = ({ expiresAt, ...r }) => (expiresAt ? { ...r, ttlSeconds: Math.max(1, (Date.parse(expiresAt) - now) / 1000) } : r);
  const remove = (id) => put(rules.filter((r) => r.id !== id).map(keep));
  const clear = () => action.run(async () => {
    await api(p + '/faults', { method: 'DELETE' });
    onChange([]);
  });
  return html`<section class="panel">
    <h2>Fault rules</h2>
    <table>
      <thead><tr><th>Rule</th><th>Match</th><th>Hit</th><th>Action</th><th>Expires</th>${canWrite && html`<th></th>`}</tr></thead>
      <tbody>${rules.map((r) => html`<tr key=${r.id}>
        <td>${r.id}</td>
        <td>${r.match.kind}${r.match.renditions?.length ? ` · ${r.match.renditions.join(',')}` : ''}${r.match.segments ? ` · ${r.match.segments.from ?? 0}-${r.match.segments.to ?? '∞'}` : ''}</td>
        <td>${r.percent ?? 100} % ${r.mode ?? 'sticky'}</td>
        <td><code>${JSON.stringify(r.action)}</code></td>
        <td>${r.expiresAt ? `in ${Math.max(0, Math.round((Date.parse(r.expiresAt) - now) / 1000))} s` : ''}</td>
        ${canWrite && html`<td><button onClick=${() => setDraft(JSON.stringify(r, null, 2))}>Edit</button>
          <button disabled=${action.busy} onClick=${() => remove(r.id)}>Remove</button></td>`}
      </tr>`)}</tbody>
    </table>
    ${rules.length === 0 && html`<p class="muted">No active rules.</p>`}
    ${canWrite && html`
      <p class="presets">${presets(spec).map(([label, rule]) => html`<button class="preset" onClick=${() => setDraft(JSON.stringify(rule, null, 2))}>${label}</button>`)}</p>
      <textarea rows="8" spellcheck="false" placeholder="A rule as JSON; pick a preset to start" value=${draft} onInput=${(e) => setDraft(e.target.value)}></textarea>
      <p><button disabled=${action.busy || !draft} onClick=${save}>Add or replace rule</button>
        <button disabled=${action.busy || rules.length === 0} onClick=${clear}>Clear all</button></p>`}
    <${ErrorBox} error=${action.error} />
  </section>`;
}

function Cue({ label, b64 }) {
  if (!b64) return null;
  return html`<dt>${label}</dt><dd><code>${b64}</code><br /><code class="muted">${hex(b64)}</code></dd>`;
}

// Picked shows the break or event last clicked.
function Picked({ pick, breaks }) {
  if (!pick) return null;
  if (pick.break) {
    const b = breaks.get(pick.break.id) ?? pick.break;
    return html`<div class="picked-box"><h3>Break ${b.id}</h3><dl>
      <dt>Event id</dt><dd>${b.eventId}</dd>
      <dt>Time</dt><dd>${b.start} → ${b.end}</dd>
      <dt>Segments</dt><dd>${b.startSegment} to ${b.endSegment - 1}</dd>
      <dt>Duration</dt><dd>${b.durationSeconds} s (requested ${b.requestedSeconds} s)</dd>
      <dt>Flags</dt><dd>${[b.recurring && 'recurring', b.cancelled && 'cancelled'].filter(Boolean).join(', ') || 'none'}</dd>
      <${Cue} label="Cue-out" b64=${b.cueOut?.base64} />
      <${Cue} label="Cue-in" b64=${b.cueIn?.base64} />
    </dl></div>`;
  }
  const e = pick.event;
  return html`<div class="picked-box"><h3>${e.type}</h3><dl>
    <dt>At</dt><dd>${e.at}${e.segment !== null ? ` · segment ${e.segment}` : ''}${e.epoch !== null ? ` · epoch ${e.epoch}` : ''} · ${e.actor}</dd>
    ${e.data.hls && html`<dt>Cue styles</dt><dd>HLS ${e.data.hls.join(', ') || 'none'} · DASH ${e.data.dash?.join(', ') || 'none'}</dd>`}
    <${Cue} label="Cue" b64=${e.data.cue} />
    <dt>Data</dt><dd><pre>${JSON.stringify(e.data, null, 2)}</pre></dd>
  </dl></div>`;
}

function EventTable({ events, onPick }) {
  const [group, setGroup] = useState('');
  const [text, setText] = useState('');
  const shown = events.filter((e) => (!group || lane(e.type) === group) && e.type.includes(text)).slice(-300).reverse();
  return html`<section>
    <h2>Events</h2>
    <p class="inline">
      <label>Lane <select value=${group} onChange=${(e) => setGroup(e.target.value)}><option value="">all</option>${LANES.map((l) => html`<option>${l}</option>`)}</select></label>
      <label>Type contains <input value=${text} onInput=${(e) => setText(e.target.value)} placeholder="break.started" /></label>
      <span class="muted">${shown.length} of ${events.length}</span>
    </p>
    <table class="events">
      <thead><tr><th>At (UTC)</th><th>Segment</th><th>Type</th><th>Actor</th><th>Data</th></tr></thead>
      <tbody>${shown.map((e) => html`<tr key=${e.id} onClick=${() => onPick({ event: e })}>
        <td>${clock(e.at, true)}</td><td>${e.segment ?? ''}</td><td>${e.type}</td><td>${e.actor}</td><td><code>${JSON.stringify(e.data)}</code></td>
      </tr>`)}</tbody>
    </table>
  </section>`;
}
