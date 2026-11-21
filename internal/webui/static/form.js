// The create and edit form: the common spec fields, a raw JSON tab for the
// rest, 422 field errors next to their fields, and a 412 that shows what
// changed on the server.
import { useEffect, useState } from 'preact/hooks';
import { api, APIError } from './api.js';
import { html, Link, ErrorBox, navigate, streamPath } from './ui.js';

const NEW = {
  id: '',
  name: '',
  spec: {
    formats: ['hls', 'dash'],
    fps: 25,
    segmentSeconds: 1.92,
    video: [
      { id: '720p', codec: 'h264', width: 1280, height: 720, bitrateKbps: 3000 },
      { id: '360p', codec: 'h264', width: 640, height: 360, bitrateKbps: 800 },
    ],
    audio: [{ id: 'en', lang: 'en', name: 'English', channels: 2, codec: 'aac', bitrateKbps: 128, default: true }],
    subtitles: [],
  },
};

// Audio frame sizes in samples at 48 kHz.
const FRAME = { aac: 1024, ac3: 1536, eac3: 1536, opus: 960 };
const gcd = (a, b) => (b ? gcd(b, a % b) : a);

// frameLCM is the shortest span, in samples, that holds a whole number of
// frames of every audio codec of the ladder.
function frameLCM(audio) {
  const sizes = [...new Set((audio ?? []).map((a) => FRAME[a.codec || 'aac']).filter(Boolean))];
  return (sizes.length ? sizes : [1024]).reduce((a, b) => (a * b) / gcd(a, b));
}

// alignedDurations lists the segment durations from 0.5 to 10 s that are a
// whole number of video frames and of audio frames of every codec, and a
// whole number of milliseconds, so they can be typed exactly.
export function alignedDurations(fps, audio) {
  const l = frameLCM(audio);
  const out = [];
  for (let samples = l; samples <= 480_000; samples += l) {
    if (samples >= 24_000 && (samples * fps) % 48_000 === 0 && samples % 48 === 0) out.push(samples / 48_000);
  }
  return out;
}

function aligned(seconds, audio) {
  const samples = seconds * 48_000;
  return Math.abs(samples - Math.round(samples)) < 1e-6 && Math.round(samples) % frameLCM(audio) === 0;
}

// pathOf turns ['spec', 'video', 1, 'id'] into "spec.video[1].id", the API's
// field error format.
const pathOf = (p) => p.reduce((s, k) => (typeof k === 'number' ? `${s}[${k}]` : s ? `${s}.${k}` : k), '');
const getIn = (o, p) => p.reduce((v, k) => v?.[k], o);

// setIn returns a copy of o with p set to v; undefined removes the key, so
// the API applies its default.
function setIn(o, p, v) {
  const out = structuredClone(o);
  let cur = out;
  p.slice(0, -1).forEach((k, i) => {
    cur[k] ??= typeof p[i + 1] === 'number' ? [] : {};
    cur = cur[k];
  });
  if (v === undefined) delete cur[p.at(-1)];
  else cur[p.at(-1)] = v;
  return out;
}

// changes lists the leaf values that differ between a and b.
function changes(a, b, path = '') {
  const isObj = (v) => v !== null && typeof v === 'object';
  if (!isObj(a) || !isObj(b)) return JSON.stringify(a) === JSON.stringify(b) ? [] : [{ path, from: a, to: b }];
  const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
  return [...keys].flatMap((k) => changes(a[k], b[k], Array.isArray(a) || Array.isArray(b) ? `${path}[${k}]` : path ? `${path}.${k}` : k));
}

const docOf = (s) => ({ name: s.name, spec: s.spec });

