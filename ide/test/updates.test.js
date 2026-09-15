// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

import { test, expect } from 'bun:test';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { mkdtemp, writeFile, readFile, symlink, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { compare, manifest, verifyBytes, readBounded, checkUpdate, downloadUpdate } = require('../src/updates');
const { updateCommand } = require('../src/update-command');
const data = Buffer.from('fixture VSIX');
const info = { schema: 1, extension: 'bright-interaction.mesh-workspace', version: '0.2.3', source_commit: 'a'.repeat(40), dirty: false, file: 'mesh-workspace-0.2.3.vsix', bytes: data.length, sha256: createHash('sha256').update(data).digest('hex') };
const base = 'https://github.com/bright-interaction/mesh/releases/download/ide-v0.2.3/';
const listing = 'https://api.github.com/repos/bright-interaction/mesh/releases?per_page=100&page=1';
const row = { tag_name: 'ide-v0.2.3', draft: false, prerelease: false, assets: [info.file, 'manifest.json', 'SHA256SUMS'].map(name => ({ name, state: 'uploaded', browser_download_url: base + name })) };
const json = value => new Response(JSON.stringify(value));
function network(rows = [row], metadata = info) {
  const calls = [];
  const options = { fetchImpl: async (url, opts) => {
    calls.push(url); expect(opts.redirect).toBe('manual'); expect(opts.credentials).toBe('omit'); expect(opts.headers.Authorization).toBeUndefined();
    if (url === listing) return json(rows);
    if (url === base + 'manifest.json') return json(metadata);
    if (url === base + 'SHA256SUMS') return new Response(`${metadata.sha256}  ${metadata.file}\n`);
    if (url === base + info.file) return new Response(data);
    throw new Error('unexpected URL');
  } };
  return { calls, options };
}
test('stable numeric versions and strict clean manifest contract', () => {
  expect(compare('0.10.0', '0.9.9')).toBeGreaterThan(0);
  for (const version of ['01.2.3', '1.0.0-beta', '../1.0.0', '999999999999.0.0']) expect(() => compare(version, '1.0.0')).toThrow();
  for (const patch of [{ dirty: true }, { schema: 2 }, { extension: 'other' }, { version: '0.2.4' }, { file: '../x' }, { bytes: 0 }, { bytes: 17 * 1024 * 1024 }, { sha256: 'a' }, { source_commit: 'main' }]) expect(() => manifest({ ...info, ...patch }, info.version)).toThrow();
  expect(verifyBytes(data, info)).toEqual(data);
  expect(() => verifyBytes(Buffer.from('bad'), info)).toThrow();
});
test('explicit discovery ignores core, drafts, prereleases and old versions; validates before download', async () => {
  const { calls, options } = network([{ ...row, tag_name: 'v99.0.0' }, { ...row, tag_name: 'ide-v99.0.0', draft: true }, { ...row, tag_name: 'ide-v98.0.0', prerelease: true }, row]);
  expect(await checkUpdate('0.2.2', options)).toEqual(info);
  expect(calls).toHaveLength(3); expect(calls).not.toContain(base + info.file);
  expect(await downloadUpdate(info, options)).toEqual(data);
  expect(await checkUpdate('0.2.3', network().options)).toBeNull();
});
test('reject incomplete, redirected and conflicting asset metadata', async () => {
  for (const assets of [[], [...row.assets, row.assets[0]], row.assets.map(a => ({ ...a, browser_download_url: 'https://evil.invalid/file' }))]) await expect(checkUpdate('0.2.2', network([{ ...row, assets }]).options)).rejects.toThrow();
  await expect(checkUpdate('0.2.2', network([row], { ...info, dirty: true }).options)).rejects.toThrow();
  const { options } = network(); const original = options.fetchImpl;
  options.fetchImpl = (url, opts) => url.endsWith('SHA256SUMS') ? new Response('different') : original(url, opts);
  await expect(checkUpdate('0.2.2', options)).rejects.toThrow('checksums');
});
test('pagination is bounded and does not trust server next links', async () => {
  const calls = [];
  expect(await checkUpdate('0.2.2', { fetchImpl: async url => { calls.push(url); return json(Array.from({ length: 100 }, () => ({ tag_name: 'v0.1.0' }))); } })).toBeNull();
  expect(calls).toHaveLength(3); expect(calls[2]).toEndWith('page=3');
});
test('bounded network refuses errors, declared and streamed oversize, and unsafe redirects', async () => {
  for (const response of [() => new Response('no', { status: 429 }), () => new Response('small', { headers: { 'content-length': '1000' } }), () => new Response('123456')]) await expect(readBounded(listing, 5, { fetchImpl: response })).rejects.toThrow();
  for (const location of ['http://release-assets.githubusercontent.com/x', 'https://evil.invalid/x', 'https://release-assets.githubusercontent.com.evil.invalid/x', 'https://user@release-assets.githubusercontent.com/x', 'https://release-assets.githubusercontent.com:444/x']) {
    let calls = 0;
    await expect(readBounded(listing, 100, { fetchImpl: async () => { calls++; return new Response(null, { status: 302, headers: { location } }); } })).rejects.toThrow();
    expect(calls).toBe(1);
  }
  let calls = 0;
  expect((await readBounded(base + info.file, 100, { fetchImpl: async () => ++calls === 1 ? new Response(null, { status: 302, headers: { location: 'https://release-assets.githubusercontent.com/asset?signature=fixture' } }) : new Response(data) })).equals(data)).toBe(true);
  calls = 0;
  await expect(readBounded(base + info.file, 100, { fetchImpl: async () => { calls++; return new Response(null, { status: 302, headers: { location: 'https://release-assets.githubusercontent.com/loop' } }); } })).rejects.toThrow();
  expect(calls).toBe(4);
});
test('timeout and cancellation abort in-flight transport', async () => {
  const transport = (_, { signal }) => new Promise((resolve, reject) => { if (signal.aborted) reject(new Error('aborted')); else signal.addEventListener('abort', () => reject(new Error('aborted')), { once: true }); });
  await expect(readBounded(listing, 100, { fetchImpl: transport, timeout: 5 })).rejects.toThrow('aborted');
  const controller = new AbortController(); controller.abort();
  await expect(readBounded(listing, 100, { fetchImpl: transport, signal: controller.signal })).rejects.toThrow('cancelled');
});
function ui(overrides = {}) {
  const log = { checks: 0, downloads: 0, writes: [], messages: [], errors: [] };
  const vscode = { workspace: { isTrusted: true }, ProgressLocation: { Notification: 15 }, Uri: { file: fsPath => ({ scheme: 'file', fsPath }) }, window: {
    withProgress: async (_, fn) => fn({}, { isCancellationRequested: false, onCancellationRequested: () => ({ dispose() {} }) }),
    showInformationMessage: async (message, ...rest) => { log.messages.push(message); return rest.includes('Download VSIX') ? 'Download VSIX' : undefined; },
    showSaveDialog: async () => ({ scheme: 'file', fsPath: '/fixture/update.vsix' }),
    showErrorMessage: async message => { log.errors.push(message); }
  } };
  const command = updateCommand(vscode, '0.2.2', { check: async () => { log.checks++; return info; }, download: async () => { log.downloads++; return data; }, write: async (...args) => { log.writes.push(args); }, ...overrides });
  return { vscode, command, log };
}
test('activation is offline; manual consent saves verified bytes without installation', async () => {
  const { command, log } = ui(); expect(log.checks).toBe(0);
  await command.run(); expect(log.checks).toBe(1); expect(log.downloads).toBe(1); expect(log.writes).toEqual([['/fixture/update.vsix', data]]);
  expect(log.messages.at(-1)).toContain('Install from VSIX'); expect(log.errors).toHaveLength(0);
  const pkg = JSON.parse(readFileSync(new URL('../package.json', import.meta.url)));
  expect(pkg.contributes.commands.some(c => c.command === 'mesh.checkUpdates')).toBe(true);
});
test('cancelled dialogs, untrusted workspaces and disposed command never download or write', async () => {
  for (const kind of ['consent', 'save', 'trust', 'dispose']) {
    const { command, vscode, log } = ui();
    if (kind === 'consent') vscode.window.showInformationMessage = async () => undefined;
    if (kind === 'save') vscode.window.showSaveDialog = async () => undefined;
    if (kind === 'trust') vscode.workspace.isTrusted = false;
    if (kind === 'dispose') command.dispose();
    await command.run(); expect(log.downloads).toBe(0); expect(log.writes).toHaveLength(0);
  }
});
test('verification failure, nonlocal target and save collision never report success', async () => {
  for (const kind of ['verify', 'nonlocal', 'collision']) {
    const { command, vscode, log } = ui(kind === 'verify' ? { download: async () => { throw new Error('checksum'); } } : kind === 'collision' ? { write: async () => { throw new Error('EEXIST'); } } : {});
    if (kind === 'nonlocal') vscode.window.showSaveDialog = async () => ({ scheme: 'vscode-remote', fsPath: '/remote' });
    await command.run(); expect(log.writes).toHaveLength(0); expect(log.errors).toHaveLength(1); expect(log.messages.some(m => m.includes('saved and'))).toBe(false);
  }
});
test('repeated commands coalesce; disposal aborts and prevents later writes', async () => {
  let release, signal;
  const { command, log } = ui({ check: () => { throw new Error('unexpected'); } });
  command.dispose(); await command.run(); expect(log.checks).toBe(0);
  let calls = 0;
  const fixture = ui({ check: (_, opts) => { calls++; signal = opts.signal; return new Promise(resolve => { release = resolve; }); } });
  const first = fixture.command.run(); await fixture.command.run(); expect(calls).toBe(1);
  fixture.command.dispose(); expect(signal.aborted).toBe(true); release(info); await first;
  expect(fixture.log.writes).toHaveLength(0); expect(fixture.log.downloads).toBe(0);
});

test('real exclusive save preserves existing files and symlinks', async () => {
  const directory = await mkdtemp(path.join(tmpdir(), 'mesh-update-save-'));
  try {
    const existing = path.join(directory, 'existing.vsix'), link = path.join(directory, 'link.vsix'), fresh = path.join(directory, 'fresh.vsix');
    await writeFile(existing, 'preserve'); await symlink(existing, link);
    for (const target of [existing, link, fresh]) {
      const { vscode, log } = ui();
      vscode.window.showSaveDialog = async () => ({ scheme: 'file', fsPath: target });
      const command = updateCommand(vscode, '0.2.2', { check: async () => info, download: async () => data });
      await command.run(); command.dispose();
      expect(log.errors.length).toBe(target === fresh ? 0 : 1);
      expect((await readFile(existing)).toString()).toBe('preserve');
    }
    expect(await readFile(fresh)).toEqual(data);
  } finally { await rm(directory, { recursive: true }); } // only this test-created fixture
});

test('cancelling download prevents saving late completion', async () => {
  let cancel, release;
  const fixture = ui({ download: async (_, { signal }) => new Promise(resolve => { release = () => { expect(signal.aborted).toBe(true); resolve(data); }; }) });
  fixture.vscode.window.withProgress = async (_, fn) => fn({}, { isCancellationRequested: false, onCancellationRequested: callback => { cancel = callback; return { dispose() {} }; } });
  const task = fixture.command.run();
  for (let i = 0; i < 20 && !release; i++) await Promise.resolve();
  expect(release).toBeDefined(); cancel(); release(); await task;
  expect(fixture.log.writes).toHaveLength(0); expect(fixture.log.errors).toHaveLength(0);
});
