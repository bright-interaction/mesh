// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const { test, expect } = require('bun:test');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { EventEmitter } = require('node:events');
const { ViewerLifecycle, readiness, launchSpec } = require('../src/lifecycle');

const vault = '/test/Corpus';
const metadata = { name: 'mesh', apiVersion: 1, mode: 'read-only', freshness: 'current-to-observed-index', indexOwner: 'observed' };
const response = (data = {}) => ({ status: 200, body: JSON.stringify({ counts: { notes: 2 }, vault, viewer: metadata, ...data }) });
const error = code => Object.assign(new Error('deliberately private transport detail'), { code });
function deferred() { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }
function childProcess() { const child = new EventEmitter(); child.kills = []; child.kill = signal => { child.kills.push(signal); return true; }; return child; }
function harness(connect = async () => response(), options = {}, start = () => childProcess()) {
  let time = 1000000, id = 0;
  const timers = new Map(), states = [], signals = [], children = [];
  const lifecycle = new ViewerLifecycle({ connect: signal => { signals.push(signal); return connect(signal); } }, { url: 'http://127.0.0.1:7476', vault, autoStart: true, ...options }, state => states.push(state), {
    now: () => time,
    setTimer: (fn, delay) => { const handle = ++id; timers.set(handle, { fn, delay }); return handle; },
    clearTimer: handle => timers.delete(handle),
    start: opts => { const child = start(opts); children.push(child); return child; },
  });
  lifecycle.setVisible(true);
  return { lifecycle, timers, states, signals, children, advance: ms => { time += ms; } };
}

test('startup uses an absolute executable and vault with literal arguments and scrubbed viewer environment', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'mesh-lifecycle-'));
  const target = path.join(root, 'vault with spaces;$(not-a-command)');
  fs.mkdirSync(target);
  try {
    const inherited = { PATH: '/allowed/bin', KEEP: 'yes', MESH_UI_TOKEN: 'test-value', MESH_UI_HUB_DB: '/unexpected', MESH_UI_BASE_PATH: '/app', MESH_UI_OWN_INDEX: '1', MESH_UI_FUTURE_SETTING: 'unsafe', MESH_WEB_DEV: '1' };
    for (const host of ['127.0.0.1:7476', '[::1]:7476']) {
      const spec = launchSpec({ url: `http://${host}/`, binary: process.execPath, vault: target }, inherited);
      expect(spec.binary).toBe(process.execPath);
      expect(spec.args).toEqual(['ui', target, '--addr', host, '--own-index=false']);
      expect(spec.options).toEqual({ cwd: target, env: { PATH: '/allowed/bin', KEEP: 'yes', MESH_UI_OWN_INDEX: '0' }, shell: false, windowsHide: true, stdio: 'ignore' });
    }
    expect(inherited.MESH_UI_OWN_INDEX).toBe('1');
    expect(inherited.MESH_UI_TOKEN).toBe('test-value');
    const valid = { url: 'http://127.0.0.1:7476', binary: process.execPath, vault: target };
    for (const options of [
      { binary: 'mesh' }, { binary: target }, { binary: path.join(root, 'missing') },
      { vault: 'relative/Corpus' }, { vault: process.execPath }, { vault: path.join(root, 'missing') },
      ...['http://127.0.0.1:7476/app', 'http://127.0.0.1:0', 'http://127.0.0.1', 'http://localhost:7476', 'http://0.0.0.0:7476', 'http://192.168.1.2:7476', 'https://127.0.0.1:7476', 'http://user:secret@127.0.0.1:7476', 'http://127.0.0.1:7476/?x=1'].map(url => ({ url })),
    ]) expect(() => launchSpec({ ...valid, ...options }, {})).toThrow();
  } finally { fs.rmdirSync(target); fs.rmdirSync(root); }
});

test('HTTPS refusal never starts a process, even with stale local startup opt-in', async () => {
  const h = harness(async () => { throw error('ECONNREFUSED'); }, { url: 'https://mesh.example:7476', autoStart: true });
  await h.lifecycle.check();
  expect(h.children).toHaveLength(0);
  expect(h.lifecycle.starts).toHaveLength(0);
  h.lifecycle.dispose();
});

