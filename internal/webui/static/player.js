// The embedded player: hls.js or dash.js, loaded on first use, with
// rendition, audio and subtitle pickers and a log of what the player reports.
import { useEffect, useRef, useState } from 'preact/hooks';
import { html, clock } from './ui.js';

const kbps = (bps) => Math.round(bps / 1000);
const SCTE35 = ['urn:scte:scte35:2013:bin', 'urn:scte:scte35:2014:xml+bin', 'urn:scte:scte35:2013:xml'];

// Each start function plays url in video and returns the same controls:
// levels/audio/text give {list: [label], current: index} (-1: auto or off),
// set* pick one, destroy stops.

async function startHls(video, url, { update, log }) {
  const { default: Hls } = await import('./vendor/hls.min.mjs');
  if (!Hls.isSupported()) throw new Error('This browser has no Media Source Extensions for hls.js.');
  const hls = new Hls({ subtitleDisplay: true });
  const E = Hls.Events;
  const announced = new Map(); // DATERANGE id -> what was logged
  const reached = new Set();
  let details = null;
  hls.on(E.MANIFEST_PARSED, update);
  hls.on(E.LEVEL_SWITCHED, (_, d) => {
    const l = hls.levels[d.level];
    log(`rendition ${l?.height}p ${kbps(l?.bitrate)} kbit/s ${l?.videoCodec ?? ''}`);
    update();
  });
  hls.on(E.AUDIO_TRACKS_UPDATED, update);
  hls.on(E.AUDIO_TRACK_SWITCHED, (_, d) => {
    log(`audio ${hls.audioTracks[d.id]?.name ?? d.id}`);
    update();
  });
  hls.on(E.SUBTITLE_TRACKS_UPDATED, update);
  let subs = null;
  hls.on(E.SUBTITLE_TRACK_SWITCH, (_, d) => {
    if (d.id !== subs) log(`subtitles ${d.id < 0 ? 'off' : hls.subtitleTracks[d.id]?.name ?? d.id}`);
    subs = d.id;
    update();
  });
  hls.on(E.LEVEL_UPDATED, (_, d) => {
    details = d.details;
    for (const [id, dr] of Object.entries(details.dateRanges ?? {})) {
      const scte = ['SCTE35-OUT', 'SCTE35-IN', 'SCTE35-CMD'].filter((k) => dr.attr?.[k]).join(', ');
      const text = `DATERANGE ${id}: start ${clock(dr.startDate, true)}${dr.plannedDuration ? `, planned ${dr.plannedDuration} s` : ''}${dr.endDate ? `, end ${clock(dr.endDate, true)}` : ''}${scte ? `, ${scte}` : ''}`;
      if (announced.get(id) !== text) {
        announced.set(id, text);
        log(text);
      }
    }
  });
  hls.on(E.FRAG_CHANGED, (_, { frag }) => {
    for (const [tag, value] of frag.tagList ?? []) {
      if (/CUE|SCTE|OATCLS|GAP/.test(tag)) log(`segment ${frag.sn}: ${tag}${value ? ':' + value : ''}`);
    }
    for (const [id, dr] of Object.entries(details?.dateRanges ?? {})) {
      if (!reached.has(id) && frag.programDateTime >= dr.startDate.getTime()) {
        reached.add(id);
        log(`segment ${frag.sn}: playhead reached DATERANGE ${id}`);
      }
    }
  });
  hls.on(E.ERROR, (_, d) => log(`${d.fatal ? 'fatal ' : ''}error: ${d.details}${d.response?.code ? ` (HTTP ${d.response.code})` : ''}`));
  hls.loadSource(url);
  hls.attachMedia(video);
  return {
    levels: () => ({
      list: hls.levels.map((l) => `${l.height}p ${kbps(l.bitrate)} kbit/s ${l.videoCodec ?? ''}`),
      current: hls.autoLevelEnabled ? -1 : hls.currentLevel,
    }),
    setLevel: (i) => { hls.currentLevel = i; },
    audio: () => ({ list: hls.audioTracks.map((t) => `${t.name} (${t.lang}, ${t.audioCodec ?? ''})`), current: hls.audioTrack }),
    setAudio: (i) => { hls.audioTrack = i; },
    text: () => ({ list: hls.subtitleTracks.map((t) => `${t.name} (${t.lang})`), current: hls.subtitleTrack }),
    setText: (i) => { hls.subtitleTrack = i; },
    destroy: () => hls.destroy(),
  };
}

