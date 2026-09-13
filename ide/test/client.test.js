'use strict';
const { test, expect } = require('bun:test');
const http = require('node:http');
const { MeshClient, viewerURL, readPath, getJSON } = require('../src/client');

const status = { status: 200, body: JSON.stringify({ counts: { notes: 2 } }) };
async function withServer(handler, check) {
  const server = http.createServer(handler);
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
  try { await check(`http://127.0.0.1:${server.address().port}`); }
  finally { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); }
}

test('viewer URLs allow only HTTP numeric loopback and simple base paths', () => {
  expect(viewerURL('http://127.0.0.1:7474/')).toBe('http://127.0.0.1:7474');
  expect(viewerURL('http://127.0.0.1:7474/app/')).toBe('http://127.0.0.1:7474/app');
  expect(viewerURL('http://[::1]:7474/app')).toBe('http://[::1]:7474/app');
  for (const value of [null, '', 'http://localhost:7474', 'https://127.0.0.1', 'http://192.168.1.2', 'http://127.0.0.1.evil.invalid', 'http://user:secret@127.0.0.1', 'http://127.0.0.1/?x=1', 'http://127.0.0.1/#key', 'http://127.0.0.1/app%2fother', 'file:///tmp/view', 'http://127.0.0.1/' + 'a'.repeat(513)]) {
    expect(() => viewerURL(value)).toThrow();
  }
});

test('read paths retain exact allowlisted endpoints and bounded search options', () => {
  for (const path of ['/graph.json', '/api/status', '/api/dashboard', '/api/docs', '/api/docs/01-getting-started', '/api/note/my-note', '/api/note/caf%C3%A9']) expect(readPath(path)).toBe(path);
  expect(readPath('graph.json')).toBe('/graph.json');
  expect(readPath('/api/search?q=mesh%20ide&limit=30&budget=8000')).toBe('/api/search?q=mesh%20ide&limit=30&budget=8000');
  for (const path of [undefined, '', '//evil.invalid/api/status', 'http://evil.invalid/api/status', '/a/../api/status', '/api/./status', '/api/%2e%2e/status', '/api/status#x', '/api/status?x=1', '/api/status/extra', '/api/config', '/api/ask', '/api/pending', '/api/reindex', '/api/note/', '/api/note/a/b', '/api/note/%2fetc', '/api/note/%5cetc', '/api/note/%00', '/api/note/%20', '/api/note/%25', '/api/note/%3f', '/api/note/%23', '/api/note/%zz', '/api/note/' + 'x'.repeat(513), '/api/docs/x?y=1', '/api/search', '/api/search?q=', '/api/search?q=%20', '/api/search?q=one&q=two', '/api/search?q=a&limit=1&limit=2', '/api/search?q=a&unknown=1', '/api/search?q=a&limit=0', '/api/search?q=a&limit=-1', '/api/search?q=a&limit=01', '/api/search?q=a&limit=31', '/api/search?q=a&limit=1e1', '/api/search?q=a&budget=8001', '/api/search?q=a&budget=0', '/api/search?q=' + 'x'.repeat(2049), '/api/search?q=' + '%C3%A9'.repeat(1025), '/api/sta\\tus', '/api/status\n']) {
    expect(() => readPath(path)).toThrow();
  }
});

test('root viewer probe is reused and a validated status is returned directly', async () => {
  const calls = [];
  const transport = async (url, signal) => { calls.push({ url, signal }); return url.endsWith('/api/status') ? status : { status: 200, body: '{}' }; };
  const client = new MeshClient('http://127.0.0.1:7474', transport);
  const signal = new AbortController().signal;
  expect(await client.request('/api/status', signal)).toEqual(status);
  await client.request('/api/docs', signal);
  expect(calls.map(c => c.url)).toEqual(['http://127.0.0.1:7474/api/status', 'http://127.0.0.1:7474/api/docs']);
  expect(calls.every(c => c.signal === signal)).toBe(true);
});

test('only root 404 probes /app and explicit bases are respected', async () => {
  const calls = [];
  const client = new MeshClient('http://127.0.0.1:7474', async url => {
    calls.push(url);
    return url === 'http://127.0.0.1:7474/api/status' ? { status: 404, body: '{}' } : status;
  });
  await client.request('/graph.json');
  expect(calls).toEqual(['http://127.0.0.1:7474/api/status', 'http://127.0.0.1:7474/app/api/status', 'http://127.0.0.1:7474/app/graph.json']);
  for (const code of [301, 401, 403, 500]) {
    const attempts = [];
    const locked = new MeshClient('http://127.0.0.1:7474', async url => { attempts.push(url); return { status: code, body: '{}' }; });
    await expect(locked.request('/graph.json')).rejects.toThrow();
    expect(attempts).toHaveLength(1);
  }
  const explicit = [];
  const based = new MeshClient('http://127.0.0.1:7474/view', async url => { explicit.push(url); return { status: 404, body: '{}' }; });
  await expect(based.request('/graph.json')).rejects.toThrow();
  expect(explicit).toEqual(['http://127.0.0.1:7474/view/api/status']);
});

