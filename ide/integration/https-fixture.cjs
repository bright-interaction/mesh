// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

'use strict';
const assert = require('node:assert/strict');
const https = require('node:https');
const fs = require('node:fs');
const path = require('node:path');
const { RemoteAuth, getJSON } = require('../src/client');

async function run() {
  const [dir, mode] = process.argv.slice(2);
  const seen = [], stored = new Map(), sockets = new Set();
  let slowStarted, slowClosed;
  const started = new Promise(resolve => { slowStarted = resolve; });
  const closed = new Promise(resolve => { slowClosed = resolve; });
  const server = https.createServer({ key: fs.readFileSync(path.join(dir, 'key.pem')), cert: fs.readFileSync(path.join(dir, 'cert.pem')) }, (req, res) => {
    seen.push({ path: req.url, method: req.method, auth: req.headers.authorization, cookie: req.headers.cookie });
    if (req.headers.authorization !== 'Bearer fixture-member-key') { res.writeHead(401); res.end(); return; }
    if (req.url === '/app/api/docs') { res.writeHead(302, { Location: '/credential-trap' }); res.end(); return; }
    if (req.url === '/app/api/note/private') { res.writeHead(403); res.end(); return; }
    if (req.url === '/app/api/note/slow') { res.on('close', slowClosed); req.socket.on('close', slowClosed); slowStarted(); return; }
    res.writeHead(200, { 'Content-Type': 'application/json' });
    if (req.url === '/app/api/status') res.end('{"counts":{"notes":1}}');
    else if (req.url === '/app/graph.json') res.end('{"nodes":[{"id":"allowed-note"}]}');
    else if (req.url.startsWith('/app/api/search?')) res.end('{"cards":[{"id":"allowed-note"}]}');
    else if (req.url === '/app/api/note/allowed-note') res.end('{"id":"allowed-note","body":"Fixture only"}');
    else res.end('{}');
  });
  server.on('connection', socket => { sockets.add(socket); socket.on('close', () => sockets.delete(socket)); });
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
  const base = `https://127.0.0.1:${server.address().port}/app`;
  const auth = new RemoteAuth({ get: async key => stored.get(key), store: async (key, value) => stored.set(key, value), delete: async key => stored.delete(key) });
  try {
    if (mode === 'untrusted') {
      await assert.rejects(auth.signIn(base, 'fixture-member-key'), error => /CERT|SELF_SIGNED/.test(error.code || '') || /certificate|self.signed/i.test(error.message));
      assert.equal(seen.length, 0, 'untrusted TLS must fail before sending credential headers');
      assert.equal(stored.size, 0);
    } else {
      await assert.rejects(auth.client(base).connect(), error => error.code === 'AUTH_REQUIRED');
      await assert.rejects(auth.signIn(base, 'wrong-fixture-key'), error => error.code === 'AUTH_REQUIRED');
      assert.equal(stored.size, 0);
      await auth.signIn(base, 'fixture-member-key');
      assert.equal(stored.size, 1);
      const client = auth.client(base);
      assert.equal(JSON.parse((await client.request('/graph.json')).body).nodes[0].id, 'allowed-note');
      assert.equal(JSON.parse((await client.request('/api/search?q=fixture&limit=1&budget=500')).body).cards[0].id, 'allowed-note');
      assert.equal(JSON.parse((await client.request('/api/note/allowed-note')).body).body, 'Fixture only');
      assert.equal((await client.request('/api/note/private')).status, 403);
      assert.equal((await client.request('/api/docs')).status, 302);
      assert.equal(seen.some(row => row.path === '/credential-trap'), false);
      const before = seen.length;
      await assert.rejects(client.request('/api/reindex'));
      assert.equal(seen.length, before, 'mutations must fail before transport');
      const controller = new AbortController();
      const pending = client.request('/api/note/slow', controller.signal);
      await started; controller.abort();
      await assert.rejects(pending);
      let deadline;
      try { await Promise.race([closed, new Promise((_, reject) => { deadline = setTimeout(() => reject(new Error('cancelled HTTPS socket stayed open')), 1500); })]); }
      finally { clearTimeout(deadline); }
      await auth.signOut(base);
      await assert.rejects(auth.client(base).connect(), error => error.code === 'AUTH_REQUIRED');
      assert.equal(stored.size, 0);
      assert.equal(seen.at(-1).auth, undefined);
      assert(seen.every(row => row.method === 'GET' && row.cookie === undefined));
      await assert.rejects(getJSON(base + '/api/status', undefined, { token: 'invalid\r\nheader' }));
    }
    console.log('PASS ' + mode + ' real HTTPS fixture');
  } finally {
    for (const socket of sockets) socket.destroy();
    await new Promise(resolve => server.close(resolve));
  }
}
run().catch(error => { console.error(error); process.exitCode = 1; });
