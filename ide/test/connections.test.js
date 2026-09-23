// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
'use strict';
const { test, expect, spyOn } = require('bun:test');
const { EventEmitter } = require('node:events');
const https = require('node:https');
const { BrowserConnections, postJSON } = require('../src/connections');
const { RemoteAuth } = require('../src/client');
const base = 'https://mesh.example/app';
const secret = prefix => prefix + 'a'.repeat(43);
const grant = (suffix = 'a') => ({ access_token: 'mesh_access_' + suffix.repeat(43), refresh_token: 'mesh_refresh_' + suffix.repeat(43), grant_id: secret('mesh_grant_'), token_type: 'Bearer', scope: 'full', expires_in: 900 });
const device = () => ({ device_code: secret('mesh_device_'), user_code: 'ABCD-EFGH', expires_in: 600, interval: 5, verification_uri: base + '/connect', verification_uri_complete: base + '/connect?user_code=ABCD-EFGH' });
function fixture(replies = []) {
  const saved = new Map(), calls = [], waits = [];
  let now = 100000;
  const secrets = { get: async key => saved.get(key), store: async (key, value) => saved.set(key, value), delete: async key => saved.delete(key) };
  const options = { now: () => now, wait: async ms => { waits.push(ms); now += ms; }, post: async (url, body, signal) => { calls.push({ url, body }); if (signal?.aborted) throw new Error('cancelled'); const reply = replies.shift(); if (reply instanceof Error) throw reply; return reply || { status: 200, data: {} }; } };
  const auth = new RemoteAuth(secrets, async () => ({ status: 200, body: '{"counts":{}}' }), options);
  return { auth, connections: auth.connections, saved, calls, waits, options, advance: ms => { now += ms; } };
}
test('browser connection shows only public code, polls with backoff and stores verified credentials securely', async () => {
  const h = fixture([{status:200,data:device()},{status:400,data:{error:'authorization_pending'}},{status:400,data:{error:'slow_down'}},{status:200,data:grant()}]);
  const shown = [];
  await h.auth.signInBrowser(base, undefined, async request => shown.push(request));
  expect(shown).toEqual([{ code: 'ABCD-EFGH', url: base + '/connect?user_code=ABCD-EFGH', server: base }]);
  expect(JSON.stringify(shown)).not.toContain('mesh_device_');
  expect(h.waits).toEqual([5000,5000,10000]);
  expect(h.saved.has('mesh.viewer-key:' + base)).toBe(false);
  expect(JSON.parse(h.saved.get(h.connections.key(base))).grantID).toBe(grant().grant_id);
  await h.auth.client(base).connect();
  expect(h.calls).toHaveLength(4);
  await h.auth.signOut(base);
  expect(h.calls.at(-1).url).toBe(base + '/api/connect/revoke');
  expect(h.saved.size).toBe(0);
});
test('malicious approval URLs never reach the browser', async () => {
  for (const extra of [{verification_uri_complete:'https://evil.example/steal'}, {verification_uri:base+'/connect?secret=bad'}, {user_code:'<script>'}, {interval:0}, {expires_in:86400}]) {
    const h = fixture([{status:200,data:{...device(),...extra}}]);
    let shown = false;
    await expect(h.auth.signInBrowser(base, undefined, async () => {shown=true;})).rejects.toThrow();
    expect(shown).toBe(false); expect(h.saved.size).toBe(0);
  }
});
test('denial, terminal failure and cancellation stop polling and abandon the pending request', async () => {
  for (const error of ['access_denied','expired_token','invalid_grant']) {
    const h = fixture([{status:200,data:device()},{status:400,data:{error}}]);
    await expect(h.auth.signInBrowser(base,undefined,async()=>{})).rejects.toThrow();
    expect(h.calls.map(x=>x.url.split('/').at(-1))).toEqual(['device','token','cancel']);
    expect(h.saved.size).toBe(0);
  }
  const h = fixture([{status:200,data:device()}]), controller = new AbortController();
  await expect(h.auth.signInBrowser(base,controller.signal,async()=>controller.abort())).rejects.toThrow();
  expect(h.calls.filter(x=>x.url.endsWith('/token'))).toHaveLength(0);
  expect(h.calls.at(-1).url).toEndWith('/cancel');
});
test('expiry never silently starts another authorization request', async () => {
  const h=fixture([{status:200,data:{...device(),expires_in:5}}]);
  await expect(h.auth.signInBrowser(base,undefined,async()=>{})).rejects.toThrow('expired');
  expect(h.calls.map(x=>x.url.split('/').at(-1))).toEqual(['device','cancel']);
});
test('legacy servers require explicit fallback instead of a hidden access-key prompt', async () => {
  const h=fixture([{status:404,data:{}}]);
  await expect(h.auth.signInBrowser(base,undefined,async()=>{})).rejects.toThrow('Sign In with Access Key');
  expect(h.saved.size).toBe(0);
});
test('parallel reads serialize renewal and sign-out revokes the rotated credential', async () => {
  const h=fixture([{status:200,data:device()},{status:200,data:grant()},{status:200,data:grant('b')}]);
  await h.auth.signInBrowser(base,undefined,async()=>{});
  h.advance(900000);
  await Promise.all(Array.from({length:10},()=>h.auth.client(base).connect()));
  expect(h.calls.filter(x=>x.body.grant_type==='refresh_token')).toHaveLength(1);
  const stored=JSON.parse(h.saved.get(h.connections.key(base)));
  expect(stored.accessToken).toBe(grant('b').access_token);
  expect(stored.refreshing).toBeUndefined();
  await h.auth.signOut(base);
  expect(h.calls.at(-1).body.token).toBe(grant('b').refresh_token);
});
test('uncertain renewal is never retried after a crash or next request', async () => {
  const h=fixture([{status:200,data:device()},{status:200,data:grant()},new Error('network lost')]);
  await h.auth.signInBrowser(base,undefined,async()=>{}); h.advance(900000);
  await expect(h.auth.client(base).connect()).rejects.toThrow();
  await expect(h.auth.client(base).connect()).rejects.toThrow('interrupted');
  const restarted = new BrowserConnections(h.connections.secrets,h.options);
  await expect(restarted.access(base)).rejects.toThrow('interrupted');
  expect(h.calls.filter(x=>x.body.grant_type==='refresh_token')).toHaveLength(1);
});
test('failed revocation preserves credentials for retry and does not claim sign-out', async () => {
  const h=fixture([{status:200,data:device()},{status:200,data:grant()},{status:503,data:{}}]);
  await h.auth.signInBrowser(base,undefined,async()=>{});
  await expect(h.auth.signOut(base)).rejects.toThrow('did not confirm');
  expect(h.saved.size).toBe(1);
  await h.auth.signOut(base); expect(h.saved.size).toBe(0);
});
test('stored credentials cannot redirect refresh to another host or base path', async () => {
  const h=fixture();
  h.saved.set(h.connections.key(base),JSON.stringify({version:1,base:'https://evil.example/app',accessToken:grant().access_token,refreshToken:grant().refresh_token,grantID:grant().grant_id,scope:'full',expiresAt:0}));
  await expect(h.auth.client(base).connect()).rejects.toThrow('Stored Mesh connection');
  expect(h.calls).toHaveLength(0);
});
test('POST transport sends no cookies, verifies TLS and never follows a credential redirect', async () => {
  const calls=[];
  const spy=spyOn(https,'request').mockImplementation((url,options,callback)=>{
    const req=new EventEmitter(); req.destroy=()=>{};req.end=body=>{calls.push({url:url.toString(),options,body:body.toString()});queueMicrotask(()=>callback({statusCode:302,headers:{location:'https://evil.example/'},destroy(){}}));};return req;
  });
  try {
    await expect(postJSON(base+'/api/connect/token',{refresh_token:'fixture'})).rejects.toThrow('redirected');
    expect(calls).toHaveLength(1);
    expect(calls[0].options.rejectUnauthorized).toBeUndefined();
    expect(calls[0].options.headers.Cookie).toBeUndefined();
    expect(calls[0].options.headers.Authorization).toBeUndefined();
  } finally {spy.mockRestore();}
});
