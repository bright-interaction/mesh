// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const { test, expect } = require('bun:test');
const { Broker } = require('../src/broker');
const request = (id, path = '/api/status', extra = {}) => ({ type: 'request', id, method: 'GET', path, ...extra });

test('broker permits only exact read-only requests and rejects mutations before egress', async () => {
  const calls = [], sent = [];
  const broker = new Broker({ request: async (...args) => { calls.push(args); return { status: 200, body: '{}' }; } }, m => sent.push(m));
  const bad = [request(1, '/api/reindex'), request(2, '/api/status', { method: 'POST' }), request(3, '/api/status', { method: 'get' }), request(4, '/api/status', { body: '' }), request(5, '/api/status', { headers: {} }), request(6, '//other.invalid/api/status'), request(7, '/api/search?q=x&limit=999'), request(8, '/api/status', { method: undefined })];
  for (const value of bad) await broker.handle(value);
  expect(calls).toHaveLength(0);
  expect(sent).toHaveLength(bad.length);
  expect(sent.every(m => typeof m.error === 'string' && !m.body)).toBe(true);
  await broker.handle(request(20, 'graph.json'));
  expect(calls).toHaveLength(1);
  expect(calls[0][0]).toBe('/graph.json');
  expect(calls[0][1]).toBeInstanceOf(AbortSignal);
  expect(sent.at(-1)).toEqual({ type: 'response', id: 20, status: 200, body: '{}' });
});

test('malformed messages are ignored and ready is separate from data requests', async () => {
  let calls = 0, ready = 0;
  const sent = [];
  const broker = new Broker({ request: async () => { calls++; } }, m => sent.push(m), { ready: () => ready++ });
  for (const message of [undefined, null, 'request', {}, { type: 'other' }, request(0), request(-1), request('1'), request(1.2), request(Number.MAX_SAFE_INTEGER + 1)]) await broker.handle(message);
  await broker.handle({ type: 'ready' });
  expect(calls).toBe(0); expect(sent).toHaveLength(0); expect(ready).toBe(1);
});

test('concurrent request limit and duplicate ids cannot create additional egress', async () => {
  const jobs = [], sent = [];
  const broker = new Broker({ request: (_path, signal) => new Promise(resolve => jobs.push({ signal, resolve })) }, m => sent.push(m));
  const pending = Array.from({ length: 6 }, (_, i) => broker.handle(request(i + 1)));
  expect(jobs).toHaveLength(6);
  await broker.handle(request(1));
  expect(sent).toHaveLength(0);
  await broker.handle(request(7));
  expect(jobs).toHaveLength(6);
  expect(sent).toHaveLength(1);
  expect(sent[0].id).toBe(7);
  expect(sent[0].error).toContain('Too many');
  for (const job of jobs) job.resolve({ status: 200, body: '{}' });
  await Promise.all(pending);
  expect(broker.pending.size).toBe(0);
});

test('minute request limit is enforced before egress and resets after window expiry', async () => {
  let calls = 0;
  const sent = [];
  const broker = new Broker({ request: async () => { calls++; return { status: 200, body: '{}' }; } }, m => sent.push(m));
  for (let id = 1; id <= 121; id++) await broker.handle(request(id));
  expect(calls).toBe(120);
  expect(sent.at(-1).error).toContain('Too many');
  broker.window = Date.now() - 60001;
  await broker.handle(request(122));
  expect(calls).toBe(121);
  expect(sent.at(-1).status).toBe(200);
});

test('disposal aborts all in-flight work, drops late replies and forbids future egress', async () => {
  const jobs = [], sent = [];
  let ready = 0;
  const broker = new Broker({ request: (_path, signal) => new Promise(resolve => jobs.push({ signal, resolve })) }, m => sent.push(m), { ready: () => ready++ });
  const pending = [broker.handle(request(1)), broker.handle(request(2))];
  broker.dispose();
  expect(jobs.every(job => job.signal.aborted)).toBe(true);
  expect(broker.pending.size).toBe(0);
  await broker.handle(request(3));
  await broker.handle({ type: 'ready' });
  expect(jobs).toHaveLength(2); expect(ready).toBe(0);
  for (const job of jobs) job.resolve({ status: 200, body: '{"private":"late data"}' });
  await Promise.all(pending);
  expect(sent).toHaveLength(0);
  broker.dispose();
});

test('transport failure is redacted and releases the concurrency slot', async () => {
  const sent = [];
  const broker = new Broker({ request: async () => { throw new Error('secret token=do-not-publish'); } }, m => sent.push(m));
  await broker.handle(request(1));
  expect(sent).toHaveLength(1);
  expect(sent[0].error).not.toContain('do-not-publish');
  expect(broker.pending.size).toBe(0);
});
