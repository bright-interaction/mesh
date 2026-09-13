// Run only in an isolated VS Code profile. No production editor tabs/settings.
const assert = require('node:assert/strict');
const vscode = require('vscode');
const fs = require('node:fs');
const path = require('node:path');
exports.run = async () => {
  await vscode.workspace.getConfiguration('mesh').update('viewerUrl', process.env.MESH_IDE_TEST_URL || 'http://127.0.0.1:7476', vscode.ConfigurationTarget.Global);
  const extension = vscode.extensions.getExtension('bright-interaction.mesh-workspace');
  assert(extension, 'Mesh extension missing');
  const api = await extension.activate();
  const commands = await vscode.commands.getCommands(true);
  for (const command of ['mesh.open', 'mesh.refresh', 'mesh.configureServer']) assert(commands.includes(command));
  const ready = async () => {
    for (let i = 0; i < 300; i++) {
      const d = api.diagnostics();
      if (d.viewReady && d.deliveredReads.includes('/graph.json') && d.deliveredReads.includes('/api/status')) return;
      await new Promise(resolve => setTimeout(resolve, 100));
    }
    assert.fail('Real editor did not load scripts and receive graph/status from the viewer');
  };
  await vscode.commands.executeCommand('mesh.open'); await ready();
  const tabs = () => vscode.window.tabGroups.all.flatMap(group => group.tabs).filter(t => t.label === 'Mesh');
  assert.equal(tabs().length, 1);
  assert(tabs()[0].input instanceof vscode.TabInputWebview);
  await vscode.commands.executeCommand('mesh.open'); assert.equal(tabs().length, 1);
  await vscode.commands.executeCommand('mesh.refresh'); await ready();
  await vscode.window.tabGroups.close(tabs()); assert.equal(api.diagnostics().pending, 0);
  await vscode.commands.executeCommand('mesh.open'); await ready();
  await vscode.window.tabGroups.close(tabs());
  const results = path.join(__dirname, '../test-results');
  fs.mkdirSync(results, { recursive: true });
  fs.writeFileSync(path.join(results, 'editor-result.json'), JSON.stringify({ passed: true, vscode: vscode.version, extension: extension.packageJSON.version, checks: ['activation', 'native-command-tab', 'singleton', 'refresh', 'close-reopen', 'live-graph-status'], at: new Date().toISOString() }) + '\n');
  console.log('PASS real VS Code activation, native command/tab, singleton, refresh, close/reopen and live graph/status bridge.');
};
