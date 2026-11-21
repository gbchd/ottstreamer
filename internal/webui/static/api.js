// The /api client: Bearer key from sessionStorage, JSON in and out, and the
// event stream read with fetch, since EventSource cannot send Authorization.

const KEY = 'ottstreamer.key';

export const apiKey = {
  get: () => sessionStorage.getItem(KEY) || '',
  set: (k) => sessionStorage.setItem(KEY, k),
  clear: () => sessionStorage.removeItem(KEY),
};

// unauthorized is called on a 401, so the app can ask for a key again.
let unauthorized = () => {};
export const onUnauthorized = (fn) => { unauthorized = fn; };

export class APIError extends Error {
  constructor(status, body) {
    super(body?.error || `HTTP ${status}`);
    this.status = status;
    this.fields = body?.fields || [];
  }
}

function headers(extra) {
  return { Authorization: `Bearer ${apiKey.get()}`, ...extra };
}

// api calls /api + path. body is sent as JSON; the reply is parsed as JSON,
// or returned as text when it is not JSON.
export async function api(path, { method = 'GET', body, headers: extra = {} } = {}) {
  const res = await fetch('/api' + path, {
    method,
    headers: headers({ ...extra, ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) }),
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 401) unauthorized();
  if (!res.ok) throw new APIError(res.status, await res.json().catch(() => null));
  if (res.status === 204) return null;
  const type = res.headers.get('Content-Type') || '';
  return type.startsWith('application/json') ? res.json() : res.text();
}

// clockOffset is server time minus local time, in ms, from /api/time
// (without ?stream=, so no fault skew applies).
export async function clockOffset() {
  const t0 = Date.now();
  const res = await fetch('/api/time');
  const t1 = Date.now();
  return Date.parse(await res.text()) - (t0 + t1) / 2;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// followEvents calls onEvent with each event of stream id after since, and
// reconnects from the last one it saw until signal aborts.
export async function followEvents(id, since, onEvent, signal) {
  while (!signal.aborted) {
    try {
      const q = since ? `?since=${encodeURIComponent(since)}` : '';
      const res = await fetch(`/api/streams/${encodeURIComponent(id)}/events/live${q}`, {
        headers: headers({ Accept: 'text/event-stream' }),
        signal,
      });
      if (res.status === 401) unauthorized();
      if (!res.ok) throw new APIError(res.status, null);
      const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
      let buf = '';
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += value.replaceAll('\r\n', '\n');
        let i;
        while ((i = buf.indexOf('\n\n')) >= 0) {
          const e = parseEvent(buf.slice(0, i));
          buf = buf.slice(i + 2);
          if (e) {
            since = e.id;
            onEvent(e);
          }
        }
      }
    } catch (err) {
      if (signal.aborted) return;
      console.warn('event stream', err);
    }
    await sleep(2000);
  }
}

// parseEvent reads one SSE block; comments (pings) give null.
function parseEvent(block) {
  const data = [];
  for (const line of block.split('\n')) {
    if (line.startsWith('data:')) data.push(line.slice(line.startsWith('data: ') ? 6 : 5));
  }
  return data.length ? JSON.parse(data.join('\n')) : null;
}
