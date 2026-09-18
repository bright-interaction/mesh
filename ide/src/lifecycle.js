// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const path = require('node:path');
const fs = require('node:fs');
const { spawn } = require('node:child_process');
const { viewerURL } = require('./client');

function failure(code) { return Object.assign(new Error(code), { code }); }
function readiness(data, vault) {
  if (vault && (typeof data.vault !== 'string' || !path.isAbsolute(data.vault) || path.resolve(data.vault) !== path.resolve(vault))) throw failure('WRONG_VAULT');
  const v = data.viewer;
  if (v == null) return { kind: 'legacy', detail: 'Legacy viewer: ownership and freshness unknown.' };
  if (v.name !== 'mesh' || v.apiVersion !== 1 || !['read-only', 'owner'].includes(v.mode) || !['current-to-observed-index', 'unknown'].includes(v.freshness) || !['observed', 'not-observed', 'unknown'].includes(v.indexOwner)) throw failure('INCOMPATIBLE');
  return { kind: 'connected', detail: `${v.mode}; index ${v.freshness}; owner ${v.indexOwner}${vault ? '' : '; vault not pinned'}.` };
}
function launchSpec(options, inherited = process.env) {
  const u = new URL(viewerURL(options.url));
  if (u.pathname !== '/' || !u.port || Number(u.port) < 1) throw failure('STARTUP_SETTINGS');
  if (!path.isAbsolute(options.binary || '') || !path.isAbsolute(options.vault || '')) throw failure('STARTUP_SETTINGS');
  if (!fs.statSync(options.binary).isFile() || !fs.statSync(options.vault).isDirectory()) throw failure('STARTUP_SETTINGS');
  fs.accessSync(options.binary, fs.constants.X_OK);
  const env = { ...inherited };
  for (const key of Object.keys(env)) if (key.startsWith('MESH_UI_') || key === 'MESH_WEB_DEV') delete env[key];
  env.MESH_UI_OWN_INDEX = '0';
  return { binary: options.binary, args: ['ui', options.vault, '--addr', u.host, '--own-index=false'], options: { cwd: options.vault, env, shell: false, windowsHide: true, stdio: 'ignore' } };
}
function startViewer(options) {
  const spec = launchSpec(options);
  return spawn(spec.binary, spec.args, spec.options);
}

// Only the extension host drives this controller. Renderer messages cannot
// choose executables, alter settings or request process operations.
class ViewerLifecycle {
  constructor(client, options, onState, deps = {}) {
    this.client = client; this.options = options; this.onState = onState;
    this.start = deps.start || startViewer; this.now = deps.now || Date.now;
    this.setTimer = deps.setTimer || setTimeout; this.clearTimer = deps.clearTimer || clearTimeout;
    this.visible = false; this.disposed = false; this.epoch = 0; this.failures = 0; this.starts = [];
    this.state = { kind: 'idle', detail: 'Open Mesh to connect.' };
  }
  emit(state) { this.state = state; this.onState(state); }
  schedule(ms) {
    this.clearTimer(this.timer);
    if (this.visible && !this.disposed) this.timer = this.setTimer(() => { void this.check(); }, ms);
  }
  setVisible(visible) {
    if (this.disposed || visible === this.visible) return;
    this.visible = visible; this.epoch++; this.controller?.abort(); this.clearTimer(this.timer);
    if (!visible && this.childDeadline) this.stopChild();
    if (visible) this.schedule(0);
  }
  retry() { if (!this.disposed) { this.epoch++; this.controller?.abort(); this.schedule(0); } }
  async check() {
    if (!this.visible || this.disposed) return;
    if (this.busy) { this.schedule(100); return; }
    this.clearTimer(this.timer);
    this.busy = true; const epoch = this.epoch;
    const current = () => !this.disposed && this.visible && epoch === this.epoch;
    const controller = this.controller = new AbortController();
    let delay = 30000;
    try {
      if (!['connected', 'legacy'].includes(this.state.kind)) this.emit({ kind: this.child ? 'starting' : 'connecting', detail: 'Connecting to the local viewer…' });
      const response = await this.client.connect(controller.signal);
      if (!current()) return;
      const state = readiness(JSON.parse(response.body), this.options.vault);
      if (this.child && (state.kind === 'legacy' || JSON.parse(response.body).viewer.mode !== 'read-only')) {
        this.stopChild(); throw failure('UNSAFE_CHILD');
      }
      this.failures = 0; this.childDeadline = undefined; this.emit(state);
    } catch (error) {
      if (!current()) return;
      this.failures++;
      delay = Math.min(30000, 1000 * 2 ** Math.min(this.failures - 1, 5));
      let detail = ({ WRONG_VAULT: 'Wrong vault. Check Mesh startup settings.', INCOMPATIBLE: 'Unsupported viewer API. Upgrade Mesh or change its URL.', UNSAFE_CHILD: 'Started viewer did not confirm read-only mode and was stopped.' })[error.code] || 'Viewer unavailable. Use Mesh: Refresh View to retry or configure startup.';
      if (this.child && this.childDeadline && this.now() >= this.childDeadline) {
        this.stopChild(); detail = 'Viewer startup timed out. Check the binary and vault.';
      } else if (error.code === 'ECONNREFUSED' && !this.child && this.options.autoStart) {
        this.starts = this.starts.filter(t => this.now() - t < 600000);
        if (this.starts.length >= 3) detail = 'Automatic startup paused after three attempts in ten minutes.';
        else {
          this.starts.push(this.now());
          try {
            const child = this.child = this.start(this.options);
            this.childDeadline = this.now() + 60000;
            const ended = () => {
              if (this.child !== child) return;
              this.child = undefined; this.childDeadline = undefined;
              this.emit({ kind: 'offline', detail: 'Viewer process exited; reconnecting while the tab is visible.' });
              this.schedule(1000);
            };
            child.once('error', ended); child.once('exit', ended);
            detail = 'Starting a read-only viewer; the existing index owner is unchanged.';
          } catch (_) { detail = 'Cannot start viewer. Check the approved executable and vault paths.'; }
        }
      }
      this.emit({ kind: this.child ? 'starting' : 'offline', detail });
    } finally {
      this.busy = false;
      if (!this.disposed && this.visible) this.schedule(epoch === this.epoch ? delay : 0);
    }
  }
  stopChild() { const child = this.child; this.child = undefined; this.childDeadline = undefined; child?.kill('SIGTERM'); }
  dispose() { this.disposed = true; this.visible = false; this.epoch++; this.clearTimer(this.timer); this.controller?.abort(); this.stopChild(); }
}
module.exports = { ViewerLifecycle, readiness, launchSpec, startViewer };
