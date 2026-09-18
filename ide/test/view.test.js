// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
const { test, expect } = require('bun:test');
const fs = require('node:fs');
const { renderView } = require('../src/view');
test('packaged document uses nonce bridge first, local assets, no write surfaces or login', () => {
  const html = renderView(fs.readFileSync('../internal/web/assets/index.html', 'utf8'), { resource: p => 'https://resource.test/' + p, cspSource: 'https://resource.test' });
  expect(html).toContain("connect-src &#39;none&#39;");
  expect(html).toContain("base-uri &#39;none&#39;");
  expect(html).not.toMatch(/<base\b/);
  expect(html).not.toMatch(/data-view="(?:ask|review|settings|api)"/);
  expect(html).not.toContain('id="login-form"');
  expect(html).not.toContain('__MESH_SOURCE__');
  const scripts = [...html.matchAll(/<script nonce="([^"]+)" src="([^"]+)" defer><\/script>/g)];
  expect(scripts.length).toBe(7);
  expect(new Set(scripts.map(s => s[1])).size).toBe(1);
  expect(scripts[0][2]).toBe('https://resource.test/src/bridge.js');
  for (const s of scripts) expect(fs.existsSync(s[2].replace('https://resource.test/', ''))).toBe(true);
});
test('packaged fonts remain within the extension and default browser search stays debounced', () => {
  expect(fs.readFileSync('media/style.css', 'utf8')).not.toContain('/assets/fonts/');
  const search = fs.readFileSync('media/search.js', 'utf8');
  expect(search).toContain('if (M.searchOnSubmit) return;');
  expect(search).toContain('timer = setTimeout(run, 250)');
});
