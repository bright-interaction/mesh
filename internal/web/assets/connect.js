// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
(() => {
  const $ = id => document.getElementById(id);
  const params = new URLSearchParams(location.search);
  const code = (params.get('user_code') || '').trim().toUpperCase();
  const signIn = $('signin-link').dataset.signIn;
  if (signIn) {
    const target = new URL(signIn, location.href);
    if (target.origin === location.origin && target.pathname === '/auth/oidc/login') {
      $('signin-link').href = target.href;
      $('account-signin').hidden = false;
      $('key-signin').open = false;
    }
  }
  let requestedScope, busy = false;
  const status = message => { $('status').textContent = message; };
  const api = async (route, body) => {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 15000);
    try {
      const response = await fetch('api/connect/' + route, { method: body ? 'POST' : 'GET', credentials: 'same-origin', redirect: 'error', signal: controller.signal, headers: body ? { 'Content-Type': 'application/json' } : {}, body: body ? JSON.stringify(body) : undefined });
      if (response.status === 401) { $('signin').hidden = false; throw new Error('Sign in to review and approve this connection.'); }
      const result = await response.json();
      if (!response.ok) throw new Error(result.error === 'invalid_grant' ? 'This request has expired or was already handled. Start a new connection from your IDE or assistant.' : result.error === 'slow_down' ? 'Too many requests. Please wait before trying again.' : 'Mesh could not complete this request. Please retry.');
      $('signin').hidden = true;
      return result;
    } finally { clearTimeout(timer); }
  };
  async function load() {
    $('request').hidden = true;
    try {
      if (code) {
        if (!/^[A-Z2-7]{4}-?[A-Z2-7]{4}$/.test(code)) throw new Error('Invalid connection code. Open the link from your IDE or assistant again.');
        const request = await api('request?user_code=' + encodeURIComponent(code));
        if (!['read','full'].includes(request.scope)) throw new Error('Unsupported permission request.');
        requestedScope = request.scope;
        $('server').textContent = request.audience;
        $('account').textContent = request.account;
        $('client').textContent = ({'mesh-ide':'Mesh IDE','mesh-cli':'Mesh CLI','mesh-mcp':'Mesh MCP client'})[request.client_id] || 'Unknown client';
        $('code').textContent = code.replace(/^([A-Z2-7]{4})([A-Z2-7]{4})$/, '$1-$2');
        $('full-option').hidden = requestedScope !== 'full';
        document.querySelector('input[name=scope][value=' + requestedScope + ']').checked = true;
        $('confirm-code').checked = false;
        $('request').hidden = false;
        status('Review the request, confirm the code, then click Connect.');
      } else {
        const connections = await api('connections');
        $('title').textContent = 'Your Mesh connections';
        $('connections').hidden = false;
        $('connection-list').replaceChildren();
        for (const item of connections) {
          const li = document.createElement('li'), button = document.createElement('button');
          li.textContent = item.client_id + ' · ' + item.scope + ' access · expires ' + new Date(item.expires_at * 1000).toLocaleDateString();
          button.textContent = 'Disconnect';
          button.addEventListener('click', async () => { button.disabled = true; try { await api('disconnect', { grant_id: item.id }); await load(); } catch (err) { status(err.message); button.disabled = false; } });
          li.append(button); $('connection-list').append(li);
        }
        status(connections.length ? 'Only connections approved by your account are shown.' : 'You have no active connections.');
      }
    } catch (err) { status(err.message); }
  }
  async function decide(decision) {
    if (busy || !requestedScope) return;
    if (decision === 'approve' && !$('confirm-code').checked) return;
    busy = true; $('approve').disabled = true; $('deny').disabled = true;
    try {
      const scope = document.querySelector('input[name=scope]:checked').value;
      await api('decision', { user_code: code, scope, decision });
      $('request').hidden = true; $('done').hidden = false;
      $('result-title').textContent = decision === 'approve' ? 'Connection approved' : 'Connection denied';
      $('result-detail').textContent = decision === 'approve' ? 'Return to your IDE or assistant. It will finish connecting automatically. You can revoke access from Manage connections.' : 'No access was granted. You can close this tab.';
      status('You can close this page.');
    } catch (err) { status(err.message); }
    finally { busy = false; $('approve').disabled = false; $('deny').disabled = false; }
  }
  $('consent').addEventListener('submit', event => { event.preventDefault(); void decide('approve'); });
  $('deny').addEventListener('click', () => { void decide('deny'); });
  $('login').addEventListener('submit', async event => {
    event.preventDefault();
    const key = $('key').value; $('key').value = '';
    const button = $('login').querySelector('button'); button.disabled = true;
    try {
      const response = await fetch('api/login', { method: 'POST', credentials: 'same-origin', redirect: 'error', headers: {'Content-Type':'application/json'}, body: JSON.stringify({key}), signal: AbortSignal.timeout(15000) });
      if (!response.ok) throw new Error('Sign-in failed. Check your Mesh access key or try again later.');
      await load();
    } catch (err) { status(err.message); } finally { button.disabled = false; }
  });
  void load();
})();
