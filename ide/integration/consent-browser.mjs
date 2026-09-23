// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
// Rendered consent regression with synthetic API responses. No live accounts.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = new URL('../../internal/web/assets/', import.meta.url);
let sso = false;
const server = Bun.serve({ hostname: '127.0.0.1', port: 0, async fetch(request) {
  const path = new URL(request.url).pathname;
  const name = ({'/app/connect':'connect.html','/app/assets/connect.js':'connect.js','/app/assets/connect.css':'connect.css'})[path];
  if (!name) return new Response('Not found', {status:404});
  let body = await readFile(new URL(name,assets),'utf8');
  if (name.endsWith('.html')) {
    const target = new URL('/auth/oidc/login',request.url);
    target.searchParams.set('return_to',new URL(request.url).pathname+new URL(request.url).search);
    body = body.replace('{{.Base}}','/app/').replace('{{.SignInURL}}',sso ? target.href.replaceAll('&','&amp;') : '');
  }
  return new Response(body,{headers:{'Content-Type':name.endsWith('.html')?'text/html':name.endsWith('.js')?'text/javascript':'text/css','Cache-Control':'no-store','Content-Security-Policy':"default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'"}});
}});
let browser;
try {
  browser = await chromium.launch({headless:true,...(process.env.MESH_BROWSER_EXECUTABLE ? {executablePath:process.env.MESH_BROWSER_EXECUTABLE}: {})});
  const page = await browser.newPage({viewport:{width:1000,height:1100}});
  const errors=[],calls=[];
  page.on('pageerror',err=>errors.push(err.message));
  let signedIn=true, handled=false, status=200, scope='full', connection=true;
  await page.route('**/app/api/**',async route=>{
    const req=route.request(),path=new URL(req.url()).pathname,body=req.postDataJSON();
    calls.push({path,method:req.method(),body});
    let response={},code=200;
    if (path.endsWith('/api/login')) {signedIn=true; await route.fulfill({status:204});return;}
    if (!signedIn) {code=401;response={error:'browser_sign_in_required'};}
    else if (path.endsWith('/request')) {code=handled?400:status;response=code===200?{client_id:'mesh-ide',scope,audience:'https://mesh.example/app',account:'Mesh member 7 (member)',role:'member'}:{error:handled?'invalid_grant':'server_error'};}
    else if (path.endsWith('/decision')) {handled=true;response={decision:body.decision};}
    else if (path.endsWith('/connections')) response=connection?[{id:'public-grant-id',client_id:'mesh-ide',scope:'full',expires_at:Math.floor(Date.now()/1000)+3600}]:[];
    else if (path.endsWith('/disconnect')) connection=false;
    else throw new Error('Unexpected fixture request: '+path);
    await route.fulfill({status:code,json:response});
  });
  const open=async code=>{await page.goto(`http://127.0.0.1:${server.port}/app/connect${code?'?user_code='+code:''}`);};
  const visible=async id=>page.locator(id).waitFor({state:'visible'});
  await open('ABCD-EFGH');await visible('#request');
  assert.equal(await page.locator('#code').textContent(),'ABCD-EFGH');
  assert.equal(await page.locator('input[value=full]').isChecked(),true);
  assert.equal(calls.filter(c=>c.method==='POST').length,0);
  await page.locator('#approve').click();
  assert.equal(calls.filter(c=>c.method==='POST').length,0,'unchecked code confirmation submitted');
  await page.setViewportSize({width:320,height:750});
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true,'mobile layout overflow');
  if (process.env.MESH_CONSENT_SCREENSHOT) await page.screenshot({path:process.env.MESH_CONSENT_SCREENSHOT,fullPage:true});
  await page.locator('input[value=read]').check();await page.locator('#confirm-code').check();await page.locator('#approve').click();await visible('#done');
  assert.deepEqual(calls.find(c=>c.path.endsWith('/decision')).body,{user_code:'ABCD-EFGH',scope:'read',decision:'approve'});
  await page.reload();await page.getByText(/expired or was already handled/).waitFor();
  assert.equal(await page.locator('#request').isVisible(),false);

  handled=false;signedIn=false;calls.length=0;
  await open('ABCD-EFGH');await visible('#signin');
  await page.locator('#key').fill('fixture-browser-key');await page.locator('#login button').click();await visible('#request');
  assert.equal(await page.locator('#key').inputValue(),'');
  assert.equal(calls.filter(c=>c.path.endsWith('/decision')).length,0,'sign-in auto-approved request');
  assert.match(page.url(),/user_code=ABCD-EFGH/);
  assert.equal(await page.evaluate(()=>localStorage.length+sessionStorage.length),0);
  await page.locator('#deny').click();await visible('#done');
  assert.equal(await page.locator('#result-title').textContent(),'Connection denied');

  sso=true;signedIn=false;handled=false;calls.length=0;
  await page.route('**/auth/oidc/login?**',async route=>{
    signedIn=true;
    const target=new URL(route.request().url()).searchParams.get('return_to');
    assert.equal(target,'/app/connect?user_code=ABCD-EFGH');
    await route.fulfill({status:302,headers:{location:target}});
  });
  await open('ABCD-EFGH');await visible('#signin-link');
  assert.equal(await page.locator('#key').isVisible(),false,'access key should not be the default SSO experience');
  await page.locator('#signin-link').click();await visible('#request');
  assert.equal(await page.locator('#code').textContent(),'ABCD-EFGH');
  assert.equal(await page.locator('#confirm-code').isChecked(),false);
  assert.equal(calls.filter(c=>c.method==='POST').length,0,'SSO automatically approved or posted credentials');

  handled=false;scope='read';await open('ABCD-EFGH');await visible('#request');
  assert.equal(await page.locator('#full-option').isVisible(),false);
  await open();await visible('#connections');await page.getByRole('button',{name:'Disconnect',exact:true}).click();await page.getByText('You have no active connections.').waitFor();
  status=503;await open('ABCD-EFGH');await page.getByText(/could not complete/).waitFor();
  assert.equal(await page.locator('#request').isVisible(),false);
  assert.deepEqual(errors,[]);
  console.log('PASS rendered Mesh consent: code confirmation, scope narrowing, retained request through sign-in, denial, errors, revocation and mobile layout. Synthetic API; not a live release receipt.');
} finally {if (browser) await browser.close();server.stop(true);}
