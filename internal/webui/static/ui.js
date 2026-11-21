// Shared view helpers.
import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';

export const html = htm.bind(h);

export function navigate(path) {
  history.pushState(null, '', path);
  dispatchEvent(new PopStateEvent('popstate'));
}

// Link navigates without a page load, unless a modifier key asks for a tab.
export function Link({ href, children, ...rest }) {
  const click = (e) => {
    if (e.button || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    navigate(href);
  };
  return html`<a href=${href} onClick=${click} ...${rest}>${children}</a>`;
}

export const Badge = ({ state }) => html`<span class="badge ${state}">${state}</span>`;

export function ErrorBox({ error }) {
  if (!error) return null;
  return html`<div class="error" role="alert">${error.message}
    ${error.fields?.length > 0 && html`<ul>${error.fields.map((f) => html`<li><code>${f.field}</code> ${f.message}</li>`)}</ul>`}
  </div>`;
}

// useNow re-renders every ms milliseconds and returns Date.now() + offset.
export function useNow(ms, offset = 0) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now + offset;
}

// useAction runs fn, keeping its error for an ErrorBox and a busy flag for
// the buttons.
export function useAction() {
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const run = async (fn) => {
    setBusy(true);
    setError(null);
    try {
      return await fn();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };
  return { error, busy, run, setError };
}

// clock formats a time as UTC hh:mm:ss, or hh:mm:ss.mmm with ms.
export function clock(t, ms = false) {
  if (t === undefined || t === null) return '';
  return new Date(t).toISOString().slice(11, ms ? 23 : 19);
}

export function duration(ms) {
  let s = Math.max(0, Math.floor(ms / 1000));
  const d = Math.floor(s / 86400);
  s %= 86400;
  const hms = [Math.floor(s / 3600), Math.floor((s % 3600) / 60), s % 60].map((v) => String(v).padStart(2, '0')).join(':');
  return d ? `${d}d ${hms}` : hms;
}

// ladder reads "3 video · 3 audio · 2 subs".
export function ladder(spec) {
  const parts = [`${spec.video.length} video`, `${spec.audio.length} audio`];
  if (spec.subtitles?.length) parts.push(`${spec.subtitles.length} subs`);
  return parts.join(' · ');
}

// liveEdge is timeline.LiveEdge: the last segment n with AST+(n+1)S <= now,
// capped at the end segment, -1 before the first.
export function liveEdge(astMs, segSeconds, endSegment, now) {
  let n = Math.floor((now - astMs) / (segSeconds * 1000)) - 1;
  if (endSegment !== null && endSegment !== undefined) n = Math.min(n, endSegment);
  return Math.max(n, -1);
}

export function hex(b64) {
  return [...atob(b64)].map((c) => c.charCodeAt(0).toString(16).padStart(2, '0')).join('');
}

export const streamPath = (id) => '/streams/' + encodeURIComponent(id);

// useCached returns {value} or {error} of load() once it settles, undefined
// before, and keeps it by key for the page's lifetime, so load runs once
// per key. A null key loads nothing.
// ponytail: never evicts, one entry per cue or signal clicked; an LRU if
// pages stay open for days.
const pending = new Map();
const settled = new Map();
export function useCached(key, load) {
  const [, rerender] = useState(0);
  useEffect(() => {
    if (key === null || settled.has(key)) return;
    if (!pending.has(key)) {
      pending.set(key, load().then((value) => ({ value }), (error) => ({ error })).then((r) => settled.set(key, r)));
    }
    pending.get(key).then(() => rerender((n) => n + 1));
  }, [key]);
  return key === null ? undefined : settled.get(key);
}
