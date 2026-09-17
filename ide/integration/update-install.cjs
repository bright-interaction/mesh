// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Run via --extensionTestsPath ONLY in a disposable VS Code profile. The
// installer is real; release discovery/download use our locally built fixture.
// Reload is recorded, not executed, so the test runner can assert and exit.
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const os = require('node:os');
const vscode = require('vscode');
const { updateCommand } = require('../src/update-command');

exports.run = async () => {
  const root = await fs.realpath(process.env.MESH_UPDATE_TEST_PROFILE || '');
  const temp = await fs.realpath(os.tmpdir());
  assert(path.dirname(root) === temp && path.basename(root).startsWith('mesh-update-editor-'), 'Disposable test profile required');
  // The launcher must put both editor state and extension installs here.
  const marker = JSON.parse(await fs.readFile(path.join(root, 'profile.json'), 'utf8'));
  assert.equal(marker.userData, path.join(root, 'user-data'));
  assert.equal(marker.extensions, path.join(root, 'extensions'));
  const info = JSON.parse(await fs.readFile(path.join(__dirname, '../release/manifest.json'), 'utf8'));
  const bytes = await fs.readFile(path.join(__dirname, '../release', info.file));
  const events = [], errors = [];
  let staged;
  const api = {
    workspace: { isTrusted: true }, Uri: vscode.Uri, ProgressLocation: vscode.ProgressLocation,
    window: {
      withProgress: (_options, fn) => fn({}, { isCancellationRequested: false, onCancellationRequested: () => ({ dispose() {} }) }),
      showInformationMessage: async (_message, ...choices) => choices.includes('Update now') ? 'Update now' : choices.includes('Reload window') ? 'Reload window' : undefined,
      showErrorMessage: async message => errors.push(message),
      showWarningMessage: async message => errors.push(message),
    },
    commands: { executeCommand: async (name, uri) => {
      events.push(name);
      if (name === 'workbench.action.reloadWindow') return;
      assert.equal(name, 'workbench.extensions.installExtension');
      assert.equal(uri.scheme, 'file'); staged = uri.fsPath;
      await vscode.commands.executeCommand(name, uri);
    } },
  };
  const command = updateCommand(api, '0.2.3', { check: async () => info, download: async () => bytes });
  try {
    await command.run();
    assert.deepEqual(errors, []);
    assert.deepEqual(events, ['workbench.extensions.installExtension', 'workbench.action.reloadWindow']);
    assert(staged);
    await assert.rejects(fs.stat(staged), { code: 'ENOENT' });
    const entries = await fs.readdir(marker.extensions);
    let found = false;
    for (const entry of entries.filter(name => name.startsWith('bright-interaction.mesh-workspace-'))) {
      const pkg = JSON.parse(await fs.readFile(path.join(marker.extensions, entry, 'package.json'), 'utf8'));
      if (pkg.version === info.version) found = true;
    }
    assert(found, 'Expected Mesh version was not installed in the disposable profile');
    await fs.writeFile(path.join(root, 'result.json'), JSON.stringify({ passed: true, version: info.version, events, temporaryVSIXRemoved: true, realReloadExecuted: false }) + '\n');
    console.log('PASS real VS Code VSIX install in isolated profile; verified temporary cleanup and reload dispatch.');
  } finally { command.dispose(); }
};
