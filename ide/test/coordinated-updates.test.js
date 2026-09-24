// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
const { test, expect } = require('bun:test');
const { createHash } = require('node:crypto');
const { coordinatedOverview, serverSummary } = require('../src/coordinated-updates');
const { manifest } = require('../src/updates');
const { updateCommand } = require('../src/update-command');
const bytes = Buffer.from('fixture');
const candidate = { schema: 1, extension: 'bright-interaction.mesh-workspace', version: '0.3.1', source_commit: 'a'.repeat(40), dirty: false, file: 'mesh-workspace-0.3.1.vsix', bytes: bytes.length, sha256: createHash('sha256').update(bytes).digest('hex'), mesh_release: 'v0.41.7', viewer_api: 1 };
const source = { mesh_release: 'v0.41.0', viewer_api: 1 };
const snapshot = { remote: true, status: { release: 'v0.41.0', viewer: { name: 'mesh', apiVersion: 1, mode: 'owner' }, update: { current: 'v0.41.0', latest: 'v0.41.7', available: true } } };
const overview = (server = snapshot, check = async () => candidate) => coordinatedOverview('0.3.0', source, async () => server, { check });

test('paired manifests remain backwards compatible but reject partial or unsupported pairing', () => {
  expect(manifest(candidate, candidate.version)).toBe(candidate);
  const { mesh_release, viewer_api, ...legacy } = candidate;
  expect(manifest(legacy, legacy.version)).toBe(legacy);
  for (const fields of [{ mesh_release }, { viewer_api }, { mesh_release: '0.41.7', viewer_api: 1 }, { mesh_release: 'v0.41.7-pre', viewer_api: 1 }, { mesh_release: 'v00.41.7', viewer_api: 1 }, { mesh_release, viewer_api: 2 }, { mesh_release: 'v0.41.7\nrun', viewer_api: 1 }, { mesh_release: 'v0.41.7\n', viewer_api: 1 }]) {
    expect(() => manifest({ ...legacy, ...fields }, legacy.version)).toThrow('pairing');
  }
  for (const fields of [{ source_commit: 'a'.repeat(40) + '\n' }, { sha256: candidate.sha256 + '\n' }, { version: '0.3.1\n' }]) expect(() => manifest({ ...candidate, ...fields }, fields.version || candidate.version)).toThrow();
});
test('overview shows server, installed bundle and candidate without upgrading them together', async () => {
  const result = await overview();
  expect(result.info).toBe(candidate);
  for (const text of ['IDE: 0.3.0', 'bundled Mesh viewer: v0.41.0', 'hosted server: v0.41.0', 'v0.41.7 available', 'source pairing', 'separate approval']) expect(result.message).toContain(text);
  expect(result.serverInstructions).toContain('owns its index');
  expect(result.serverInstructions).toContain('does not grant deployment access');
  expect(result.serverInstructions).toContain('Running mesh upgrade on your computer does not update this server');
});
test('component checks run concurrently; cancellation signal is propagated to both', async () => {
  const controller = new AbortController(); let serverStarted = false, releaseCheck;
  const task = coordinatedOverview('0.3.0', source, async signal => { expect(signal).toBe(controller.signal); serverStarted = true; return snapshot; }, { signal: controller.signal, check: (_, { signal }) => { expect(signal).toBe(controller.signal); return new Promise(resolve => { releaseCheck = resolve; }); } });
  await Promise.resolve(); expect(serverStarted).toBe(true); releaseCheck(candidate);
  expect((await task).info).toBe(candidate);
});
test('independent failures never become up-to-date or conceal the other component', async () => {
  const noServer = await coordinatedOverview('0.3.0', source, async () => { throw new Error('secret'); }, { check: async () => candidate });
  expect(noServer.info).toBe(candidate);
  expect(noServer.message).toContain('unavailable or not connected');
  expect(noServer.message).toContain('compatibility could not be checked');
  expect(noServer.message).not.toContain('secret');
  const noRelease = await overview(snapshot, async () => { throw new Error('private'); });
  expect(noRelease.info).toBeNull();
  expect(noRelease.message).toContain('IDE release check failed');
  expect(noRelease.message).toContain('v0.41.7 available');
  expect(noRelease.message).not.toContain('No newer');
  expect(noRelease.message).not.toContain('private');
  expect((await overview(snapshot, () => { throw new Error('synchronous'); })).message).toContain('v0.41.7 available');
});
test('disabled check keeps reported version; legacy check can supply a version; unsupported API blocks installation', async () => {
  expect(serverSummary({ remote: false, status: { release: 'v0.41.7' } }).text).toContain('local viewer: v0.41.7; update status unknown');
  const old = serverSummary({ status: { update: { current: 'v0.40.1', latest: 'v0.40.1', available: false } } });
  expect(old.text).toContain('v0.40.1; no newer release');
  const mismatched = await overview({ ...snapshot, status: { ...snapshot.status, viewer: { name: 'mesh', apiVersion: 2 } } });
  expect(mismatched.info).toBeNull(); expect(mismatched.message).toContain('installation is blocked');
});
test('stale or contradictory server notices remain unknown rather than claim readiness', () => {
  for (const change of [{ current: 'v0.41.1' }, { latest: 'v0.40.0' }, { available: false }, { available: 'true' }]) {
    const result = serverSummary({ ...snapshot, status: { ...snapshot.status, update: { ...snapshot.status.update, ...change } } });
    expect(result.text).toContain('update status unknown');
  }
  const invalid = serverSummary({ ...snapshot, status: { ...snapshot.status, release: 'dev' } });
  expect(invalid.text).toContain('version unknown');
});
test('hostile versions, URLs, commands and member roles cannot enter operator guidance', async () => {
  const poison = { status: { release: 'v0.41.7\nsecret', viewer: { apiVersion: 'secret', mode: 'secret' }, role: 'admin', update: { current: '<script>secret', latest: 'secret', url: 'https://secret.invalid', command: 'secret', prebuilt_command: 'secret' } } };
  const result = await overview(poison);
  expect(result.message).not.toContain('secret'); expect(result.serverInstructions).not.toContain('secret');
  expect(result.message).toContain('version unknown');
  const invalidRelease = await overview(snapshot, async () => ({ ...candidate, mesh_release: 'secret' }));
  expect(invalidRelease.info).toBeNull(); expect(invalidRelease.message).toContain('availability unknown');
});

