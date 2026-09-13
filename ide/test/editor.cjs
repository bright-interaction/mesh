// Run only in an isolated VS Code profile. No production editor tabs/settings.
const assert = require('node:assert/strict');
const vscode = require('vscode');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const net = require('node:net');
const { spawnSync } = require('node:child_process');
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
exports.run = async () => {
  await vscode.workspace.getConfiguration('mesh').update('viewerUrl', process.env.MESH_IDE_TEST_URL || 'http://127.0.0.1:7476', vscode.ConfigurationTarget.Global);
  const extension = vscode.extensions.getExtension('bright-interaction.mesh-workspace');
  assert(extension, 'Mesh extension missing');
  const api = await extension.activate();
  const commands = await vscode.commands.getCommands(true);
  for (const command of ['mesh.open', 'mesh.refresh', 'mesh.configureServer', 'mesh.configureStartup', 'mesh.checkUpdates']) assert(commands.includes(command));
  const ready = async () => {
    for (let i = 0; i < 300; i++) {
      const d = api.diagnostics();
      if (d.viewReady && d.deliveredReads.includes('/graph.json') && d.deliveredReads.includes('/api/status')) return;
      await new Promise(resolve => setTimeout(resolve, 100));
    }
    assert.fail('Real editor did not load scripts and receive graph/status from the viewer');
  };
  await vscode.commands.executeCommand('mesh.open'); await ready();
  assert.equal(api.diagnostics().ownedChild, false, 'external viewer must not be adopted');
  const tabs = () => vscode.window.tabGroups.all.flatMap(group => group.tabs).filter(t => t.label === 'Mesh');
  assert.equal(tabs().length, 1);
  assert(tabs()[0].input instanceof vscode.TabInputWebview);
  await vscode.commands.executeCommand('mesh.open'); assert.equal(tabs().length, 1);
  await vscode.commands.executeCommand('mesh.refresh'); await ready();
  await vscode.window.tabGroups.close(tabs()); assert.equal(api.diagnostics().pending, 0);
  await vscode.commands.executeCommand('mesh.open'); await ready();
  await vscode.window.tabGroups.close(tabs());
  const checks = ['activation', 'native-command-tab', 'singleton', 'refresh', 'close-reopen', 'live-graph-status', 'external-viewer-not-adopted'];
  if (process.env.MESH_IDE_TEST_STARTUP_BINARY) {
    const binary = fs.realpathSync(process.env.MESH_IDE_TEST_STARTUP_BINARY);
    const vault = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'mesh-editor-startup-')));
    fs.writeFileSync(path.join(vault, 'fixture.md'), '---\nid: editor-startup-fixture\ntype: note\nwhen: 2026-09-13\n---\n# Editor startup fixture\nRead-only startup test.\n');
    const indexed = spawnSync(binary, ['index', vault], { encoding: 'utf8', timeout: 30000, shell: false });
    assert.equal(indexed.status, 0, 'isolated fixture indexing failed: ' + indexed.stderr);
    const listener = net.createServer();
    await new Promise((resolve, reject) => { listener.once('error', reject); listener.listen(0, '127.0.0.1', resolve); });
    const url = 'http://127.0.0.1:' + listener.address().port;
    await new Promise(resolve => listener.close(resolve));
    const config = vscode.workspace.getConfiguration('mesh');
    const originalURL = config.inspect('viewerUrl')?.globalValue;
    let pid;
    try {
      await config.update('viewerUrl', url, vscode.ConfigurationTarget.Global);
      await config.update('startup', { enabled: true, binary, vault }, vscode.ConfigurationTarget.Global);
      await delay(300);
      await vscode.commands.executeCommand('mesh.open'); await ready();
      pid = api.diagnostics().ownedChildPID;
      assert(Number.isInteger(pid), 'no owned viewer process');
      const status = await (await fetch(url + '/api/status')).json();
      assert.equal(status.vault, vault);
      assert.equal(status.viewer.mode, 'read-only');
      assert.equal(status.viewer.indexOwner, 'not-observed');
      process.kill(pid, 'SIGTERM'); // only the child this isolated extension launched
      for (let i = 0; i < 300 && api.diagnostics().ownedChildPID === pid; i++) await delay(100);
      await ready();
      const replacement = api.diagnostics().ownedChildPID;
      assert(Number.isInteger(replacement) && replacement !== pid, 'viewer was not restarted after exit');
      pid = replacement;
      await vscode.window.tabGroups.close(tabs());
      await vscode.commands.executeCommand('mesh.open'); await ready();
      assert.equal(api.diagnostics().ownedChildPID, pid, 'ready hidden child should be reused');
      checks.push('opt-in-read-only-startup', 'owned-child-exit-reconnect', 'hidden-child-reuse');
    } finally {
      await config.update('startup', { enabled: false }, vscode.ConfigurationTarget.Global);
      if (pid) {
        let alive = true;
        for (let i = 0; i < 100 && alive; i++) {
          await delay(100);
          try { process.kill(pid, 0); } catch (error) { if (error.code === 'ESRCH') alive = false; else throw error; }
        }
        assert.equal(alive, false, 'owned viewer survived lifecycle disposal');
      }
      await config.update('viewerUrl', originalURL, vscode.ConfigurationTarget.Global);
      await vscode.window.tabGroups.close(tabs());
      fs.rmSync(vault, { recursive: true }); // exact test-created fixture, after child shutdown
    }
    checks.push('owned-child-disposal');
  }
  const results = path.join(__dirname, '../test-results');
  fs.mkdirSync(results, { recursive: true });
  fs.writeFileSync(path.join(results, 'editor-result.json'), JSON.stringify({ passed: true, vscode: vscode.version, extension: extension.packageJSON.version, checks, at: new Date().toISOString() }) + '\n');
  console.log('PASS real VS Code activation, native command/tab, singleton, refresh, close/reopen and live graph/status bridge.');
};