test('wrong-server status fails before requested data and invalid paths have zero egress', async () => {
  for (const body of ['{}', '{"counts":null}', '{"counts":3}', 'null', 'not json']) {
    let calls = 0;
    const client = new MeshClient('http://127.0.0.1:7474', async () => { calls++; return { status: 200, body }; });
    await expect(client.request('/graph.json')).rejects.toThrow();
    expect(calls).toBe(1);
    expect(client.resolved).toBe(null);
  }
  let calls = 0;
  const client = new MeshClient('http://127.0.0.1:7474', async () => { calls++; return status; });
  await expect(client.request('/api/reindex')).rejects.toThrow();
  expect(calls).toBe(0);
});

test('HTTP transport sends GET with no credentials and never follows redirects', async () => {
  const seen = [];
  await withServer((req, res) => {
    seen.push({ path: req.url, method: req.method, auth: req.headers.authorization, cookie: req.headers.cookie, accept: req.headers.accept });
    if (req.url === '/redirect') { res.writeHead(302, { Location: '/target' }); res.end(); return; }
    res.writeHead(200, { 'Content-Type': 'application/json; charset=utf-8' }); res.end('{"ok":true}');
  }, async base => {
    expect(await getJSON(base + '/data')).toEqual({ status: 200, body: '{"ok":true}' });
    expect(await getJSON(base + '/redirect')).toEqual({ status: 302, body: '{}' });
  });
  expect(seen.map(r => r.path)).toEqual(['/data', '/redirect']);
  expect(seen.every(r => r.method === 'GET' && !r.auth && !r.cookie && r.accept === 'application/json')).toBe(true);
});

test('HTTP transport rejects non-JSON, invalid JSON and oversized streamed responses', async () => {
  await withServer((req, res) => {
    res.writeHead(200, { 'Content-Type': req.url === '/html' ? 'text/html' : 'application/json' });
    if (req.url === '/html') res.end('<h1>not Mesh</h1>');
    else if (req.url === '/invalid') res.end('not JSON');
    else { res.write('{"data":"'); res.end('x'.repeat(128) + '"}'); }
  }, async base => {
    await expect(getJSON(base + '/html')).rejects.toThrow();
    await expect(getJSON(base + '/invalid')).rejects.toThrow();
    await expect(getJSON(base + '/large', undefined, { maxBytes: 32 })).rejects.toThrow();
  });
});

test('HTTP requests are bounded by a deadline and caller cancellation', async () => {
  await withServer((_req, _res) => {}, async base => {
    await expect(getJSON(base, undefined, { timeout: 30 })).rejects.toThrow('timed out');
    const controller = new AbortController();
    const result = getJSON(base, controller.signal, { timeout: 1000 });
    controller.abort();
    await expect(result).rejects.toThrow();
  });
});

test('already-aborted transport requests never reach the viewer', async () => {
  let requests = 0;
  await withServer((_req, res) => { requests++; res.writeHead(200, { 'Content-Type': 'application/json' }); res.end('{}'); }, async base => {
    const controller = new AbortController(); controller.abort();
    await expect(getJSON(base, controller.signal, { timeout: 100 })).rejects.toThrow();
    await new Promise(resolve => setTimeout(resolve, 20));
    expect(requests).toBe(0);
  });
});

test('non-200 responses cancel their streaming body instead of draining it', async () => {
  let closed = false, chunks = 0, timer;
  try {
    await withServer((req, res) => {
      res.writeHead(503, { 'Content-Type': 'application/json' });
      res.write('{"unbounded":"');
      timer = setInterval(() => { chunks++; res.write('x'.repeat(4096)); }, 10);
      const onClose = () => { closed = true; clearInterval(timer); };
      res.on('close', onClose);
      req.socket.on('close', onClose);
    }, async base => {
      expect(await getJSON(base, undefined, { timeout: 1000, maxBytes: 32 })).toEqual({ status: 503, body: '{}' });
      const deadline = Date.now() + 1000;
      while (!closed && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 10));
      expect(closed).toBe(true);
      expect(chunks).toBeLessThan(10);
    });
  } finally { clearInterval(timer); }
});
