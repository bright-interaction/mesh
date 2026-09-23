// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
'use strict';
const assert = require('node:assert/strict');
const https = require('node:https');
const { RemoteAuth } = require('../src/client');
const base = process.env.MESH_E2E_BASE;
const origin = new URL(base).origin;
async function browser(path, body, cookie) {
  return new Promise((resolve,reject)=>{
    const data = body === undefined ? undefined : JSON.stringify(body);
    const req = https.request(base + path, { method: data ? 'POST' : 'GET', agent:false, headers:{Origin:origin,...(data?{'Content-Type':'application/json','Content-Length':Buffer.byteLength(data)}:{}),...(cookie?{Cookie:cookie}:{})}},res=>{
      const chunks=[];res.on('data',v=>chunks.push(v));res.on('error',reject);res.on('end',()=>resolve({status:res.statusCode,text:Buffer.concat(chunks).toString(),headers:res.headers}));
    });
    req.on('error',reject);req.setTimeout(10000,()=>req.destroy(new Error('fixture request timed out')));req.end(data);
  });
}
(async()=>{
  const saved = new Map();let clock=Date.now();
  const secrets={get:async key=>saved.get(key),store:async(key,value)=>saved.set(key,value),delete:async key=>saved.delete(key)};
  const auth=new RemoteAuth(secrets,undefined,{now:()=>clock});
  const login=await browser('/api/login',{key:process.env.MESH_E2E_KEY});
  assert.equal(login.status,204);const cookie=login.headers['set-cookie'][0].split(';')[0];
  await auth.signInBrowser(base,undefined,async request=>{
    assert.match(request.code,/^[A-Z2-7]{4}-[A-Z2-7]{4}$/);
    assert.equal(request.server,base);
    const page=await browser('/connect?user_code='+request.code,undefined,cookie);
    assert.equal(page.status,200);assert.match(page.text,/Connect to Mesh/);
    const info=await browser('/api/connect/request?user_code='+request.code,undefined,cookie);
    assert.equal(info.status,200);assert.equal(JSON.parse(info.text).scope,'full');
    const consent=await browser('/api/connect/decision',{user_code:request.code,scope:'full',decision:'approve'},cookie);
    assert.equal(consent.status,200);
  });
  assert.equal(saved.size,1);
  let status=await auth.client(base).connect();assert.equal(status.status,200);assert.equal(JSON.parse(status.body).counts.notes,1);
  const before=await auth.connections.read(base);
  clock+=16*60*1000;
  await Promise.all(Array.from({length:4},()=>auth.client(base).connect()));
  const after=await auth.connections.read(base);
  assert.notEqual(after.accessToken,before.accessToken);assert.notEqual(after.refreshToken,before.refreshToken);assert.equal(after.grantID,before.grantID);
  const listing=await browser('/api/connect/connections',undefined,cookie);
  assert.equal(listing.status,200);assert.equal(JSON.parse(listing.text).length,1);
  assert.deepEqual(await auth.signOut(base),{revoked:true});assert.equal(saved.size,0);
  await assert.rejects(()=>auth.client(base,after.accessToken).connect());
  const remaining=await browser('/api/connect/connections',undefined,cookie);assert.deepEqual(JSON.parse(remaining.text),[]);
  console.log('PASS Mesh Go/IDE HTTPS journey: browser approval, stored credential, real read, serialized renewal, server revocation and rejected former token. Disposable fixture only.');
})().catch(error=>{console.error('FAIL Mesh Go/IDE HTTPS journey:',error.message);process.exitCode=1;});