export function StreamForm({ id, me }) {
  const isNew = id === undefined;
  const [base, setBase] = useState(null); // the stream as loaded, for If-Match and the 412 diff
  const [doc, setDoc] = useState(isNew ? NEW : null);
  const [tab, setTab] = useState('form');
  const [raw, setRaw] = useState('');
  const [error, setError] = useState(null);
  const [conflict, setConflict] = useState(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (isNew) return;
    api(streamPath(id)).then((s) => {
      setBase(s);
      setDoc(docOf(s));
    }, setError);
  }, [id]);

  if (me.role !== 'write') return html`<p class="muted">This API key is read-only, so it cannot create or edit streams.</p>`;
  if (!doc) return html`<${ErrorBox} error=${error} />${!error && html`<p class="muted">Loading…</p>`}`;

  const fieldErrors = error?.fields ?? [];
  const set = (p, v) => setDoc((d) => setIn(d, p, v));
  const props = { doc, set, fieldErrors };

  // current is the document to save: the form's, or the raw tab's parsed.
  const current = () => {
    if (tab === 'form') return doc;
    const d = JSON.parse(raw);
    setDoc(d);
    return d;
  };
  const switchTab = (t) => {
    if (t === tab) return;
    if (t === 'json') {
      setRaw(JSON.stringify(doc, null, 2));
    } else {
      try {
        setDoc(JSON.parse(raw));
      } catch (e) {
        setError(new Error(`The JSON does not parse: ${e.message}`));
        return;
      }
    }
    setError(null);
    setTab(t);
  };

  const save = async (e) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const d = current();
      if (isNew) {
        const s = await api('/streams', { method: 'POST', body: d });
        navigate('/ui/streams/' + encodeURIComponent(s.id));
        return;
      }
      await api(streamPath(id), { method: 'PUT', body: { name: d.name, spec: d.spec }, headers: { 'If-Match': `"${base.version}"` } });
      navigate('/ui/streams/' + encodeURIComponent(id));
    } catch (err) {
      if (err instanceof APIError && err.status === 412) {
        const latest = await api(streamPath(id));
        setConflict({ changes: changes(docOf(base), docOf(latest)), latest });
        setBase(latest);
      } else {
        setError(err instanceof SyntaxError ? new Error(`The JSON does not parse: ${err.message}`) : err);
      }
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!confirm(`Delete stream ${id} and its breaks?`)) return;
    try {
      await api(streamPath(id), { method: 'DELETE', headers: { 'If-Match': `"${base.version}"` } });
      navigate('/ui/');
    } catch (err) {
      setError(err);
    }
  };

  const back = isNew ? '/ui/' : '/ui/streams/' + encodeURIComponent(id);
  return html`<form class="stream-form" onSubmit=${save} novalidate>
    <p><${Link} href=${back}>← ${isNew ? 'Streams' : id}</${Link}></p>
    <h1>${isNew ? 'New stream' : `Edit ${base.name || id}`}</h1>
    ${!isNew && base.status.state !== 'stopped' && html`<p class="banner">The stream is ${base.status.state}: media changes apply at its next start; the window applies at once.</p>`}
    ${conflict && html`<${Conflict} conflict=${conflict} onLoad=${() => { setDoc(docOf(conflict.latest)); setRaw(JSON.stringify(docOf(conflict.latest), null, 2)); setConflict(null); }} onKeep=${() => setConflict(null)} />`}
    <${ErrorBox} error=${error} />
    <p class="tabs">
      <button type="button" class=${tab === 'form' ? 'on' : ''} onClick=${() => switchTab('form')}>Form</button>
      <button type="button" class=${tab === 'json' ? 'on' : ''} onClick=${() => switchTab('json')}>Raw JSON</button>
    </p>
    ${tab === 'json'
      ? html`<textarea class="raw" rows="30" spellcheck="false" value=${raw} onInput=${(e) => setRaw(e.target.value)}></textarea>`
      : html`<${Fields} ...${props} isNew=${isNew} />`}
    <p class="actions">
      <button type="submit" disabled=${busy}>${isNew ? 'Create' : conflict ? 'Save mine over it' : 'Save'}</button>
      <${Link} href=${back}>Cancel</${Link}>
      ${!isNew && html`<button type="button" class="danger" disabled=${busy || base.status.state === 'running'} title=${base.status.state === 'running' ? 'Stop the stream first' : ''} onClick=${remove}>Delete stream</button>`}
    </p>
  </form>`;
}

function Conflict({ conflict, onLoad, onKeep }) {
  const show = (v) => (v === undefined ? '—' : JSON.stringify(v));
  return html`<div class="error" role="alert">
    <strong>The stream changed since you opened it.</strong> What changed on the server:
    <table class="diff"><tbody>${conflict.changes.map((c) => html`<tr><td><code>${c.path}</code></td><td><code>${show(c.from)}</code> → <code>${show(c.to)}</code></td></tr>`)}</tbody></table>
    <p><button type="button" onClick=${onLoad}>Discard my edits, load theirs</button>
      <button type="button" onClick=${onKeep}>Keep my edits</button></p>
  </div>`;
}

