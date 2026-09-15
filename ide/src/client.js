// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const http = require('node:http');
const https = require('node:https');
const MAX_BYTES = 16 * 1024 * 1024;
function viewerURL(value) {
  if (typeof value !== 'string' || value.length > 512 || /[\s\\]/.test(value)) throw new Error('Invalid viewer URL');
  const u = new URL(value);
  if (!(u.protocol === 'https:' || (u.protocol === 'http:' && ['127.0.0.1', '[::1]'].includes(u.hostname))) || u.username || u.password || u.search || u.hash || !/^\/(?:[a-zA-Z0-9_-]+\/)*[a-zA-Z0-9_-]*$/.test(u.pathname) || /(?:^|\/)\.{1,2}(?:\/|$)|%/i.test(value)) throw new Error('Use HTTPS or HTTP numeric loopback, without credentials, query or fragment');
  return u.origin + u.pathname.replace(/\/$/, '');
}
function isRemote(value) { return new URL(viewerURL(value)).protocol === 'https:'; }
function accessKey(value) {
  if (typeof value !== 'string' || !/^[\x21-\x7e]{1,4096}$/.test(value)) throw new Error('Invalid Mesh access key');
  return value;
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
function getJSON(url, signal, { timeout = 15000, maxBytes = MAX_BYTES, token } = {}) {
  return new Promise((resolve, reject) => {
    const target = new URL(url);
    if (!['http:', 'https:'].includes(target.protocol) || target.username || target.password) throw new Error('Invalid viewer transport');
    const headers = { Accept: 'application/json' };
    if (token !== undefined) {
      if (target.protocol !== 'https:') throw new Error('Credentials require HTTPS');
      headers.Authorization = 'Bearer ' + accessKey(token);
    }
    let settled = false, timer, req;
    const finish = (error, value) => {
      if (settled) return;
      settled = true; clearTimeout(timer); signal?.removeEventListener('abort', abort);
      if (error) reject(error); else resolve(value);
    };
    const abort = () => { const error = new Error('Viewer request aborted'); finish(error); req?.destroy(error); };
    if (signal?.aborted) { abort(); return; }
    signal?.addEventListener('abort', abort, { once: true });
    req = (target.protocol === 'https:' ? https : http).get(target, { headers, agent: false }, res => {
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
  async connect(signal) {
    let base = this.base;
    let status = await this.transport(base + '/api/status', signal);
    if (status.status === 404 && new URL(base).pathname === '/') {
      base += '/app'; status = await this.transport(base + '/api/status', signal);
    }
    if (status.status !== 200) throw Object.assign(new Error('Mesh viewer unavailable or locked.'), { code: status.status === 401 ? 'AUTH_REQUIRED' : status.status === 403 ? 'ACCESS_DENIED' : 'UNAVAILABLE' });
    const data = JSON.parse(status.body);
    if (!data || typeof data.counts !== 'object' || data.counts === null || Array.isArray(data.counts)) throw new Error('This endpoint is not a Mesh viewer');
    if (signal?.aborted) throw new Error('Viewer request aborted');
    this.resolved = base;
    return status;
  }
  async request(raw, signal) {
    const route = readPath(raw);
    if (!this.resolved) {
      const status = await this.connect(signal);
      if (route === '/api/status') return status;
    }
    return this.transport(this.resolved + route, signal);
  }
}

// Credentials never enter settings or the webview. Bind each secret to the full
// approved HTTPS base, including its path; never share with Stage or another vault.
class RemoteAuth {
  constructor(secrets, transport = getJSON) { this.secrets = secrets; this.transport = transport; this.writes = Promise.resolve(); }
  write(action) { const result = this.writes.then(action); this.writes = result.catch(() => {}); return result; }
  key(base) { if (!isRemote(base)) throw new Error('Remote sign-in requires HTTPS'); return 'mesh.viewer-key:' + viewerURL(base); }
  client(base, candidate) {
    base = viewerURL(base);
    if (!isRemote(base)) return new MeshClient(base, this.transport);
    const key = this.key(base);
    return new MeshClient(base, async (url, signal) => {
      // Defence in depth: even internal callers cannot redirect this credential.
      if (!url.startsWith(base + '/')) throw new Error('Unapproved credential destination');
      let route = url.slice(base.length);
      if (new URL(base).pathname === '/' && route.startsWith('/app/')) route = route.slice(4);
      readPath(route);
      const token = candidate === undefined ? await this.secrets.get(key) : candidate;
      if (signal?.aborted) throw new Error('Viewer request aborted');
      return this.transport(url, signal, token === undefined ? {} : { token: accessKey(token) });
    });
  }
  async signIn(base, token, signal) {
    const key = this.key(base);
    await this.client(base, accessKey(token)).connect(signal);
    if (signal?.aborted) throw new Error('Viewer request aborted');
    await this.write(async () => {
      if (signal?.aborted) throw new Error('Viewer request aborted');
      await this.secrets.store(key, token);
    });
  }
  async signOut(base) { const key = this.key(base); await this.write(() => this.secrets.delete(key)); }
}
module.exports = { MeshClient, RemoteAuth, viewerURL, isRemote, accessKey, readPath, getJSON };
