// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
'use strict';
const https = require('node:https');
const CLIENT = 'mesh-ide';
function failure(message, code = 'AUTH_REQUIRED') { return Object.assign(new Error(message), { code }); }
function checkAbort(signal) { if (signal?.aborted) throw failure('Connection cancelled.', 'ABORTED'); }
function delay(ms, signal) {
  return new Promise((resolve, reject) => {
    checkAbort(signal);
    const finish = () => { signal?.removeEventListener('abort', abort); resolve(); };
    const timer = setTimeout(finish, ms);
    const abort = () => { clearTimeout(timer); signal?.removeEventListener('abort', abort); reject(failure('Connection cancelled.', 'ABORTED')); };
    signal?.addEventListener('abort', abort, { once: true });
  });
}
function postJSON(url, body, signal) {
  return new Promise((resolve, reject) => {
    const target = new URL(url);
    if (target.protocol !== 'https:' || target.username || target.password || target.search || target.hash || !/\/api\/connect\/(device|token|cancel|revoke)$/.test(target.pathname)) throw failure('Unapproved connection endpoint.');
    const payload = Buffer.from(JSON.stringify(body));
    if (payload.length > 8192) throw failure('Connection request too large.');
    let settled = false, request, timer;
    const finish = (error, value) => { if (settled) return; settled = true; clearTimeout(timer); signal?.removeEventListener('abort', abort); error ? reject(error) : resolve(value); };
    const abort = () => { const err = failure('Connection cancelled.', 'ABORTED'); finish(err); request?.destroy(err); };
    if (signal?.aborted) { abort(); return; }
    signal?.addEventListener('abort', abort, { once: true });
    request = https.request(target, { method: 'POST', agent: false, headers: { Accept: 'application/json', 'Content-Type': 'application/json', 'Content-Length': payload.length } }, response => {
      // Never follow redirects with device or renewal credentials, even same-host.
      if (response.statusCode >= 300 && response.statusCode < 400) { finish(failure('Mesh redirected a connection request. Check the viewer URL.')); response.destroy(); request.destroy(); return; }
      if (response.statusCode === 404) { finish(null, { status: 404, data: {} }); response.destroy(); request.destroy(); return; }
      if (!/^application\/json(?:;|$)/i.test(response.headers['content-type'] || '')) { finish(failure('Mesh returned an unsupported connection response.')); response.destroy(); request.destroy(); return; }
      let size = 0; const chunks = [];
      response.on('data', chunk => { size += chunk.length; if (size > 32768) { finish(failure('Connection response too large.')); response.destroy(); request.destroy(); } else chunks.push(chunk); });
      response.on('error', () => finish(failure('Mesh connection response was interrupted.')));
      response.on('end', () => { try { finish(null, { status: response.statusCode, data: JSON.parse(Buffer.concat(chunks).toString('utf8')) }); } catch (_) { finish(failure('Invalid Mesh connection response.')); } });
    });
    timer = setTimeout(() => { const err = failure('Mesh connection request timed out.', 'TIMEOUT'); finish(err); request.destroy(err); }, 15000);
    request.on('error', () => finish(failure('Could not reach Mesh securely. Check the server URL and network.')));
    request.end(payload);
  });
}
function credential(value, prefix) { return typeof value === 'string' && new RegExp('^' + prefix + '[A-Za-z0-9_-]{43}$').test(value); }
function tokens(data, now, base) {
  if (!data || !credential(data.access_token, 'mesh_access_') || !credential(data.refresh_token, 'mesh_refresh_') || !credential(data.grant_id, 'mesh_grant_') || data.token_type !== 'Bearer' || !['read', 'full'].includes(data.scope) || !Number.isInteger(data.expires_in) || data.expires_in < 1 || data.expires_in > 900) throw failure('Invalid Mesh connection credentials.');
  return { version: 1, base, accessToken: data.access_token, refreshToken: data.refresh_token, grantID: data.grant_id, scope: data.scope, expiresAt: now + data.expires_in * 1000 };
}
function endpointAllowed(configured, actual) { return actual === configured || new URL(configured).pathname === '/' && actual === configured + '/app'; }

