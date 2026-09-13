import { mkdir, copyFile, readFile, writeFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
const root = new URL('../', import.meta.url);
await mkdir(new URL('media/fonts/', root), { recursive: true });
const assets = {};
for (const name of ['index.html', 'style.css', 'gl3d.js', 'app.js', 'search.js', 'dashboard.js', 'docs.js', 'shell.js', 'fonts/geist.woff2', 'fonts/jetbrains-mono.woff2']) {
  let content = await readFile(new URL('../internal/web/assets/' + name, root));
  if (name === 'style.css') content = Buffer.from(content.toString().replaceAll('/assets/fonts/', 'fonts/'));
  await writeFile(new URL('media/' + name, root), content);
  assets[name] = createHash('sha256').update(content).digest('hex');
}
await copyFile(new URL('../LICENSE', root), new URL('LICENSE', root));
const base_commit = execFileSync('git', ['rev-parse', 'HEAD'], { cwd: fileURLToPath(root), encoding: 'utf8' }).trim();
const dirty = Boolean(execFileSync('git', ['status', '--porcelain', '--', '.', '../internal/web/assets'], { cwd: fileURLToPath(root), encoding: 'utf8' }).trim());
await writeFile(new URL('media/source.json', root), JSON.stringify({ base_commit, dirty, assets }) + '\n');
console.log('Bundled shared Mesh viewer assets: ' + base_commit.slice(0, 12));
