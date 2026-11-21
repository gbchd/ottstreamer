// The app shell: API key login, routing and the page header.
import { render } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import { api, apiKey, onUnauthorized } from './api.js';
import { html, Link, ErrorBox } from './ui.js';
import { Streams } from './streams.js';
import { Detail } from './detail.js';
import { StreamForm } from './form.js';

function usePath() {
  const [path, setPath] = useState(location.pathname);
  useEffect(() => {
    const f = () => setPath(location.pathname);
    addEventListener('popstate', f);
    return () => removeEventListener('popstate', f);
  }, []);
  return path;
}

function Login({ onLogin, error }) {
  const [key, setKey] = useState('');
  const submit = (e) => {
    e.preventDefault();
    apiKey.set(key.trim());
    onLogin();
  };
  return html`<form class="login" onSubmit=${submit}>
    <h1>ottstreamer</h1>
    <label>API key <input type="password" autocomplete="current-password" required value=${key} onInput=${(e) => setKey(e.target.value)} autofocus /></label>
    <button type="submit">Sign in</button>
    <p class="muted">Kept in this tab's session storage and sent as a Bearer token.</p>
    <${ErrorBox} error=${error} />
  </form>`;
}

function App() {
  const path = usePath();
  const [me, setMe] = useState(null);
  const [error, setError] = useState(null);
  const [checking, setChecking] = useState(Boolean(apiKey.get()));
  const check = async () => {
    setChecking(true);
    try {
      setMe(await api('/me'));
      setError(null);
    } catch (e) {
      apiKey.clear();
      setMe(null);
      setError(e);
    } finally {
      setChecking(false);
    }
  };
  useEffect(() => {
    onUnauthorized(() => {
      apiKey.clear();
      setMe(null);
      setError(new Error('The API key was refused.'));
    });
    if (apiKey.get()) check();
  }, []);
  if (checking) return html`<p class="muted">Checking the API key…</p>`;
  if (!me) return html`<${Login} onLogin=${check} error=${error} />`;
  const signOut = () => {
    apiKey.clear();
    setMe(null);
  };
  const m = path.match(/^\/ui\/streams\/([^/]+)(\/edit)?\/?$/);
  let page = html`<${Streams} me=${me} />`;
  if (m?.[1] === 'new' && !m[2]) page = html`<${StreamForm} key="new" me=${me} />`;
  else if (m?.[2]) page = html`<${StreamForm} key=${'edit-' + m[1]} id=${decodeURIComponent(m[1])} me=${me} />`;
  else if (m) page = html`<${Detail} key=${m[1]} id=${decodeURIComponent(m[1])} me=${me} />`;
  return html`
    <header class="top">
      <${Link} href="/ui/" class="brand">ottstreamer</${Link}>
      <span class="muted">${me.name} · ${me.role}</span>
      <button class="link" onClick=${signOut}>Sign out</button>
    </header>
    <main>
      ${page}
    </main>`;
}

render(html`<${App} />`, document.getElementById('app'));
