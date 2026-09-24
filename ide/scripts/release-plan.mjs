import { readFile } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import { validateArchive, shippedSources, shippedAssets, releaseMetadata } from './archive.mjs';
import updates from '../src/updates.js';

export function releasePlan(info, bytes, sums, expected) {
  updates.manifest(info, expected.version);
  releaseMetadata(info);
  releaseMetadata(expected);
  if (info.mesh_release !== expected.mesh_release || info.viewer_api !== expected.viewer_api) throw new Error('Release Mesh pairing metadata disagrees');
  updates.verifyBytes(bytes, info);
  if (info.source_commit !== expected.commit || sums !== `${info.sha256}  ${info.file}\n`) throw new Error('Release provenance/checksums disagree');
  validateArchive(bytes, expected);
  const tag = `ide-v${info.version}`;
  return {
    schema: 1, repository: updates.REPO, tag, source_commit: info.source_commit,
    mesh_release: info.mesh_release, viewer_api: info.viewer_api,
    sha256: info.sha256, bytes: info.bytes,
    prerequisites: [
      'Pass exact-source IDE CI and required checks; obtain publication approval.',
      'Verify the public Mesh mirror matches the reviewed mesh/ subtree; create the independent IDE tag there only with approval.',
      `Verify the paired core release ${info.mesh_release} and its public tag match the reviewed shared viewer source and viewer API ${info.viewer_api}; obtain approval for paired publication.`,
      'Never reuse a published IDE version or overwrite release assets. Keep this IDE release separate from core latest.',
      'Run the command from mesh/ide. It creates only a draft and requires an existing tag. Review its downloaded assets before separately publishing with latest=false.'
    ],
    // Data, not execution: no gh, network, tag creation or remote mutation here.
    draft_command: ['gh', 'release', 'create', tag, '--repo', updates.REPO, '--verify-tag', '--draft', '--latest=false', '--title', `Mesh IDE ${info.version}`, '--notes', `Mesh IDE ${info.version}. Shared viewer source: Mesh ${info.mesh_release}; viewer API ${info.viewer_api}. Source: ${info.source_commit}. VSIX SHA-256: ${info.sha256}. Install manually with Extensions: Install from VSIX. Does not upgrade the Mesh binary or connected server.`, `release/${info.file}`, 'release/manifest.json', 'release/SHA256SUMS']
  };
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  if (process.argv.length !== 2) throw new Error('release:plan accepts no arguments and never publishes');
  const root = fileURLToPath(new URL('../', import.meta.url));
  const git = args => execFileSync('git', args, { cwd: root, encoding: 'utf8' }).trim();
  if (git(['status', '--porcelain', '--', '.', '../internal/web/assets', '../LICENSE', '../VERSION'])) throw new Error('Commit reviewed source before preparing a release');
  const pkg = JSON.parse(await readFile(path.join(root, 'package.json'), 'utf8'));
  const info = JSON.parse(await readFile(path.join(root, 'release/manifest.json'), 'utf8'));
  updates.manifest(info, pkg.version); // validate the filename before joining it
  const expected = releaseMetadata({ name: pkg.name, publisher: pkg.publisher, version: pkg.version, commit: git(['rev-parse', 'HEAD']), mesh_release: (await readFile(path.join(root, '../VERSION'), 'utf8')).trim(), viewer_api: 1, contents: {} });
  for (const [entry, file] of [['extension/package.json', 'package.json'], ['extension/media/source.json', 'media/source.json'], ['extension/readme.md', 'README.md'], ['extension/LICENSE.txt', 'LICENSE'], ...shippedSources.map(n => ['extension/src/' + n, 'src/' + n]), ...shippedAssets.map(n => ['extension/media/' + n, 'media/' + n])]) expected.contents[entry] = await readFile(path.join(root, file));
  console.log(JSON.stringify(releasePlan(info, await readFile(path.join(root, 'release', info.file)), await readFile(path.join(root, 'release/SHA256SUMS'), 'utf8'), expected), null, 2));
}
