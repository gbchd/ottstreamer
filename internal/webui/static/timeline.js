// The timeline: one SVG lane per event group on a shared time axis, with the
// DVR window, the live edge and the frozen horizon.
import { useEffect, useRef, useState } from 'preact/hooks';
import { html, clock } from './ui.js';

export const LANES = ['stream', 'break', 'signal', 'fault', 'health'];

export function lane(type) {
  const g = type.split('.')[0];
  if (g === 'faults') return 'fault';
  if (g === 'segment' || g === 'ffmpeg' || g === 'runtime') return 'health';
  return LANES.includes(g) ? g : 'stream';
}

const ZOOMS = [
  { label: '2 min', ms: 120_000 },
  { label: '30 min', ms: 1_800_000 },
  { label: 'Epoch', ms: 0 },
];
const STEPS = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 21600].map((s) => s * 1000);
const LABEL = 70;
const LANE = 26;
const AXIS = 20;

// Timeline draws events and breaks. m holds the markers in ms and segments
// (ast, seg, edge, horizon, dvrStart), already advanced to now. onRange
// asks for the data of a new view [from, to]; onPick gets a clicked break
// or event.
export function Timeline({ m, now, events, breaks, onRange, onPick }) {
  const box = useRef(null);
  const [width, setWidth] = useState(800);
  const [zoom, setZoom] = useState(0);
  const [end, setEnd] = useState(null); // view end when not following the live edge
  const drag = useRef(null);
  useEffect(() => {
    const ro = new ResizeObserver(([e]) => setWidth(Math.max(300, e.contentRect.width)));
    ro.observe(box.current);
    return () => ro.disconnect();
  }, []);

  const edgeAt = m ? m.ast + (m.edge + 1) * m.seg : now;
  const span = ZOOMS[zoom].ms || Math.max(60_000, (edgeAt - (m?.ast ?? now)) * 1.1);
  const to = end ?? edgeAt + span * 0.2;
  const from = to - span;
  const plot = width - LABEL;
  const x = (t) => LABEL + ((t - from) / span) * plot;
  const segAt = (n) => m.ast + n * m.seg;

  // Ask for data on a new zoom, or once the view scrolls back past what was
  // asked for; the event stream keeps the live end current.
  const asked = useRef(null);
  useEffect(() => {
    const a = asked.current;
    if (drag.current || (a && a.span === span && from >= a.from)) return;
    asked.current = { from: from - span / 2, span };
    onRange(from - span / 2, to + span / 2);
  });

  const down = (e) => { drag.current = { x: e.clientX, to }; };
  const move = (e) => {
    const d = drag.current;
    if (!d || !(e.buttons & 1)) return;
    if (Math.abs(e.clientX - d.x) > 3) setEnd(d.to - ((e.clientX - d.x) / plot) * span);
  };
  const up = () => { drag.current = null; };

  const step = STEPS.find((s) => (s / span) * plot >= 90) ?? STEPS.at(-1);
  const ticks = [];
  for (let t = Math.ceil(from / step) * step; t <= to; t += step) ticks.push(t);
  const height = AXIS + LANES.length * LANE;
  const y = (l) => AXIS + LANES.indexOf(l) * LANE;
  const visible = events.filter((e) => {
    const t = Date.parse(e.at);
    return t >= from && t <= to;
  });

  return html`<div class="timeline" ref=${box}>
    <div class="controls">
      ${ZOOMS.map((z, i) => html`<button class=${i === zoom ? 'on' : ''} onClick=${() => { setZoom(i); setEnd(null); }}>${z.label}</button>`)}
      <button disabled=${end === null} onClick=${() => setEnd(null)}>Follow live</button>
      <span class="muted">drag to scroll back · times in UTC</span>
    </div>
    <svg width=${width} height=${height} onPointerDown=${down} onPointerMove=${move} onPointerUp=${up} onPointerLeave=${up}>
      ${LANES.map((l, i) => html`<g key=${l}>
        <rect class=${i % 2 ? 'lane odd' : 'lane'} x="0" y=${y(l)} width=${width} height=${LANE} />
        <text class="lane-label" x="6" y=${y(l) + 17}>${l}</text>
      </g>`)}
      ${ticks.map((t) => html`<g key=${t}>
        <line class="tick" x1=${x(t)} x2=${x(t)} y1=${AXIS - 4} y2=${height} />
        <text class="tick-label" x=${x(t) + 3} y="12">${clock(t)}</text>
      </g>`)}
      ${m && html`<rect class="dvr" x=${x(segAt(m.dvrStart))} y=${AXIS} width=${Math.max(0, x(edgeAt) - x(segAt(m.dvrStart)))} height=${height - AXIS}><title>DVR window: segments ${m.dvrStart}-${m.edge}</title></rect>`}
      ${breaks.map((b) => {
        const x0 = x(Date.parse(b.start));
        const w = Math.max(2, x(Date.parse(b.end)) - x0);
        const cls = ['break', b.cancelled && 'cancelled', b.recurring && 'recurring'].filter(Boolean).join(' ');
        return html`<rect key=${b.id} class=${cls} x=${x0} y=${y('break') + 4} width=${w} height=${LANE - 8} onClick=${() => onPick({ break: b })}>
          <title>break ${b.id}${b.recurring ? ' (recurring)' : ''}${b.cancelled ? ' (cancelled)' : ''}: ${clock(b.start)}-${clock(b.end)}, segments ${b.startSegment}-${b.endSegment - 1}</title>
        </rect>`;
      })}
      ${visible.map((e) => html`<circle key=${e.id} class=${'ev ' + lane(e.type)} cx=${x(Date.parse(e.at))} cy=${y(lane(e.type)) + LANE / 2} r="5" onClick=${() => onPick({ event: e })}>
        <title>${clock(e.at, true)} ${e.type}${e.segment !== null ? ` · segment ${e.segment}` : ''} · ${e.actor}\n${JSON.stringify(e.data)}</title>
      </circle>`)}
      ${m && html`<line class="edge" x1=${x(edgeAt)} x2=${x(edgeAt)} y1=${AXIS} y2=${height}><title>live edge: segment ${m.edge}</title></line>`}
      ${m && html`<line class="horizon" x1=${x(segAt(m.horizon + 1))} x2=${x(segAt(m.horizon + 1))} y1=${AXIS} y2=${height}><title>frozen horizon: segment ${m.horizon}; a new break starts after it</title></line>`}
    </svg>
  </div>`;
}