// Err shows the API's messages for the field at path p.
function Err({ fieldErrors, p }) {
  const key = pathOf(p);
  const msgs = fieldErrors.filter((f) => f.field === key);
  return msgs.length ? html`<span class="field-error">${msgs.map((f) => f.message).join('; ')}</span>` : null;
}

function Num({ doc, set, fieldErrors, p, label, placeholder, step = 'any', cls = '' }) {
  const v = getIn(doc, p);
  return html`<label class=${cls}>${label} <input type="number" step=${step} placeholder=${placeholder ?? ''} value=${v ?? ''}
    onInput=${(e) => set(p, e.target.value === '' ? undefined : Number(e.target.value))} /><${Err} fieldErrors=${fieldErrors} p=${p} /></label>`;
}

function Text({ doc, set, fieldErrors, p, label, placeholder, cls = '' }) {
  return html`<label class=${cls}>${label} <input value=${getIn(doc, p) ?? ''} placeholder=${placeholder ?? ''}
    onInput=${(e) => set(p, e.target.value === '' ? undefined : e.target.value)} /><${Err} fieldErrors=${fieldErrors} p=${p} /></label>`;
}

// Select stores numbers as numbers; '' removes the field (the default).
function Select({ doc, set, fieldErrors, p, label, options, def }) {
  const v = getIn(doc, p);
  return html`<label>${label} <select value=${v ?? ''} onChange=${(e) => {
    const s = e.target.value;
    set(p, s === '' ? undefined : typeof options[0] === 'number' ? Number(s) : s);
  }}>
    ${def !== undefined && html`<option value="">${def}</option>`}
    ${options.map((o) => html`<option value=${o}>${o}</option>`)}
  </select><${Err} fieldErrors=${fieldErrors} p=${p} /></label>`;
}

function Check({ doc, set, p, label }) {
  return html`<label><input type="checkbox" checked=${Boolean(getIn(doc, p))} onChange=${(e) => set(p, e.target.checked || undefined)} /> ${label}</label>`;
}

// Multi edits an array of choices as checkboxes.
function Multi({ doc, set, fieldErrors, p, label, options, hint }) {
  const v = getIn(doc, p) ?? [];
  const toggle = (o) => {
    const next = v.includes(o) ? v.filter((x) => x !== o) : options.filter((x) => x === o || v.includes(x));
    set(p, next.length ? next : undefined);
  };
  return html`<fieldset><legend>${label}</legend>
    ${options.map((o) => html`<label><input type="checkbox" checked=${v.includes(o)} onChange=${() => toggle(o)} /> ${o}</label>`)}
    ${hint && html`<span class="muted">${hint}</span>`}
    <${Err} fieldErrors=${fieldErrors} p=${p} />
  </fieldset>`;
}

const PROFILES = { h264: ['baseline', 'main', 'high'], hevc: ['main', 'main10'], av1: ['main'] };

// Rows edits one array of the spec as a table with add and remove.
function Rows({ doc, set, fieldErrors, name, title, columns, blank }) {
  const rows = getIn(doc, ['spec', name]) ?? [];
  const add = () => set(['spec', name], [...rows, blank(rows)]);
  const remove = (i) => set(['spec', name], rows.filter((_, j) => j !== i));
  return html`<section>
    <h2>${title} <${Err} fieldErrors=${fieldErrors} p=${['spec', name]} /></h2>
    <table class="rows">
      <thead><tr>${columns.map((c) => html`<th>${c.label}</th>`)}<th></th></tr></thead>
      <tbody>${rows.map((r, i) => html`<tr key=${i}>
        ${columns.map((c) => html`<td>${c.cell({ doc, set, fieldErrors, p: ['spec', name, i, c.key], row: r, label: '' })}</td>`)}
        <td><button type="button" onClick=${() => remove(i)} aria-label=${`remove ${r.id ?? i}`}>Remove</button>
          <${Err} fieldErrors=${fieldErrors} p=${['spec', name, i]} /></td>
      </tr>`)}</tbody>
    </table>
    <p><button type="button" onClick=${add}>Add ${title.toLowerCase().replace(/s$/, '')}</button></p>
  </section>`;
}

const num = (extra = {}) => (a) => html`<${Num} ...${a} ...${extra} cls="cell" />`;
const text = (extra = {}) => (a) => html`<${Text} ...${a} ...${extra} cls="cell" />`;
const select = (options, def) => (a) => html`<${Select} ...${a} options=${options} def=${def} />`;
const check = (a) => html`<${Check} ...${a} />`;

