// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

import { test, expect } from 'bun:test';
import { zipSync } from 'fflate';
import { canonicalArchive, validateArchive, sha256, shippedAssets, shippedSources } from '../scripts/archive.mjs';
import { releasePlan } from '../scripts/release-plan.mjs';

function fixture() {
  const bytes = value => Buffer.from(typeof value === 'string' ? value : JSON.stringify(value));
  const expected = { name: 'mesh-workspace', publisher: 'bright-interaction', version: '0.2.1', commit: 'a'.repeat(40), contents: {} };
  const files = { '[Content_Types].xml': bytes('<Types/>'), 'extension.vsixmanifest': bytes('<PackageManifest><Identity Id="mesh-workspace" Publisher="bright-interaction" Version="0.2.1" /></PackageManifest>'), 'extension/LICENSE.txt': bytes('license'), 'extension/readme.md': bytes('readme'), 'extension/package.json': bytes({ ...expected, main: './src/extension.js' }) };
  const assets = {};
  for (const name of shippedAssets) { files['extension/media/' + name] = bytes(name); assets[name] = sha256(bytes(name)); }
  for (const name of shippedSources) files['extension/src/' + name] = bytes(name);
  files['extension/media/source.json'] = bytes({ base_commit: expected.commit, dirty: false, assets });
  for (const [name, content] of Object.entries(files)) if (name.startsWith('extension/') && name !== 'extension.vsixmanifest') expected.contents[name] = content;
  return { files, expected };
}

test('release handoff validates the exact clean bundle and emits draft-only arguments without execution', () => {
  const { files, expected } = fixture();
  const bytes = canonicalArchive(files);
  const info = { schema: 1, extension: 'bright-interaction.mesh-workspace', version: expected.version, source_commit: expected.commit, dirty: false, file: `mesh-workspace-${expected.version}.vsix`, bytes: bytes.length, sha256: sha256(bytes) };
  const sums = `${info.sha256}  ${info.file}\n`;
  const plan = releasePlan(info, bytes, sums, expected);
  expect(plan.repository).toBe('bright-interaction/mesh'); expect(plan.tag).toBe('ide-v0.2.1');
  expect(plan.draft_command).toContain('--draft'); expect(plan.draft_command).toContain('--verify-tag'); expect(plan.draft_command).toContain('--latest=false');
  expect(plan.draft_command).not.toContain('--clobber'); expect(plan.draft_command).not.toContain('--target');
  for (const patch of [{ dirty: true }, { source_commit: 'b'.repeat(40) }, { sha256: '0'.repeat(64) }, { file: '../bad.vsix' }]) expect(() => releasePlan({ ...info, ...patch }, bytes, sums, expected)).toThrow();
  expect(() => releasePlan(info, bytes, 'wrong sums', expected)).toThrow();
  files['extension/src/extension.js'] = Buffer.from('different runtime');
  const changed = canonicalArchive(files), tampered = { ...info, bytes: changed.length, sha256: sha256(changed) };
  expect(() => releasePlan(tampered, changed, `${tampered.sha256}  ${tampered.file}\n`, expected)).toThrow('source content');
});

test('canonical VSIX ignores original order and ZIP timestamps', () => {
  const { files, expected } = fixture();
  const reversed = Object.fromEntries(Object.entries(files).reverse());
  const first = canonicalArchive(validateArchive(zipSync(files, { mtime: new Date(2020, 1, 2) }), expected));
  const second = canonicalArchive(validateArchive(zipSync(reversed, { mtime: new Date(2025, 7, 8) }), expected));
  expect(first.equals(second)).toBe(true);
  expect(Object.keys(validateArchive(first, expected))).toHaveLength(25);
});

test('archive gate rejects missing, unexpected and traversal paths', () => {
  for (const name of ['extension/.env', 'extension/node_modules/x.js', 'extension/scripts/package.mjs', '../escape']) {
    const { files, expected } = fixture(); files[name] = Buffer.from('unshipped');
    expect(() => validateArchive(zipSync(files), expected)).toThrow('allowlist');
  }
  const { files, expected } = fixture(); delete files['extension/src/lifecycle.js'];
  expect(() => validateArchive(zipSync(files), expected)).toThrow('allowlist');
});

test('archive gate rejects dirty, stale and mismatched package identities', () => {
  for (const change of [{ dirty: true }, { base_commit: 'b'.repeat(40) }]) {
    const { files, expected } = fixture();
    const source = JSON.parse(files['extension/media/source.json']);
    files['extension/media/source.json'] = Buffer.from(JSON.stringify({ ...source, ...change }));
    expect(() => validateArchive(zipSync(files), expected)).toThrow('provenance');
  }
  const { files, expected } = fixture();
  const pkg = JSON.parse(files['extension/package.json']);
  files['extension/package.json'] = Buffer.from(JSON.stringify({ ...pkg, version: '9.0.0' }));
  expect(() => validateArchive(zipSync(files), expected)).toThrow('identity');
});

test('a forged self-consistent asset manifest cannot replace reviewed source', () => {
  const { files, expected } = fixture();
  files['extension/media/app.js'] = Buffer.from('tampered');
  expect(() => validateArchive(zipSync(files), expected)).toThrow('asset hash');
  const source = JSON.parse(files['extension/media/source.json']);
  source.assets['app.js'] = sha256(files['extension/media/app.js']);
  files['extension/media/source.json'] = Buffer.from(JSON.stringify(source));
  expect(() => validateArchive(zipSync(files), expected)).toThrow('source content');
});

test('installer manifest must match the extension identity, not only package.json', () => {
  const { files, expected } = fixture();
  files['extension.vsixmanifest'] = Buffer.from('<PackageManifest><Identity Id="different" Publisher="bright-interaction" Version="0.2.1" /></PackageManifest>');
  expect(() => validateArchive(zipSync(files), expected)).toThrow('installer identity');
});

test('runtime, commands, license and documentation are checked against build input', () => {
  for (const name of ['extension/src/extension.js', 'extension/LICENSE.txt', 'extension/readme.md']) {
    const { files, expected } = fixture(); files[name] = Buffer.from('changed');
    expect(() => validateArchive(zipSync(files), expected)).toThrow('source content');
  }
  const { files, expected } = fixture();
  const pkg = JSON.parse(files['extension/package.json']); pkg.contributes = { commands: [{ command: 'unexpected' }] };
  files['extension/package.json'] = Buffer.from(JSON.stringify(pkg));
  expect(() => validateArchive(zipSync(files), expected)).toThrow('source content');
});
