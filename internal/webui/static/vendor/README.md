# Vendored browser libraries

Unmodified files from npm, so the UI needs no build step and no network.

| File | Package | From the tarball |
| --- | --- | --- |
| preact.mjs | preact 11.2.0 | dist/preact.mjs |
| hooks.mjs | preact 11.2.0 | hooks/dist/hooks.mjs |
| htm.mjs | htm 3.1.1 | dist/htm.mjs |
| hls.min.mjs | hls.js 1.7.4 | dist/hls.min.mjs |
| dash.mediaplayer.min.js | dashjs 5.3.0 | dist/modern/esm/dash.mediaplayer.min.js |

To update one: `curl -sL https://registry.npmjs.org/<pkg>/-/<pkg>-<version>.tgz | tar xz`,
copy the file over, and change the version here. The import map in
../index.html maps `preact`, `preact/hooks` and `htm` to these files.