const VIDEO = [
  { key: 'id', label: 'Id', cell: text() },
  { key: 'codec', label: 'Codec', cell: select(['h264', 'hevc', 'av1'], 'h264') },
  { key: 'profile', label: 'Profile', cell: (a) => select(PROFILES[a.row.codec || 'h264'], 'default')(a) },
  { key: 'bitDepth', label: 'Bit depth', cell: (a) => ((a.row.codec || 'h264') === 'av1' ? select([8, 10], '8')(a) : '') },
  { key: 'width', label: 'Width', cell: num({ step: 2 }) },
  { key: 'height', label: 'Height', cell: num({ step: 2 }) },
  { key: 'bitrateKbps', label: 'kbit/s', cell: num({ step: 1 }) },
];
const AUDIO = [
  { key: 'id', label: 'Id', cell: text() },
  { key: 'lang', label: 'Lang', cell: text({ placeholder: 'en' }) },
  { key: 'name', label: 'Name', cell: text() },
  { key: 'channels', label: 'Channels', cell: select([1, 2, 6]) },
  { key: 'codec', label: 'Codec', cell: select(['aac', 'ac3', 'eac3', 'opus'], 'aac') },
  { key: 'bitrateKbps', label: 'kbit/s', cell: num({ step: 1 }) },
  { key: 'group', label: 'Group', cell: text({ placeholder: 'main' }) },
  { key: 'default', label: 'Default', cell: check },
];
const SUBTITLES = [
  { key: 'id', label: 'Id', cell: text() },
  { key: 'lang', label: 'Lang', cell: text({ placeholder: 'en' }) },
  { key: 'name', label: 'Name', cell: text() },
  { key: 'format', label: 'Format', cell: select(['webvtt', 'imsc1']) },
  { key: 'default', label: 'Default', cell: check },
];

// next returns an id for a new row: base, then base-2, base-3, ...
function nextId(rows, base) {
  const ids = new Set(rows.map((r) => r.id));
  let id = base;
  for (let k = 2; ids.has(id); k++) id = `${base}-${k}`;
  return id;
}

