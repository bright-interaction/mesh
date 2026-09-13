import { createHash } from 'node:crypto';
import { zipSync, unzipSync } from 'fflate';

export const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');
export const shippedSources = ['bridge.js', 'broker.js', 'client.js', 'extension.js', 'lifecycle.js', 'view.css', 'view.js'];
export const shippedAssets = ['index.html', 'style.css', 'gl3d.js', 'app.js', 'search.js', 'dashboard.js', 'docs.js', 'shell.js', 'fonts/geist.woff2', 'fonts/jetbrains-mono.woff2'];
export function validateArchive(bytes, expected, allowDirty = false) {
  const files = unzipSync(bytes);
  const names = ['[Content_Types].xml', 'extension.vsixmanifest', 'extension/LICENSE.txt', 'extension/package.json', 'extension/readme.md', 'extension/media/source.json', ...shippedSources.map(n => 'extension/src/' + n), ...shippedAssets.map(n => 'extension/media/' + n)];
  if (JSON.stringify(Object.keys(files).sort()) !== JSON.stringify(names.sort())) throw new Error('VSIX file allowlist mismatch');
  const json = name => JSON.parse(Buffer.from(files[name]).toString());
  const pkg = json('extension/package.json');
  if (pkg.name !== expected.name || pkg.publisher !== expected.publisher || pkg.version !== expected.version || pkg.main !== './src/extension.js') throw new Error('VSIX package identity mismatch');
  const identities = [...Buffer.from(files['extension.vsixmanifest']).toString().matchAll(/<Identity\b([^>]+)>/g)];
  const attributes = Object.fromEntries([...(identities[0]?.[1] || '').matchAll(/(\w+)="([^"]*)"/g)].map(m => [m[1], m[2]]));
  if (identities.length !== 1 || attributes.Id !== expected.name || attributes.Publisher !== expected.publisher || attributes.Version !== expected.version) throw new Error('VSIX installer identity mismatch');
  const source = json('extension/media/source.json');
  if (source.base_commit !== expected.commit || (!allowDirty && source.dirty !== false)) throw new Error('VSIX source provenance mismatch');
  if (JSON.stringify(Object.keys(source.assets).sort()) !== JSON.stringify([...shippedAssets].sort())) throw new Error('VSIX asset inventory mismatch');
  for (const name of shippedAssets) if (sha256(files['extension/media/' + name]) !== source.assets[name]) throw new Error('VSIX asset hash mismatch: ' + name);
  // Compare every content-bearing file to the current reviewed build, not only
  // to an attacker-controlled manifest carried inside the archive.
  for (const [name, content] of Object.entries(expected.contents || {})) {
    if (!files[name] || sha256(files[name]) !== sha256(content)) throw new Error('VSIX source content mismatch: ' + name);
  }
  return files;
}
export function canonicalArchive(files) {
  const ordered = {};
  for (const name of Object.keys(files).sort()) {
    ordered[name] = [files[name], { mtime: new Date(1980, 0, 1, 0, 0, 0), os: 3, attrs: 0o100644 << 16 }];
  }
  return Buffer.from(zipSync(ordered, { level: 9 }));
}
