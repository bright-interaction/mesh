'use strict';
const http = require('node:http');
const MAX_BYTES = 16 * 1024 * 1024;
function viewerURL(value) {
  if (typeof value !== 'string' || value.length > 512) throw new Error('Invalid local viewer URL');
  const u = new URL(value);
  if (u.protocol !== 'http:' || !['127.0.0.1', '[::1]'].includes(u.hostname) || u.username || u.password || u.search || u.hash || !/^\/(?:[a-zA-Z0-9_-]+\/)*[a-zA-Z0-9_-]*$/.test(u.pathname)) throw new Error('Use an HTTP loopback viewer URL without credentials');
  return u.origin + u.pathname.replace(/\/$/, '');
}
function readPath(raw) {
  if (typeof raw !== 'string' || raw.length > 4096 || /[\\#\s]/.test(raw) || raw.startsWith('//')) throw new Error('Unsupported viewer request');
  const p = raw.startsWith('/') ? raw : '/' + raw;
  const u = new URL(p, 'http://mesh.invalid');
  // Compare before URL normalization so /a/../api/status cannot pass.
  if (u.origin !== 'http://mesh.invalid' || p.split('?')[0] !== u.pathname) throw new Error('Unsupported viewer path');
  const fixed = ['/graph.json', '/api/status', '/api/dashboard', '/api/docs'];
  const segment = /^\/api\/(?:note|docs)\/([^/]+)$/.exec(u.pathname);
  if (segment) {
    const id = decodeURIComponent(segment[1]);
    if (!id || id.length > 512 || /[\\/\x00-\x20%?#]/.test(id) || id === '.' || id === '..') throw new Error('Invalid note identifier');
  }
  if (u.pathname === '/api/search') {
    const keys = [...u.searchParams.keys()];
    if (new Set(keys).size !== keys.length || keys.some(k => !['q', 'limit', 'budget'].includes(k))) throw new Error('Invalid search options');
    const q = u.searchParams.get('q');
    if (!q?.trim() || Buffer.byteLength(q) > 2048) throw new Error('Search for a few words');
    for (const [k, max] of [['limit', 30], ['budget', 8000]]) {
      const v = u.searchParams.get(k);
      if (v !== null && (!/^[1-9][0-9]*$/.test(v) || Number(v) > max)) throw new Error('Search budget exceeded');
    }
  } else if ((!fixed.includes(u.pathname) && !segment) || u.search) throw new Error('Unsupported read-only endpoint');
  return u.pathname + u.search;
}
function getJSON(url, signal, { timeout = 15000, maxBytes = MAX_BYTES } = {}) {
  return new Promise((resolve, reject) => {
    let settled = false, timer, req;
    const finish = (error, value) => {
      if (settled) return;
      settled = true; clearTimeout(timer); signal?.removeEventListener('abort', abort);
      if (error) reject(error); else resolve(value);
    };
    const abort = () => { const error = new Error('Viewer request aborted'); finish(error); req?.destroy(error); };
    if (signal?.aborted) { abort(); return; }
    signal?.addEventListener('abort', abort, { once: true });
    req = http.get(url, { headers: { Accept: 'application/json' }, agent: false }, res => {
      const status = res.statusCode;
      if (status !== 200) { finish(null, { status, body: '{}' }); res.destroy(); req.destroy(); return; }
      if (!/^application\/json(?:;|$)/i.test(res.headers['content-type'] || '')) { finish(new Error('Viewer did not return JSON')); res.destroy(); req.destroy(); return; }
      let size = 0; const chunks = [];
      res.on('data', chunk => { size += chunk.length; if (size > maxBytes) { const error = new Error('Viewer response too large'); finish(error); res.destroy(); req.destroy(); } else chunks.push(chunk); });
      res.on('error', finish);
      res.on('end', () => {
        try { const body = Buffer.concat(chunks).toString('utf8'); JSON.parse(body); finish(null, { status, body }); } catch (_) { finish(new Error('Invalid viewer response')); }
      });
    });
    timer = setTimeout(() => { const error = new Error('Viewer request timed out'); finish(error); req.destroy(error); }, timeout);
    req.once('error', finish);
  });
}
class MeshClient {
  constructor(base, transport = getJSON) { this.base = viewerURL(base); this.transport = transport; this.resolved = null; }
  async request(raw, signal) {
    const route = readPath(raw);
    if (!this.resolved) {
      let base = this.base;
      let status = await this.transport(base + '/api/status', signal);
      if (status.status === 404 && new URL(base).pathname === '/') {
        base += '/app'; status = await this.transport(base + '/api/status', signal);
      }
      if (status.status !== 200) throw new Error('Mesh viewer unavailable or locked. Check its URL in Mesh: Set Local Viewer URL.');
      const data = JSON.parse(status.body);
      if (!data || typeof data.counts !== 'object' || data.counts === null) throw new Error('This endpoint is not a Mesh viewer');
      this.resolved = base;
      if (route === '/api/status') return status;
    }
    return this.transport(this.resolved + route, signal);
  }
}
module.exports = { MeshClient, viewerURL, readPath, getJSON };