function Fields(props) {
  const { doc, set, isNew } = props;
  const spec = doc.spec ?? {};
  const fps = spec.fps ?? 25;
  const durations = alignedDurations(fps, spec.audio);
  const seg = spec.segmentSeconds ?? 2;
  const hls = (spec.formats ?? []).includes('hls');
  const dash = (spec.formats ?? []).includes('dash');
  const ads = spec.ads ?? {};
  const recurring = Boolean(ads.recurring);
  return html`
    <section class="fields">
      ${isNew && html`<${Text} ...${props} p=${['id']} label="Id" placeholder="demo" />`}
      <${Text} ...${props} p=${['name']} label="Name" placeholder="Demo channel" />
      <${Multi} ...${props} p=${['spec', 'formats']} label="Formats" options=${['hls', 'dash']} />
    </section>
    <section class="fields">
      <h2>Timing</h2>
      <${Select} ...${props} p=${['spec', 'fps']} label="fps" options=${[24, 25, 30, 50, 60]} def="25" />
      <label>Segment (s) <input type="number" step="any" list="aligned" placeholder="2" value=${spec.segmentSeconds ?? ''}
        onInput=${(e) => {
          const v = e.target.value === '' ? undefined : Number(e.target.value);
          set(['spec', 'segmentSeconds'], v);
          // a GOP of one segment, as the API stores the default, follows it
          if (spec.gopSeconds === spec.segmentSeconds) set(['spec', 'gopSeconds'], v);
        }} />
        <datalist id="aligned">${durations.map((d) => html`<option value=${d} />`)}</datalist>
        <${Err} fieldErrors=${props.fieldErrors} p=${['spec', 'segmentSeconds']} /></label>
      <span class=${aligned(seg, spec.audio) ? 'ok' : 'muted'}>${aligned(seg, spec.audio) ? 'audio-aligned' : 'audio segments vary by a frame (valid)'}</span>
      <span class="muted">Audio-aligned at ${fps} fps: ${durations.length ? durations.slice(0, 8).join(', ') + (durations.length > 8 ? ', …' : '') + ' s' : 'none from 0.5 to 10 s'}</span>
      <${Num} ...${props} p=${['spec', 'gopSeconds']} label="GOP (s)" placeholder="segment" />
      <${Text} ...${props} p=${['spec', 'timezone']} label="Clock zone" placeholder="UTC" />
    </section>
    <${Rows} ...${props} name="video" title="Video renditions" columns=${VIDEO}
      blank=${(rows) => ({ ...(rows.at(-1) ?? { codec: 'h264', width: 1280, height: 720, bitrateKbps: 3000 }), id: nextId(rows, 'video') })} />
    <${Rows} ...${props} name="audio" title="Audio tracks" columns=${AUDIO}
      blank=${(rows) => ({ id: nextId(rows, 'audio'), lang: 'en', channels: 2, codec: 'aac', bitrateKbps: 128 })} />
    <${Rows} ...${props} name="subtitles" title="Subtitles" columns=${SUBTITLES}
      blank=${(rows) => ({ id: nextId(rows, 'sub'), lang: 'en', format: 'webvtt' })} />
    <section class="fields">
      <h2>CEA-608 captions</h2>
      <label>Channel <select value=${spec.captions?.cea608?.channel ?? (spec.captions?.cea608 ? 'CC1' : '')} onChange=${(e) => set(['spec', 'captions'],
        e.target.value ? { cea608: { lang: 'en', ...spec.captions?.cea608, channel: e.target.value } } : undefined)}>
        <option value="">none</option>${['CC1', 'CC2', 'CC3', 'CC4'].map((c) => html`<option>${c}</option>`)}
      </select><${Err} fieldErrors=${props.fieldErrors} p=${['spec', 'captions', 'cea608', 'channel']} /></label>
      ${spec.captions?.cea608 && html`<${Text} ...${props} p=${['spec', 'captions', 'cea608', 'lang']} label="Lang" placeholder="en" />`}
      <span class="muted">The subtitle text, inside the video as pop-on captions.</span>
    </section>
    <section class="fields">
      <h2>Ads</h2>
      <${Num} ...${props} p=${['spec', 'ads', 'leadSeconds']} label="Lead (s)" placeholder="8" />
      <${Select} ...${props} p=${['spec', 'ads', 'command']} label="Command" options=${['splice_insert', 'time_signal']} def="splice_insert" />
      ${ads.command === 'time_signal' && html`<${Select} ...${props} p=${['spec', 'ads', 'segmentationType']} label="Segmentation type"
        options=${['provider_placement_opportunity', 'provider_ad', 'break', 'distributor_placement_opportunity']} def="choose one" />`}
      ${hls && html`<${Multi} ...${props} p=${['spec', 'ads', 'hls']} label="HLS cue styles" options=${['daterange', 'cueout', 'oatcls', 'adobe']} hint="none: daterange" />`}
      ${dash && html`<${Multi} ...${props} p=${['spec', 'ads', 'dash']} label="DASH cue styles" options=${['eventstream-bin', 'eventstream-xml', 'inband']} hint="none: eventstream-bin" />`}
      ${dash && html`<${Check} ...${props} p=${['spec', 'ads', 'dashMultiPeriod']} label="DASH multi-period" />`}
      <label><input type="checkbox" checked=${recurring} onChange=${(e) => set(['spec', 'ads', 'recurring'], e.target.checked ? { everySeconds: 300, durationSeconds: 30, offsetSeconds: 60 } : undefined)} /> Recurring breaks</label>
      ${recurring && html`
        <${Num} ...${props} p=${['spec', 'ads', 'recurring', 'everySeconds']} label="every (s)" />
        <${Num} ...${props} p=${['spec', 'ads', 'recurring', 'durationSeconds']} label="lasting (s)" />
        <${Num} ...${props} p=${['spec', 'ads', 'recurring', 'offsetSeconds']} label="first after (s)" />`}
    </section>
    <section class="fields">
      <h2>Window and DASH</h2>
      <${Num} ...${props} p=${['spec', 'window', 'dvrSeconds']} label="DVR (s)" placeholder="30" />
      <${Num} ...${props} p=${['spec', 'window', 'retainSeconds']} label="Retain (s)" placeholder="300" />
      ${dash && html`<${Select} ...${props} p=${['spec', 'dash', 'addressing']} label="DASH addressing" options=${['number', 'timeline-time', 'timeline-number']} def="timeline-time" />`}
    </section>
    <p class="muted">Overlay, fault rule seeds and anything else: Raw JSON tab.</p>`;
}
