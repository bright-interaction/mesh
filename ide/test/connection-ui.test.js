// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

'use strict';
const { test, expect } = require('bun:test');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { RemoteAuth } = require('../src/client');
function setup() {
  const commands = new Map(), secrets = new Map(), prompts = [], calls = [], lifecycles = [];
  const globals = { viewerUrl: 'https://mesh.example/app', startup: { enabled: true, vault: '/old/vault', binary: '/old/mesh' } };
  let configurationChanged, input = 'fixture-key';
  const disposable = () => ({ dispose() {} });
  const panel = { visible: true, webview: { options: {}, html: '', cspSource: 'vscode-test:', onDidReceiveMessage: disposable, postMessage() {}, asWebviewUri: uri => uri }, onDidDispose: disposable, onDidChangeViewState: disposable, reveal() {}, dispose() {} };
  const vscode = {
    workspace: { isTrusted: true, getConfiguration: () => ({ inspect: key => ({ globalValue: globals[key], workspaceValue: 'https://untrusted.example' }), update: async (key, value) => { globals[key] = value; configurationChanged({ affectsConfiguration: () => true }); } }), onDidChangeConfiguration: fn => { configurationChanged = fn; return disposable(); } },
    window: { createStatusBarItem: () => ({ show() {}, dispose() {} }), showInputBox: async options => { prompts.push(options); return input; }, showInformationMessage() {}, showWarningMessage() {}, showErrorMessage() {}, createWebviewPanel: () => panel, registerWebviewPanelSerializer: disposable },
    commands: { registerCommand: (name, fn) => { commands.set(name, fn); return disposable(); } },
    StatusBarAlignment: { Left: 1 }, ViewColumn: { Active: 1 }, ConfigurationTarget: { Global: 1 },
    Uri: { joinPath: (_base, name) => ({ toString: () => 'vscode-test:' + name }) },
  };
  const context = { subscriptions: [], extension: { packageJSON: { version: '0.2.3' } }, extensionPath: path.resolve(__dirname, '..'), extensionUri: {}, secrets: { get: async key => secrets.get(key), store: async (key, value) => secrets.set(key, value), delete: async key => secrets.delete(key) } };
  const exported = { exports: {} };
  const source = fs.readFileSync(path.join(__dirname, '../src/extension.js'), 'utf8');
  vm.runInNewContext(source, { module: exported, AbortController, require: name => {
    if (name === 'vscode') return vscode;
    if (name === './client') return { ...require('../src/client'), RemoteAuth: class extends RemoteAuth { constructor(store) { super(store, async (url, _signal, opts) => { calls.push({ url, opts }); return { status: 200, body: '{"counts":{}}' }; }); } } };
    if (name === './lifecycle') return { ViewerLifecycle: class {
      constructor(client, options, onState) { Object.assign(this, { client, options, onState, state: { kind: 'idle' } }); lifecycles.push(this); }
      dispose() { this.disposed = true; } setVisible() {} retry() {}
    } };
    if (name === './update-command') return { updateCommand: () => ({ dispose() {}, run() {} }) };
    return name.startsWith('./') ? require('../src/' + name.slice(2)) : require(name);
  } });
  exported.exports.activate(context);
  return { commands, secrets, prompts, calls, lifecycles, globals, panel, vscode, dispose: exported.exports.deactivate, input: value => { input = value; } };
}
test('native remote sign-in uses masked input, secure storage and no local startup settings', async () => {
  const h = setup();
  try {
    expect(h.calls).toEqual([]);
    expect(h.lifecycles[0].options).toEqual({ url: 'https://mesh.example/app', autoStart: false });
    await h.commands.get('mesh.signIn')();
    expect(h.prompts[0].password).toBe(true);
    expect(h.prompts[0].prompt).toContain('https://mesh.example/app');
    expect([...h.secrets.values()]).toEqual(['fixture-key']);
    expect(h.calls).toEqual([{ url: 'https://mesh.example/app/api/status', opts: { token: 'fixture-key' } }]);
    expect(JSON.stringify(h.globals)).not.toContain('fixture-key');
    expect(h.panel.webview.html).not.toContain('fixture-key');
    expect(h.panel.webview.html).toContain('command:mesh.signIn');
    expect(h.panel.webview.options.enableScripts).toBe(false);
    expect(h.panel.webview.options.enableCommandUris).toEqual(['mesh.signIn', 'mesh.configureServer', 'mesh.configureStartup', 'mesh.refresh']);
    h.lifecycles.at(-1).onState({ kind: 'connected', detail: 'fixture ready' });
    expect(h.panel.webview.options.enableCommandUris).toBe(false);
    expect(h.panel.webview.options.enableScripts).toBe(true);
    await h.commands.get('mesh.signOut')();
    expect(h.secrets.size).toBe(0);
    expect(h.panel.webview.html).toContain('Signed out');
  } finally { h.dispose(); }
});
test('cancelled prompts and untrusted workspaces never send or save a key', async () => {
  const h = setup();
  try {
    h.input(undefined);
    await h.commands.get('mesh.signIn')();
    h.input('fixture-key'); h.vscode.workspace.isTrusted = false;
    await h.commands.get('mesh.signIn')();
    expect(h.calls).toEqual([]);
    expect(h.secrets.size).toBe(0);
    expect(h.prompts).toHaveLength(1);
  } finally { h.dispose(); }
});