test('auth failures show actionable guidance and stop polling until explicit retry', async () => {
  for (const code of ['AUTH_REQUIRED', 'ACCESS_DENIED']) {
    const h = harness(async () => { throw error(code); }, { url: 'https://mesh.example/app' });
    await h.lifecycle.check();
    expect(h.children).toHaveLength(0);
    expect(h.timers.size).toBe(0);
    expect(h.lifecycle.state.detail).toContain(code === 'AUTH_REQUIRED' ? 'Mesh: Connect' : 'permissions');
    expect(h.lifecycle.state.detail).not.toContain('private');
    h.lifecycle.retry();
    expect(h.timers.size).toBe(1);
    h.lifecycle.dispose();
  }
});

test('readiness checks pinned vault identity and the supported viewer metadata contract', () => {
  expect(readiness({ vault: '/test/other', viewer: metadata }, '')).toEqual({ kind: 'connected', detail: expect.stringContaining('vault not pinned') });
  expect(readiness({ vault: '/test/Corpus/', viewer: metadata }, vault).kind).toBe('connected');
  for (const wrong of [undefined, null, 42, 'test/Corpus', '/test/Other', '/test/Corpus-other']) {
    expect(() => readiness({ vault: wrong, viewer: metadata }, vault)).toThrow('WRONG_VAULT');
  }
  for (const changed of [{ name: 'other' }, { apiVersion: 2 }, { apiVersion: '1' }, { mode: 'writer' }, { freshness: 'fresh' }, { indexOwner: true }]) {
    expect(() => readiness({ vault, viewer: { ...metadata, ...changed } }, vault)).toThrow('INCOMPATIBLE');
  }
  expect(readiness({ vault }, vault)).toEqual({ kind: 'legacy', detail: expect.stringContaining('unknown') });
  const unknown = readiness({ vault, viewer: { ...metadata, mode: 'owner', freshness: 'unknown', indexOwner: 'not-observed' } }, vault);
  expect(unknown.kind).toBe('connected');
  expect(unknown.detail).toContain('index unknown; owner not-observed');
});

test('overlapping lifecycle checks share one in-flight connection and one scheduled poll', async () => {
  const pending = deferred();
  const h = harness(() => pending.promise);
  const first = h.lifecycle.check();
  await h.lifecycle.check(); await h.lifecycle.check();
  expect(h.signals).toHaveLength(1);
  expect(h.timers.size).toBe(1);
  pending.resolve(response()); await first;
  expect(h.lifecycle.state.kind).toBe('connected');
  expect(h.timers.size).toBe(1);
  expect([...h.timers.values()][0].delay).toBe(30000);
  h.lifecycle.dispose();
});

test('hiding aborts pending work and ignores stale success without spawning or polling', async () => {
  const pending = deferred();
  const h = harness(() => pending.promise);
  const work = h.lifecycle.check();
  h.lifecycle.setVisible(false);
  expect(h.signals[0].aborted).toBe(true);
  pending.resolve(response()); await work;
  expect(h.states.map(s => s.kind)).toEqual(['connecting']);
  expect(h.timers.size).toBe(0);
  await h.lifecycle.check();
  expect(h.signals).toHaveLength(1);
  expect(h.children).toHaveLength(0);
  h.lifecycle.dispose();
});

test('retry invalidates the old generation and stale refusal cannot start a viewer', async () => {
  const pending = deferred(); let calls = 0;
  const h = harness(() => ++calls === 1 ? pending.promise : Promise.resolve(response()));
  const work = h.lifecycle.check();
  h.lifecycle.retry();
  expect(h.signals[0].aborted).toBe(true);
  pending.reject(error('ECONNREFUSED')); await work;
  expect(h.children).toHaveLength(0);
  expect([...h.timers.values()][0].delay).toBe(0);
  await h.lifecycle.check();
  expect(h.lifecycle.state.kind).toBe('connected');
  h.lifecycle.dispose();
});

test('external modern and legacy viewers are never adopted or terminated', async () => {
  for (const data of [{}, { viewer: undefined }]) {
    const h = harness(async () => response(data));
    await h.lifecycle.check();
    expect(h.lifecycle.state.kind).toBe(data.viewer === undefined && 'viewer' in data ? 'legacy' : 'connected');
    h.lifecycle.setVisible(false); h.lifecycle.setVisible(true);
    await h.lifecycle.check();
    h.lifecycle.dispose(); h.lifecycle.dispose();
    expect(h.children).toHaveLength(0);
    expect(h.timers.size).toBe(0);
  }
});

