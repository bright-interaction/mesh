'use strict';
const { randomBytes } = require('node:crypto');
const escape = value => String(value).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
function renderView(template, { resource, cspSource }) {
  const nonce = randomBytes(24).toString('base64');
  // Graph labels use inline style attributes. Only styles allow inline content;
  // scripts remain nonce-only, and content cannot make any network requests.
  const csp = `default-src 'none'; script-src 'nonce-${nonce}'; style-src ${cspSource} 'unsafe-inline'; img-src ${cspSource} data:; font-src ${cspSource}; connect-src 'none'; base-uri 'none'; form-action 'none'; frame-src 'none';`;
  let html = template.replace('<meta charset="utf-8">', `<meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="${escape(csp)}">`)
    .replace(/<base\b[^>]*>/, '')
    .replace(/<li><button class="nav" data-view="(?:ask|review|settings|api)"[\s\S]*?<\/li>/g, '')
    .replace(/<section class="panel-view" data-panel="(?:ask|review|settings|api)"[^>]*><\/section>/g, '')
    .replace(/<script src="assets\/(?:ask|review|settings|api)\.js" defer><\/script>/g, '')
    .replace(/<div id="login"[\s\S]*?<\/form>\s*<\/div>/, '')
    .replace('__MESH_SOURCE__', 'Mesh IDE · read-only viewer')
    .replace('href="assets/style.css"', `href="${escape(resource('media/style.css'))}"`)
    .replace('</head>', `<link rel="stylesheet" href="${escape(resource('src/view.css'))}"></head>`)
    .replace(/<script src="assets\/([a-z0-9]+\.js)" defer><\/script>/g, (_, name) => `<script nonce="${nonce}" src="${escape(resource('media/' + name))}" defer></script>`);
  html = html.replace('<script nonce=', `<script nonce="${nonce}" src="${escape(resource('src/bridge.js'))}" defer></script>\n<script nonce=`);
  return html;
}
module.exports = { renderView };