async function startDash(video, url, ttml, { update, log }) {
  const { MediaPlayer } = await import('./vendor/dash.mediaplayer.min.js');
  const p = MediaPlayer().create();
  const E = MediaPlayer.events;
  for (const scheme of SCTE35) {
    const show = (what) => ({ event: ev }) =>
      log(`${scheme} ${what}: id ${ev.id}, at ${(ev.calculatedPresentationTime ?? ev.presentationTime)?.toFixed?.(3)} s, ${ev.duration ? `${ev.duration} s` : 'no duration'}`);
    p.on(scheme, show('received'), null, { mode: E.EVENT_MODE_ON_RECEIVE });
    p.on(scheme, show('started'), null, { mode: E.EVENT_MODE_ON_START });
  }
  p.on(E.STREAM_INITIALIZED, update);
  p.on(E.QUALITY_CHANGE_RENDERED, (e) => {
    const r = e.newRepresentation;
    log(`${e.mediaType} rendition ${r?.id ?? ''} ${r?.bandwidth ? kbps(r.bandwidth) + ' kbit/s' : ''}`);
    update();
  });
  p.on(E.TRACK_CHANGE_RENDERED, (e) => {
    log(`${e.mediaType} track ${e.newMediaInfo?.lang ?? ''} ${e.newMediaInfo?.id ?? ''}`);
    update();
  });
  p.on(E.ERROR, (e) => log(`error: ${e.error?.message ?? e.error?.code ?? 'unknown'}`));
  p.initialize(video, url, true);
  p.attachTTMLRenderingDiv(ttml);
  const reps = () => p.getRepresentationsByType('video') ?? [];
  const tracks = (type) => p.getTracksFor(type) ?? [];
  const label = (t) => `${t.labels?.[0]?.text ?? t.id ?? ''} (${t.lang}, ${t.codec ?? ''})`;
  let auto = true;
  return {
    levels: () => {
      const cur = p.getCurrentRepresentationForType('video');
      const list = reps();
      return { list: list.map((r) => `${r.id} ${r.height}p ${kbps(r.bandwidth)} kbit/s`), current: auto ? -1 : list.findIndex((r) => r.id === cur?.id) };
    },
    setLevel: (i) => {
      auto = i < 0;
      p.updateSettings({ streaming: { abr: { autoSwitchBitrate: { video: auto } } } });
      if (!auto) p.setRepresentationForTypeById('video', reps()[i].id, true);
      update();
    },
    audio: () => {
      const cur = p.getCurrentTrackFor('audio');
      const list = tracks('audio');
      return { list: list.map(label), current: list.findIndex((t) => t.id === cur?.id) };
    },
    setAudio: (i) => p.setCurrentTrack(tracks('audio')[i]),
    text: () => ({ list: tracks('text').map(label), current: p.getCurrentTextTrackIndex() }),
    setText: (i) => {
      p.enableText(i >= 0);
      if (i >= 0) p.setTextTrack(i);
      log(`subtitles ${i < 0 ? 'off' : label(tracks('text')[i])}`);
      update();
    },
    destroy: () => p.reset(),
  };
}

function Picker({ label, state, set, none }) {
  if (!state || state.list.length === 0) return null;
  return html`<label>${label} <select value=${state.current} onChange=${(e) => set(Number(e.target.value))}>
    ${none && html`<option value="-1">${none}</option>`}
    ${state.list.map((l, i) => html`<option value=${i}>${l}</option>`)}
  </select></label>`;
}

// Player plays urls[format]; formats lists the ones the stream serves.
export function Player({ urls, formats }) {
  const [format, setFormat] = useState(formats[0]);
  const [reload, setReload] = useState(0);
  const [, setTick] = useState(0);
  const [log, setLog] = useState([]);
  const [error, setError] = useState(null);
  const video = useRef(null);
  const ttml = useRef(null);
  const ctl = useRef(null);
  const url = urls[format];
  useEffect(() => {
    let live = true;
    const cb = {
      update: () => live && setTick((t) => t + 1),
      log: (msg) => live && setLog((l) => [{ at: Date.now(), msg }, ...l].slice(0, 200)),
    };
    setError(null);
    cb.log(`${format}: ${url}`);
    (format === 'hls' ? startHls(video.current, url, cb) : startDash(video.current, url, ttml.current, cb))
      .then((c) => { if (live) ctl.current = c; else c.destroy(); })
      .catch((e) => live && setError(e));
    return () => {
      live = false;
      ctl.current?.destroy();
      ctl.current = null;
    };
  }, [format, url, reload]);
  const c = ctl.current;
  return html`<div class="player">
    <div class="video-box">
      <video ref=${video} controls muted playsinline></video>
      <div class="ttml" ref=${ttml}></div>
    </div>
    <div class="controls">
      ${formats.length > 1 && html`<label>Player <select value=${format} onChange=${(e) => setFormat(e.target.value)}>
        ${formats.map((f) => html`<option value=${f}>${f === 'hls' ? 'hls.js' : 'dash.js'}</option>`)}
      </select></label>`}
      <${Picker} label="Rendition" state=${c?.levels()} set=${(i) => c.setLevel(i)} none="auto" />
      <${Picker} label="Audio" state=${c?.audio()} set=${(i) => c.setAudio(i)} />
      <${Picker} label="Subtitles" state=${c?.text()} set=${(i) => c.setText(i)} none="off" />
      <button onClick=${() => setReload((r) => r + 1)}>Reload</button>
    </div>
    ${error && html`<div class="error">${error.message}</div>`}
    <ol class="player-log">${log.map((l) => html`<li><span class="muted">${clock(l.at, true)}</span> ${l.msg}</li>`)}</ol>
  </div>`;
}
