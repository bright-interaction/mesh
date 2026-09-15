// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

'use strict';
const fs = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');
const { checkUpdate, downloadUpdate } = require('./updates');

function updateCommand(vscode, version, dependencies = {}) {
  const check = dependencies.check || checkUpdate, download = dependencies.download || downloadUpdate;
  const write = dependencies.write || ((file, bytes) => fs.writeFile(file, bytes, { flag: 'wx', mode: 0o600 }));
  let active = false, disposed = false, controller;
  const run = async () => {
    if (active || disposed || !vscode.workspace.isTrusted) return;
    active = true; controller = new AbortController();
    const signal = controller.signal;
    try {
      const progress = (title, fn) => vscode.window.withProgress({ location: vscode.ProgressLocation.Notification, title, cancellable: true }, async (_, token) => {
        const subscription = token.onCancellationRequested(() => controller.abort());
        if (token.isCancellationRequested) controller.abort();
        try { return await fn(); } finally { subscription.dispose(); }
      });
      const info = await progress('Checking public Mesh IDE releases…', () => check(version, { signal }));
      if (signal.aborted || disposed) return;
      if (!info) { await vscode.window.showInformationMessage('No newer stable Mesh IDE release found in the most recent 300 public releases.'); return; }
      const choice = await vscode.window.showInformationMessage(`Mesh IDE ${info.version} is available (installed: ${version}). Download from bright-interaction/mesh and verify SHA-256? Installation remains manual.`, { modal: true }, 'Download VSIX');
      if (choice !== 'Download VSIX' || signal.aborted || disposed) return;
      const target = await vscode.window.showSaveDialog({ title: 'Save verified Mesh IDE update (choose a new file)', defaultUri: vscode.Uri.file(path.join(os.homedir(), 'Downloads', info.file)), filters: { 'VS Code Extension': ['vsix'] } });
      if (!target || signal.aborted || disposed) return;
      if (target.scheme !== 'file') throw new Error('Local file required');
      const bytes = await progress('Downloading and verifying Mesh IDE…', () => download(info, { signal }));
      if (signal.aborted || disposed) return;
      await write(target.fsPath, bytes);
      await vscode.window.showInformationMessage(`Mesh IDE ${info.version} saved and SHA-256 verified. Use Extensions: Install from VSIX… and select the saved file. Your Mesh binary and viewer were not changed.`);
    } catch (_) {
      if (!signal.aborted && !disposed) await vscode.window.showErrorMessage('Mesh IDE update could not be verified or saved. Check your connection and choose a new writable local filename. No update was installed.');
    } finally { active = false; controller = undefined; }
  };
  return { run, dispose: () => { disposed = true; controller?.abort(); } };
}
module.exports = { updateCommand };