test('only connection refusal may auto-start and operational errors are redacted', async () => {
  for (const code of ['ECONNRESET', 'ETIMEDOUT', 'EACCES', 'WRONG_VAULT', 'INCOMPATIBLE', undefined]) {
    const h = harness(async () => { throw error(code); });
    await h.lifecycle.check();
    expect(h.children).toHaveLength(0);
    expect(h.lifecycle.state.kind).toBe('offline');
    expect(h.lifecycle.state.detail).not.toContain('deliberately private');
    h.lifecycle.dispose();
  }
  for (const data of [{ vault: '/wrong' }, { viewer: { ...metadata, apiVersion: 99 } }]) {
    const h = harness(async () => response(data)); await h.lifecycle.check();
    expect(h.lifecycle.state.kind).toBe('offline'); expect(h.children).toHaveLength(0); h.lifecycle.dispose();
  }
  const disabled = harness(async () => { throw error('ECONNREFUSED'); }, { autoStart: false });
  await disabled.lifecycle.check(); expect(disabled.children).toHaveLength(0); disabled.lifecycle.dispose();
});

test('owned process survives hidden tabs, recovers readiness, and is terminated once on disposal', async () => {
  let online = false;
  const h = harness(async () => { if (!online) throw error('ECONNREFUSED'); return response(); });
  await h.lifecycle.check();
  expect(h.children).toHaveLength(1); expect(h.lifecycle.state.kind).toBe('starting');
  expect(h.lifecycle.state.detail).toContain('existing index owner is unchanged');
  online = true; await h.lifecycle.check();
  expect(h.lifecycle.state.kind).toBe('connected'); expect(h.lifecycle.failures).toBe(0);
  expect(h.lifecycle.childDeadline).toBeUndefined();
  h.lifecycle.setVisible(false); expect(h.children[0].kills).toEqual([]);
  h.lifecycle.setVisible(true); await h.lifecycle.check();
  h.lifecycle.dispose(); h.lifecycle.dispose();
  expect(h.children[0].kills).toEqual(['SIGTERM']); expect(h.timers.size).toBe(0);
  h.children[0].emit('exit', 0); expect(h.timers.size).toBe(0);
});

test('owned process exit clears ready status immediately before reconnect', async () => {
  let online = false;
  const h = harness(async () => { if (!online) throw error('ECONNREFUSED'); return response(); });
  await h.lifecycle.check(); online = true; await h.lifecycle.check();
  expect(h.lifecycle.state.kind).toBe('connected');
  h.children[0].emit('exit', 0);
  expect(h.lifecycle.state.kind).toBe('offline');
  expect(h.lifecycle.child).toBeUndefined();
  expect(h.lifecycle.childDeadline).toBeUndefined();
  expect([...h.timers.values()][0].delay).toBe(1000);
  h.lifecycle.dispose();
});

test('auto-started legacy and owner-mode viewers are stopped instead of claiming read-only readiness', async () => {
  for (const viewer of [undefined, { ...metadata, mode: 'owner' }]) {
    let online = false;
    const h = harness(async () => { if (!online) throw error('ECONNREFUSED'); return response({ viewer }); });
    await h.lifecycle.check(); online = true; await h.lifecycle.check();
    expect(h.lifecycle.state.kind).toBe('offline');
    expect(h.lifecycle.state.detail).toContain('did not confirm read-only mode');
    expect(h.lifecycle.childDeadline).toBeUndefined();
    expect(h.lifecycle.child).toBeUndefined();
    expect(h.children[0].kills).toEqual(['SIGTERM']);
    h.lifecycle.dispose(); expect(h.children[0].kills).toEqual(['SIGTERM']);
  }
});

