// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

'use strict';
const { test, expect, spyOn } = require('bun:test');
const https = require('node:https');
const { EventEmitter } = require('node:events');
const { RemoteAuth, getJSON } = require('../src/client');
const base = 'https://mesh.example/app';
const status = { status: 200, body: '{"counts":{"notes":2}}' };
function fixture(transport = async () => status) {
  const saved = new Map(), gets = [], writes = [];
  const secrets = {
    get: async key => { gets.push(key); return saved.get(key); },
    store: async (key, value) => { writes.push(key); saved.set(key, value); },
    delete: async key => { saved.delete(key); },
  };
  return { auth: new RemoteAuth(secrets, transport), saved, gets, writes };
}
test('verified keys persist only in server-and-base-bound secure storage and sign out removes them', async () => {
  const calls = [];
  const h = fixture(async (url, signal, options) => { calls.push({ url, options }); return status; });
  await h.auth.signIn(base, 'fixture-key');
  expect(h.writes).toEqual(['mesh.viewer-key:' + base]);
  expect(calls).toEqual([{ url: base + '/api/status', options: { token: 'fixture-key' } }]);
  await h.auth.client(base).request('/api/docs');
  expect(calls.at(-1)).toEqual({ url: base + '/api/docs', options: { token: 'fixture-key' } });
  await h.auth.client('https://other.example/app').connect();
  expect(calls.at(-1).options).toEqual({});
  await h.auth.client('https://mesh.example/other').connect();
  expect(calls.at(-1).options).toEqual({});
  await h.auth.signOut(base);
  expect(h.saved.size).toBe(0);
  await h.auth.client(base).connect();
  expect(calls.at(-1).options).toEqual({});
});
test('local viewers never read remote secrets and HTTP sign-in is forbidden', async () => {
  const calls = [];
  const h = fixture(async (...args) => { calls.push(args); return status; });
  await h.auth.client('http://127.0.0.1:7474').connect();
  expect(h.gets).toEqual([]);
  expect(calls[0][2]).toBeUndefined();
  await expect(h.auth.signIn('http://127.0.0.1:7474', 'fixture-key')).rejects.toThrow();
  await expect(getJSON('http://127.0.0.1:7474', undefined, { token: 'fixture-key' })).rejects.toThrow('HTTPS');
});
test('failed or cancelled verification does not replace an existing key', async () => {
  for (const reply of [{ status: 401, body: '{}' }, { status: 403, body: '{}' }, { status: 302, body: '{}' }, { status: 200, body: '{}' }]) {
    const h = fixture(async () => reply);
    h.saved.set(h.auth.key(base), 'existing-key');
    await expect(h.auth.signIn(base, 'bad-key')).rejects.toThrow();
    expect(h.writes).toEqual([]);
    expect(h.saved.get(h.auth.key(base))).toBe('existing-key');
  }
  const controller = new AbortController();
  const h = fixture(async () => { controller.abort(); return status; });
  await expect(h.auth.signIn(base, 'fixture-key', controller.signal)).rejects.toThrow();
  expect(h.saved.size).toBe(0);
});
test('invalid keys and non-allowlisted routes cause zero credential egress', async () => {
  let calls = 0;
  const h = fixture(async () => { calls++; return status; });
  for (const token of ['', 'x\r\nCookie: injected', ' ', 'x'.repeat(4097)]) await expect(h.auth.signIn(base, token)).rejects.toThrow();
  const client = h.auth.client(base);
  for (const route of ['/api/reindex', 'https://other.example/api/status', '/api/login', '/api/status?key=x']) await expect(client.request(route)).rejects.toThrow();
  for (const url of ['https://other.example/api/status', base + '-evil/api/status', base + '/api/../status']) await expect(client.transport(url)).rejects.toThrow();
  expect(calls).toBe(0);
  expect(h.gets).toHaveLength(0);
});
test('cancellation during secret lookup stops transport', async () => {
  let resolve, entered, calls = 0;
  const reading = new Promise(done => { entered = done; });
  const auth = new RemoteAuth({ get: key => key.startsWith('mesh.viewer-connection:') ? Promise.resolve(undefined) : new Promise(done => { resolve = done; entered(); }) }, async () => { calls++; return status; });
  const controller = new AbortController();
  const result = auth.client(base).connect(controller.signal);
  await reading;
  controller.abort(); resolve('fixture-key');
  await expect(result).rejects.toThrow();
  expect(calls).toBe(0);
});
test('sign out waits for an in-flight secret write so a late write cannot sign back in', async () => {
  let entered, finish;
  const writing = new Promise(done => { entered = done; });
  const stored = new Map();
  const auth = new RemoteAuth({
    store: async (key, value) => { entered(); await new Promise(done => { finish = done; }); stored.set(key, value); },
    delete: async key => stored.delete(key),
  }, async () => status);
  const login = auth.signIn(base, 'fixture-key');
  await writing;
  const logout = auth.signOut(base);
  finish(); await Promise.all([login, logout]);
  expect(stored.size).toBe(0);
});
test('HTTPS transport uses bearer header, normal TLS verification, no cookies and no redirect following', async () => {
  const calls = [];
  const spy = spyOn(https, 'get').mockImplementation((url, options, callback) => {
    calls.push({ url: url.toString(), options });
    const req = new EventEmitter(); req.destroy = () => {};
    queueMicrotask(() => callback({ statusCode: 302, headers: { location: 'https://other.example/steal' }, destroy() {} }));
    return req;
  });
  try {
    expect(await getJSON(base + '/api/status', undefined, { token: 'fixture-key' })).toEqual({ status: 302, body: '{}' });
    expect(calls).toEqual([{ url: base + '/api/status', options: { headers: { Accept: 'application/json', Authorization: 'Bearer fixture-key' }, agent: false } }]);
  } finally { spy.mockRestore(); }
});