class BrowserConnections {
  constructor(secrets, { post = postJSON, now = Date.now, wait = delay } = {}) { this.secrets = secrets; this.post = post; this.now = now; this.wait = wait; }
  key(base) { return 'mesh.viewer-connection:' + base; }
  async read(base) {
    const raw = await this.secrets.get?.(this.key(base));
    if (raw === undefined) return;
    let value;
    try { value = JSON.parse(raw); } catch (_) { throw failure('Stored Mesh connection is invalid. Reconnect to Mesh.'); }
    if (!value || value.version !== 1 || !endpointAllowed(base, value.base) || !credential(value.accessToken, 'mesh_access_') || !credential(value.refreshToken, 'mesh_refresh_') || !credential(value.grantID, 'mesh_grant_') || !['read','full'].includes(value.scope) || !Number.isFinite(value.expiresAt)) throw failure('Stored Mesh connection is invalid. Reconnect to Mesh.');
    return value;
  }
  async authorize(base, signal, showRequest) {
    let endpoint = base, device, record, acquired = false;
    try {
      checkAbort(signal);
      let reply = await this.post(endpoint + '/api/connect/device', { client_id: CLIENT, scope: 'full' }, signal);
      if (reply.status === 404 && new URL(base).pathname === '/') { endpoint += '/app'; reply = await this.post(endpoint + '/api/connect/device', { client_id: CLIENT, scope: 'full' }, signal); }
      if (reply.status === 404) throw failure('This Mesh server has not enabled browser connections. Ask its administrator to enable them, or use Mesh: Sign In with Access Key.', 'NOT_CONFIGURED');
      if (reply.status !== 200) throw failure('Mesh could not start a connection. Please try again later.');
      device = reply.data;
      if (!device || !credential(device.device_code, 'mesh_device_') || !/^[A-Z2-7]{4}-[A-Z2-7]{4}$/.test(device.user_code) || !Number.isInteger(device.expires_in) || device.expires_in < 1 || device.expires_in > 600 || !Number.isInteger(device.interval) || device.interval < 5 || device.interval > 60 || device.verification_uri !== endpoint + '/connect' || device.verification_uri_complete !== endpoint + '/connect?user_code=' + encodeURIComponent(device.user_code)) throw failure('Mesh returned an unsafe approval request.');
      const deadline = this.now() + device.expires_in * 1000;
      checkAbort(signal);
      await showRequest({ code: device.user_code, url: device.verification_uri_complete, server: endpoint });
      let interval = device.interval * 1000;
      while (this.now() < deadline) {
        await this.wait(Math.min(interval, deadline - this.now()), signal); checkAbort(signal);
        if (this.now() >= deadline) break;
        try { reply = await this.post(endpoint + '/api/connect/token', { client_id: CLIENT, device_code: device.device_code, grant_type: 'urn:ietf:params:oauth:grant-type:device_code' }, signal); }
        catch (err) { if (err.code === 'TIMEOUT') { interval = Math.min(interval * 2, 60000); continue; } throw err; }
        if (reply.status === 200) { record = tokens(reply.data, this.now(), endpoint); acquired = true; checkAbort(signal); return record; }
        if (reply.status === 400 && reply.data?.error === 'authorization_pending') continue;
        if ([400,429].includes(reply.status) && reply.data?.error === 'slow_down') { interval += 5000; continue; }
        throw failure(reply.data?.error === 'access_denied' ? 'The Mesh connection was denied.' : reply.data?.error === 'expired_token' ? 'The Mesh connection request expired. Start Connect again.' : 'Mesh could not complete this connection. Start Connect again.');
      }
      throw failure('The Mesh connection request expired. Start Connect again.');
    } catch (err) {
      // Bounded best-effort cleanup, without the already-cancelled UI signal.
      // Never retry an exchange that might have succeeded.
      if (acquired && record) await this.revoke(record).catch(() => {});
      else if (device && credential(device.device_code, 'mesh_device_')) await this.post(endpoint + '/api/connect/cancel', { client_id: CLIENT, device_code: device.device_code }, AbortSignal.timeout(5000)).catch(() => {});
      throw err;
    }
  }
  // Caller serializes this with all credential writes. Persisting the marker
  // BEFORE rotation prevents a crash or uncertain response from replaying a used
  // refresh token and revoking a newly-issued credential family.
  async access(base, signal) {
    const current = await this.read(base); checkAbort(signal);
    if (!current) return;
    if (current.refreshing) throw failure('Mesh renewal was interrupted. Reconnect to Mesh.');
    if (current.expiresAt > this.now() + 30000) return current.accessToken;
    await this.secrets.store(this.key(base), JSON.stringify({ ...current, refreshing: true }));
    if (signal?.aborted) { await this.secrets.store(this.key(base), JSON.stringify(current)); checkAbort(signal); }
    const reply = await this.post(current.base + '/api/connect/token', { client_id: CLIENT, refresh_token: current.refreshToken, grant_type: 'refresh_token' }, signal);
    if (reply.status !== 200) throw failure('Mesh could not renew this connection. Reconnect to Mesh.');
    const next = tokens(reply.data, this.now(), current.base);
    if (next.grantID !== current.grantID || next.scope !== current.scope) throw failure('Mesh changed the approved connection during renewal. Reconnect to Mesh.');
    // Store the rotated pair even if the caller cancelled while the response was
    // arriving. Sign-out is serialized after this write and revokes that pair.
    await this.secrets.store(this.key(base), JSON.stringify(next));
    checkAbort(signal); return next.accessToken;
  }
  async revoke(record) {
    const reply = await this.post(record.base + '/api/connect/revoke', { client_id: CLIENT, token: record.refreshToken }, AbortSignal.timeout(5000));
    if (reply.status !== 200) throw failure('Mesh did not confirm disconnection. Retry, or revoke the connection in your browser.');
  }
}
module.exports = { BrowserConnections, postJSON, delay };