test('hiding before readiness stops an owned startup and aborts its pending probe', async () => {
  const pending = deferred(); let calls = 0;
  const h = harness(async () => { if (++calls === 1) throw error('ECONNREFUSED'); return pending.promise; });
  await h.lifecycle.check();
  const work = h.lifecycle.check();
  h.lifecycle.setVisible(false);
  expect(h.signals[1].aborted).toBe(true);
  expect(h.children[0].kills).toEqual(['SIGTERM']);
  expect(h.lifecycle.child).toBeUndefined(); expect(h.lifecycle.childDeadline).toBeUndefined();
  pending.resolve(response()); await work;
  expect(h.states.some(state => state.kind === 'connected')).toBe(false);
  expect(h.timers.size).toBe(0);
  h.lifecycle.dispose(); expect(h.children[0].kills).toEqual(['SIGTERM']);
});

test('owned startup is stopped at its deadline without launching a replacement in the same check', async () => {
  const h = harness(async () => { throw error('ECONNREFUSED'); });
  await h.lifecycle.check(); h.advance(59999); await h.lifecycle.check();
  expect(h.children).toHaveLength(1); expect(h.children[0].kills).toEqual([]);
  h.advance(1); await h.lifecycle.check();
  expect(h.children).toHaveLength(1); expect(h.children[0].kills).toEqual(['SIGTERM']);
  expect(h.lifecycle.state.kind).toBe('offline'); expect(h.lifecycle.state.detail).toContain('timed out');
  h.lifecycle.dispose();
});

test('automatic startup is capped at three attempts in a sliding ten-minute window', async () => {
  const h = harness(async () => { throw error('ECONNREFUSED'); });
  for (let i = 0; i < 3; i++) {
    await h.lifecycle.check(); expect(h.children).toHaveLength(i + 1);
    h.children[i].emit(i % 2 ? 'error' : 'exit', error('ENOENT')); h.advance(1000);
  }
  await h.lifecycle.check(); expect(h.children).toHaveLength(3);
  expect(h.lifecycle.state.detail).toContain('three attempts in ten minutes');
  h.lifecycle.retry(); await h.lifecycle.check(); expect(h.children).toHaveLength(3);
  h.advance(597000); await h.lifecycle.check(); expect(h.children).toHaveLength(4);
  expect(h.lifecycle.starts).toHaveLength(3);
  h.lifecycle.dispose();
});

test('synchronous launch failures consume startup allowance and never expose exception details', async () => {
  let attempts = 0;
  const h = harness(async () => { throw error('ECONNREFUSED'); }, {}, () => { attempts++; throw error('ENOENT'); });
  for (let i = 0; i < 4; i++) await h.lifecycle.check();
  expect(attempts).toBe(3); expect(h.lifecycle.state.kind).toBe('offline');
  expect(h.states.some(state => state.detail.includes('approved executable and vault'))).toBe(true);
  expect(h.states.some(state => state.detail.includes('deliberately private'))).toBe(false);
  h.lifecycle.dispose();
});

test('offline polling uses bounded backoff, then reports recovered status and normal polling', async () => {
  let online = false;
  const h = harness(async () => { if (!online) throw error('ETIMEDOUT'); return response(); });
  for (const delay of [1000, 2000, 4000, 8000, 16000, 30000, 30000]) {
    await h.lifecycle.check(); expect([...h.timers.values()][0].delay).toBe(delay);
  }
  online = true; await h.lifecycle.check();
  expect(h.lifecycle.state.kind).toBe('connected');
  expect(h.lifecycle.state.detail).toContain('read-only; index current-to-observed-index; owner observed');
  expect([...h.timers.values()][0].delay).toBe(30000);
  expect(h.lifecycle.failures).toBe(0); expect(h.children).toHaveLength(0);
  h.lifecycle.dispose();
});

test('disposal cancels connection and suppresses late completion and rescheduling', async () => {
  const pending = deferred(); const h = harness(() => pending.promise);
  const work = h.lifecycle.check(); h.lifecycle.dispose();
  expect(h.signals[0].aborted).toBe(true);
  pending.reject(error('ECONNREFUSED')); await work;
  h.lifecycle.retry(); h.lifecycle.setVisible(true); await h.lifecycle.check();
  expect(h.children).toHaveLength(0); expect(h.signals).toHaveLength(1); expect(h.timers.size).toBe(0);
  expect(h.states.map(state => state.kind)).toEqual(['connecting']);
});
