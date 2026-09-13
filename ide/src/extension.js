'use strict';
const vscode = require('vscode');
const fs = require('node:fs');
const path = require('node:path');
const { MeshClient, viewerURL } = require('./client');
const { Broker } = require('./broker');
const { renderView } = require('./view');
const DEFAULT = 'http://127.0.0.1:7474';
function activate(context) {
  let panel, broker, listener, ready = false;
  const deliveredReads = new Set();
  const configured = () => viewerURL(vscode.workspace.getConfiguration('mesh').inspect('viewerUrl')?.globalValue || DEFAULT);
  let previous = configured();
  function disconnect() { ready = false; broker?.dispose(); listener?.dispose(); broker = listener = undefined; }
  function reload() {
    if (!panel || !panel.visible) return;
    disconnect();
    const current = panel;
    deliveredReads.clear();
    broker = new Broker(new MeshClient(configured()), message => current.webview.postMessage(message), { ready: () => { ready = true; }, read: path => { deliveredReads.add(path.split('?')[0]); } });
    const active = broker;
    listener = current.webview.onDidReceiveMessage(message => { void active.handle(message); });
    current.webview.html = renderView(fs.readFileSync(path.join(context.extensionPath, 'media/index.html'), 'utf8'), {
      cspSource: current.webview.cspSource,
      resource: name => current.webview.asWebviewUri(vscode.Uri.joinPath(context.extensionUri, name)).toString()
    });
  }
  function attach(created) {
    panel = created;
    panel.webview.options = { enableScripts: true, localResourceRoots: ['media', 'src'].map(p => vscode.Uri.joinPath(context.extensionUri, p)) };
    const disposables = [];
    disposables.push(panel.onDidDispose(() => { disconnect(); panel = undefined; disposables.forEach(d => d.dispose()); }));
    let visible = panel.visible;
    disposables.push(panel.onDidChangeViewState(({ webviewPanel }) => {
      if (visible === webviewPanel.visible) return;
      visible = webviewPanel.visible; if (visible) reload(); else disconnect();
    }));
    reload();
  }
  function open() {
    if (!vscode.workspace.isTrusted) return vscode.window.showWarningMessage('Trust this workspace before opening Mesh.');
    if (panel) { panel.reveal(vscode.ViewColumn.Active); return; }
    attach(vscode.window.createWebviewPanel('mesh.workspace', 'Mesh', vscode.ViewColumn.Active, { enableScripts: true }));
  }
  async function configureServer() {
    const value = await vscode.window.showInputBox({ title: 'Mesh local viewer URL', value: configured(), prompt: 'Existing loopback web viewer, not the MCP port. No process or index owner is started.', validateInput: value => { try { viewerURL(value); } catch (_) { return 'Use http://127.0.0.1:7474 or a loopback URL with a base path.'; } } });
    if (value) await vscode.workspace.getConfiguration('mesh').update('viewerUrl', viewerURL(value), vscode.ConfigurationTarget.Global);
  }
  const safe = fn => () => Promise.resolve().then(fn).catch(() => { disconnect(); void vscode.window.showErrorMessage('Mesh could not open. Check Mesh: Set Local Viewer URL and the extension installation.'); });
  for (const [name, fn] of Object.entries({ open, refresh: () => { if (!panel) open(); else reload(); }, configureServer })) context.subscriptions.push(vscode.commands.registerCommand('mesh.' + name, safe(fn)));
  context.subscriptions.push(vscode.window.registerWebviewPanelSerializer('mesh.workspace', { deserializeWebviewPanel: async restored => {
    if (!vscode.workspace.isTrusted) { restored.dispose(); return; } attach(restored);
  } }));
  context.subscriptions.push(vscode.workspace.onDidChangeConfiguration(event => {
    if (!event.affectsConfiguration('mesh.viewerUrl')) return;
    try { const next = configured(); if (next === previous) return; previous = next; disconnect(); reload(); } catch (_) { disconnect(); panel?.dispose(); }
  }));
  const status = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 8);
  status.text = '$(symbol-misc) Mesh'; status.tooltip = 'Open Mesh knowledge workspace'; status.command = 'mesh.open'; status.show();
  context.subscriptions.push(status, { dispose: disconnect });
  return { diagnostics: () => ({ viewReady: ready, open: Boolean(panel), pending: broker?.pending.size || 0, deliveredReads: [...deliveredReads] }) };
}
module.exports = { activate };
