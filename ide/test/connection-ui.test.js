// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

'use strict';
const { test, expect } = require('bun:test');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { RemoteAuth } = require('../src/client');
function setup() {
  const commands = new Map(), secrets = new Map(), prompts = [], calls = [], lifecycles = [], messages = [], external = [], connectionCalls = [], progress = [];
  let choice = 'Open approval page', currentTime = 100000;
  const globals = { viewerUrl: 'https://mesh.example/app', startup: { enabled: true, vault: '/old/vault', binary: '/old/mesh' } };
  let configurationChanged, input = 'fixture-key';
  const updater = { cancelled: 0, disposed: false, dependencies: null, snapshots: [], sources: [] };
  const disposable = () => ({ dispose() {} });
  const panel = { visible: true, webview: { options: {}, html: '', cspSource: 'vscode-test:', onDidReceiveMessage: disposable, postMessage() {}, asWebviewUri: uri => uri }, onDidDispose: disposable, onDidChangeViewState: disposable, reveal() {}, dispose() {} };
  const vscode = {
    workspace: { isTrusted: true, getConfiguration: () => ({ inspect: key => ({ globalValue: globals[key], workspaceValue: 'https://untrusted.example' }), update: async (key, value) => { globals[key] = value; configurationChanged({ affectsConfiguration: () => true }); } }), onDidChangeConfiguration: fn => { configurationChanged = fn; return disposable(); } },
    window: { createStatusBarItem: () => ({ show() {}, dispose() {} }), showInputBox: async options => { prompts.push(options); return input; }, showInformationMessage: async (...args) => { messages.push(args); return choice; }, showWarningMessage() {}, showErrorMessage: message => messages.push([message]), createWebviewPanel: () => panel, registerWebviewPanelSerializer: disposable, withProgress: async (_options, run) => run({ report: value => progress.push(value) }, { onCancellationRequested: disposable }) },
    commands: { registerCommand: (name, fn) => { commands.set(name, fn); return disposable(); } },
    StatusBarAlignment: { Left: 1 }, ViewColumn: { Active: 1 }, ConfigurationTarget: { Global: 1 }, ProgressLocation: { Notification: 15 },
    env: { openExternal: async uri => { external.push(uri); return true; } },
    Uri: { joinPath: (_base, name) => ({ toString: () => 'vscode-test:' + name }), parse: value => value },
  };
  const context = { subscriptions: [], extension: { packageJSON: { version: '0.2.3' } }, extensionPath: path.resolve(__dirname, '..'), extensionUri: {}, secrets: { get: async key => secrets.get(key), store: async (key, value) => secrets.set(key, value), delete: async key => secrets.delete(key) } };
  const exported = { exports: {} };
  const source = fs.readFileSync(path.join(__dirname, '../src/extension.js'), 'utf8');
  vm.runInNewContext(source, { module: exported, AbortController, require: name => {
    if (name === 'vscode') return vscode;
    if (name === './client') return { ...require('../src/client'), RemoteAuth: class extends RemoteAuth { constructor(store) { super(store, async (url, _signal, opts) => { calls.push({ url, opts }); return { status: 200, body: '{"counts":{}}' }; }, {
      now: () => currentTime, wait: async ms => { currentTime += ms; }, post: async (url, body) => {
        connectionCalls.push({url, body});
        if (url.endsWith('/device')) return {status:200,data:{device_code:'mesh_device_'+'a'.repeat(43),user_code:'ABCD-EFGH',expires_in:600,interval:5,verification_uri:globals.viewerUrl+'/connect',verification_uri_complete:globals.viewerUrl+'/connect?user_code=ABCD-EFGH'}};
        if (url.endsWith('/token')) return {status:200,data:{access_token:'mesh_access_'+'a'.repeat(43),refresh_token:'mesh_refresh_'+'a'.repeat(43),grant_id:'mesh_grant_'+'a'.repeat(43),token_type:'Bearer',scope:'full',expires_in:900}};
        return {status:200,data:{}};
      }
    }); } } };
    if (name === './lifecycle') return { ViewerLifecycle: class {
      constructor(client, options, onState) { Object.assign(this, { client, options, onState, state: { kind: 'idle' } }); lifecycles.push(this); }
      dispose() { this.disposed = true; } setVisible() {} retry() {}
    } };
    if (name === './update-command') return { updateCommand: (_, version, dependencies) => {
      updater.dependencies = dependencies;
      return { dispose() { updater.disposed = true; }, cancel() { updater.cancelled++; }, run: () => dependencies.overview(new AbortController().signal) };
    } };
    if (name === './coordinated-updates') return { coordinatedOverview: async (_, source, serverStatus, { signal }) => {
      updater.sources.push(source); const snapshot = await serverStatus(signal); updater.snapshots.push(snapshot); return snapshot;
    } };
    return name.startsWith('./') ? require('../src/' + name.slice(2)) : require(name);
  } });
  exported.exports.activate(context);
  return { updater, commands, secrets, prompts, calls, lifecycles, globals, panel, vscode, messages, external, connectionCalls, progress, choose: value => { choice = value; }, dispose: exported.exports.deactivate, input: value => { input = value; } };
}
test('native remote sign-in uses masked input, secure storage and no local startup settings', async () => {
  const h = setup();
  try {
    expect(h.calls).toEqual([]);
    expect(h.lifecycles[0].options).toEqual({ url: 'https://mesh.example/app', autoStart: false });
    await h.commands.get('mesh.signInKey')();
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

test('Connect displays matching code and opens approval without asking for an access key', async () => {
  const h=setup();
  try {
    await h.commands.get('mesh.signIn')();
    expect(h.prompts).toHaveLength(0);
    expect(h.external).toEqual(['https://mesh.example/app/connect?user_code=ABCD-EFGH']);
    expect(h.messages.some(args=>args[0].includes('ABCD-EFGH'))).toBe(true);
    expect(h.progress[0].message).toContain('ABCD-EFGH');
    expect(JSON.stringify(h.messages)).not.toContain('mesh_device_');
    expect(JSON.stringify(h.globals)).not.toContain('mesh_access_');
    expect(h.panel.webview.html).not.toContain('mesh_access_');
    expect([...h.secrets.keys()]).toEqual(['mesh.viewer-connection:https://mesh.example/app']);
    await h.commands.get('mesh.signOut')();
    expect(h.connectionCalls.at(-1).url).toEndWith('/revoke');
    expect(h.secrets.size).toBe(0);
    expect(h.panel.webview.html).toContain('revoked on Mesh');
  } finally {h.dispose();}
});

test('cancelled browser prompt cancels request and never opens browser or exchanges token', async () => {
  const h=setup();h.choose(undefined);
  try {
    await h.commands.get('mesh.signIn')();
    expect(h.external).toHaveLength(0);
    expect(h.connectionCalls.map(call=>call.url.split('/').at(-1))).toEqual(['device','cancel']);
    expect(h.secrets.size).toBe(0);
  } finally {h.dispose();}
});
test('cancelled prompts and untrusted workspaces never send or save a key', async () => {
  const h = setup();
  try {
    h.input(undefined);
    await h.commands.get('mesh.signInKey')();
    h.input('fixture-key'); h.vscode.workspace.isTrusted = false;
    await h.commands.get('mesh.signInKey')();
    expect(h.calls).toEqual([]);
    expect(h.secrets.size).toBe(0);
    expect(h.prompts).toHaveLength(1);
  } finally { h.dispose(); }
});
test('coordinated update status uses only the approved viewer and existing auth without process startup', async () => {
  const h = setup();
  try {
    expect(h.calls).toHaveLength(0);
    h.secrets.set('mesh.viewer-key:https://mesh.example/app', 'fixture-key');
    await h.commands.get('mesh.updateNow')();
    expect(h.calls).toEqual([{ url: 'https://mesh.example/app/api/status', opts: { token: 'fixture-key' } }]);
    expect(h.updater.snapshots).toEqual([{ remote: true, status: { counts: {} } }]);
    expect(h.updater.sources[0].mesh_release).toBe(fs.readFileSync(path.resolve(__dirname, '../../VERSION'), 'utf8').trim());
    expect(h.connectionCalls).toHaveLength(0); expect(h.external).toHaveLength(0);
    expect(h.lifecycles).toHaveLength(1);
    await h.vscode.workspace.getConfiguration().update('viewerUrl', 'https://mesh.other/app');
    expect(h.updater.cancelled).toBe(1);
    expect(h.updater.dependencies.context()).toBe('https://mesh.other/app');
    await h.commands.get('mesh.signOut')();
    expect(h.updater.cancelled).toBe(2);
    h.dispose(); expect(h.updater.disposed).toBe(true);
  } finally { h.dispose(); }
});