function host(choice = 'Update now', dependencies = {}) {
  const log = { prompts: [], calls: [], checks: 0, errors: [] };
  const vscode = {
    workspace: { isTrusted: true }, ProgressLocation: { Notification: 1 }, Uri: { file: fsPath => ({ fsPath }) },
    commands: { executeCommand: async name => log.calls.push(name) },
    window: {
      withProgress: async (_, fn) => fn({}, { isCancellationRequested: false, onCancellationRequested: () => ({ dispose() {} }) }),
      showInformationMessage: async (message, _, ...buttons) => { log.prompts.push({ message, buttons }); return buttons.includes(choice) ? choice : undefined; },
      showErrorMessage: async message => log.errors.push(message), showWarningMessage: async message => log.errors.push(message)
    }
  };
  const command = updateCommand(vscode, '0.3.0', {
    overview: async () => { log.checks++; return overview(); },
    check: async () => { throw new Error('must not check twice'); },
    download: async () => { log.calls.push('download'); return bytes; },
    stage: async () => ({ file: '/fixture/update.vsix', cleanup: async () => log.calls.push('cleanup') }),
    ...dependencies
  });
  return { log, vscode, command };
}
test('one coordinated confirmation installs only IDE; separate reload remains optional', async () => {
  const h = host(); expect(h.log.checks).toBe(0);
  await h.command.run();
  expect(h.log.checks).toBe(1);
  expect(h.log.prompts[0].message).toContain('Mesh update overview');
  expect(h.log.calls).toEqual(['download', 'workbench.extensions.installExtension', 'cleanup']);
  expect(h.log.prompts[1].message).toContain('Save your work first');
  expect(h.log.errors).toHaveLength(0);
});
test('server action is read-only operator guidance, including when IDE is already current', async () => {
  for (const candidateAvailable of [true, false]) {
    const h = host('Server update steps', { overview: async () => overview(snapshot, async () => candidateAvailable ? candidate : null) });
    await h.command.run();
    expect(h.log.prompts[1].message).toContain('deployment operator');
    expect(h.log.calls).toHaveLength(0); expect(h.log.errors).toHaveLength(0);
  }
});
test('configuration change, trust loss, cancellation and disposal fence late overview completion', async () => {
  for (const reason of ['configuration', 'trust', 'cancel', 'dispose']) {
    let binding = 'server-a', finish;
    const h = host('Update now', { context: () => binding, overview: () => new Promise(resolve => { finish = resolve; }) });
    const task = h.command.run(); await Promise.resolve();
    if (reason === 'configuration') binding = 'server-b';
    if (reason === 'trust') h.vscode.workspace.isTrusted = false;
    if (reason === 'cancel') h.command.cancel();
    if (reason === 'dispose') h.command.dispose();
    finish(await overview()); await task;
    expect(h.log.calls).toHaveLength(0); expect(h.log.prompts).toHaveLength(0);
  }
});
test('bad context cannot leave the updater permanently busy', async () => {
  let bad = true;
  const h = host('Server update steps', { context: () => { if (bad) throw new Error('invalid URL'); return 'valid'; } });
  await h.command.run(); bad = false; await h.command.run();
  expect(h.log.checks).toBe(1); expect(h.log.prompts).toHaveLength(2); expect(h.log.calls).toHaveLength(0);
});
