'use strict';
const vscode = require('vscode');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const { MeshClient, viewerURL } = require('./client');
const { ViewerLifecycle, launchSpec } = require('./lifecycle');
const { Broker } = require('./broker');
const { renderView } = require('./view');
const DEFAULT = 'http://127.0.0.1:7474';
let deactivateCurrent;
function activate(context) {
  let panel, broker, listener, lifecycle, ready = false;
  const deliveredReads = new Set();
  const global = key => vscode.workspace.getConfiguration('mesh').inspect(key)?.globalValue;
  const configured = () => viewerURL(global('viewerUrl') || DEFAULT);
  const options = () => {
    const startup = global('startup') || {};
    return { url: configured(), binary: startup.binary, vault: startup.vault, autoStart: startup.enabled === true && vscode.workspace.isTrusted };
  };
  let previous = JSON.stringify(options());
  const status = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 8);
  status.text = '$(symbol-misc) Mesh'; status.tooltip = 'Open Mesh knowledge workspace'; status.command = 'mesh.open'; status.show();
  function disconnect() { ready = false; broker?.dispose(); listener?.dispose(); broker = listener = undefined; }
  function loading(state) {
    if (!panel?.visible) return;
    disconnect();
    const escape = text => String(text).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
    panel.webview.html = `<html><head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'"></head><body style="font-family:var(--vscode-font-family);padding:24px"><h2>Mesh</h2><p role="status">${escape(state.detail)}</p><p>Use <b>Mesh: Refresh View</b> to retry, or <b>Mesh: Configure Viewer Startup</b> to enable read-only startup.</p></body></html>`;
  }
  function render() {
    if (!panel || !panel.visible) return;
    disconnect();
    const current = panel;
    deliveredReads.clear();
    broker = new Broker(lifecycle.client, message => current.webview.postMessage(message), { ready: () => { ready = true; }, read: path => { deliveredReads.add(path.split('?')[0]); } });
    const active = broker;
    listener = current.webview.onDidReceiveMessage(message => { void active.handle(message); });
    current.webview.html = renderView(fs.readFileSync(path.join(context.extensionPath, 'media/index.html'), 'utf8'), {
      cspSource: current.webview.cspSource,
      resource: name => current.webview.asWebviewUri(vscode.Uri.joinPath(context.extensionUri, name)).toString()
    });
  }
  function newLifecycle() {
    lifecycle?.dispose();
    const config = options();
    lifecycle = new ViewerLifecycle(new MeshClient(config.url), config, state => {
      const connected = ['connected', 'legacy'].includes(state.kind);
      status.text = connected ? '$(symbol-misc) Mesh' : state.kind === 'offline' ? '$(warning) Mesh' : '$(sync~spin) Mesh';
      status.tooltip = state.detail;
      if (connected) { if (!broker) render(); } else loading(state);
    });
  }
  newLifecycle();
  function connect() {
    loading({ detail: 'Connecting to the local viewer…' });
    lifecycle.setVisible(true); lifecycle.retry();
  }
  function attach(created) {
    panel = created;
    panel.webview.options = { enableScripts: true, localResourceRoots: ['media', 'src'].map(p => vscode.Uri.joinPath(context.extensionUri, p)) };
    const disposables = [];
    disposables.push(panel.onDidDispose(() => { disconnect(); lifecycle.setVisible(false); panel = undefined; disposables.forEach(d => d.dispose()); }));
    let visible = panel.visible;
    disposables.push(panel.onDidChangeViewState(({ webviewPanel }) => {
      if (visible === webviewPanel.visible) return;
      visible = webviewPanel.visible; if (visible) connect(); else { disconnect(); lifecycle.setVisible(false); }
    }));
    connect();
  }
  function open() {
    if (!vscode.workspace.isTrusted) return vscode.window.showWarningMessage('Trust this workspace before opening Mesh.');
    if (panel) { panel.reveal(vscode.ViewColumn.Active); return; }
    attach(vscode.window.createWebviewPanel('mesh.workspace', 'Mesh', vscode.ViewColumn.Active, { enableScripts: true }));
  }
  async function configureServer() {
    const value = await vscode.window.showInputBox({ title: 'Mesh local viewer URL', value: configured(), prompt: 'Loopback web viewer, not the MCP port. Startup is separately opt-in.', validateInput: value => { try { viewerURL(value); } catch (_) { return 'Use http://127.0.0.1:7474 or a loopback URL with a base path.'; } } });
    if (value) await vscode.workspace.getConfiguration('mesh').update('viewerUrl', viewerURL(value), vscode.ConfigurationTarget.Global);
  }
  async function configureStartup() {
    if (!vscode.workspace.isTrusted) return;
    const old = options();
    const binaries = await vscode.window.showOpenDialog({ title: 'Choose the Mesh executable', canSelectMany: false, canSelectFiles: true, canSelectFolders: false, defaultUri: vscode.Uri.file(old.binary || path.join(os.homedir(), '.local/bin/mesh')) });
    if (!binaries?.length) return;
    const vaults = await vscode.window.showOpenDialog({ title: 'Choose the existing Mesh vault', canSelectMany: false, canSelectFiles: false, canSelectFolders: true, defaultUri: vscode.Uri.file(old.vault || path.join(os.homedir(), 'Corpus')) });
    if (!vaults?.length) return;
    if (binaries[0].scheme !== 'file' || vaults[0].scheme !== 'file') throw new Error('Local paths required');
    const binary = fs.realpathSync(binaries[0].fsPath), vault = fs.realpathSync(vaults[0].fsPath);
    launchSpec({ url: configured(), binary, vault });
    const answer = await vscode.window.showWarningMessage(`Allow Mesh to start ${binary} as a read-only viewer for ${vault} at ${configured()} when unavailable? No index owner will be started.`, { modal: true }, 'Enable Startup');
    if (answer === 'Enable Startup') await vscode.workspace.getConfiguration('mesh').update('startup', { enabled: true, binary, vault }, vscode.ConfigurationTarget.Global);
  }
  const safe = fn => () => Promise.resolve().then(fn).catch(() => { disconnect(); void vscode.window.showErrorMessage('Mesh could not open. Check Mesh: Set Local Viewer URL and the extension installation.'); });
  for (const [name, fn] of Object.entries({ open, refresh: () => { if (!panel) open(); else connect(); }, configureServer, configureStartup })) context.subscriptions.push(vscode.commands.registerCommand('mesh.' + name, safe(fn)));
  context.subscriptions.push(vscode.window.registerWebviewPanelSerializer('mesh.workspace', { deserializeWebviewPanel: async restored => {
    if (!vscode.workspace.isTrusted) { restored.dispose(); return; } attach(restored);
  } }));
  context.subscriptions.push(vscode.workspace.onDidChangeConfiguration(event => {
    if (!event.affectsConfiguration('mesh.viewerUrl') && !event.affectsConfiguration('mesh.startup')) return;
    try { const next = JSON.stringify(options()); if (next === previous) return; previous = next; disconnect(); newLifecycle(); if (panel?.visible) connect(); } catch (_) { disconnect(); lifecycle.dispose(); panel?.dispose(); }
  }));
  deactivateCurrent = () => { disconnect(); lifecycle.dispose(); };
  context.subscriptions.push(status, { dispose: deactivateCurrent });
  return { diagnostics: () => ({ viewReady: ready, open: Boolean(panel), pending: broker?.pending.size || 0, deliveredReads: [...deliveredReads], connection: lifecycle.state.kind, ownedChild: Boolean(lifecycle.child), ownedChildPID: lifecycle.child?.pid }) };
}
module.exports = { activate, deactivate: () => deactivateCurrent?.() };
