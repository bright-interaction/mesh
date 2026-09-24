// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const { checkUpdate, stable, compare, manifest } = require('./updates');

const release = value => typeof value === 'string' && value.startsWith('v') && stable(value.slice(1)) ? value : null;

// Consume only bounded, validated version fields. A server cannot supply commands,
// links, installer destinations or claims of deployment authority to this flow.
function serverSummary(snapshot) {
  const status = snapshot?.status;
  if (!status || typeof status !== 'object' || Array.isArray(status)) {
    return { text: 'Mesh server: unavailable or not connected; update status unknown.', instructions: 'Connect to Mesh, then check again. No server changes were made.' };
  }
  const viewer = status.viewer;
  const api = viewer?.name === 'mesh' && Number.isSafeInteger(viewer.apiVersion) && viewer.apiVersion > 0 ? viewer.apiVersion : null;
  const current = release(Object.prototype.hasOwnProperty.call(status, 'release') ? status.release : status.update?.current);
  const latest = release(status.update?.latest);
  let state = 'update status unknown (check disabled, unavailable, or legacy server)';
  if (current && latest && release(status.update?.current) === current && typeof status.update.available === 'boolean') {
    const newer = compare(latest.slice(1), current.slice(1)) > 0;
    if (status.update.available === newer) state = newer ? `${latest} available` : 'no newer release in the server’s cached check';
  }
  const mode = ['owner', 'read-only'].includes(viewer?.mode) ? viewer.mode : 'unknown';
  const location = snapshot.remote === true ? 'hosted server' : snapshot.remote === false ? 'local viewer' : 'server';
  const instructions = snapshot.remote === true
    ? 'The hosted Mesh server is updated by its deployment operator. Ask the operator to review the release, retain a verified backup and rollback image, deploy the approved image, and verify health and access. A Mesh account role does not grant deployment access. Running mesh upgrade on your computer does not update this server.'
    : 'The local Mesh viewer is updated by whoever manages its executable and service. Confirm that exact installation before using its supported updater. Then arrange an explicit restart of the affected viewer. The IDE does not replace binaries or restart an external viewer or index owner.';
  return {
    api, current,
    text: `Mesh ${location}: ${current || 'version unknown'}; ${state}. Viewer API: ${api || 'unknown'}; index mode: ${mode}.`,
    instructions: instructions + (mode === 'owner' ? ' This viewer owns its index: include index-owner shutdown and recovery checks in the rollout.' : '') + ' Existing permissions and current connection/revocation state must be preserved.'
  };
}

async function coordinatedOverview(installed, source, serverStatus, options = {}) {
  // Independent reads: an offline server must not hide an IDE release, and a
  // failed public release check must not masquerade as "up to date".
  const [ideResult, serverResult] = await Promise.allSettled([
    Promise.resolve().then(() => (options.check || checkUpdate)(installed, { signal: options.signal })),
    Promise.resolve().then(() => serverStatus(options.signal))
  ]);
  let info = ideResult.status === 'fulfilled' ? ideResult.value : null;
  let ideFailed = ideResult.status === 'rejected';
  if (info) {
    try { manifest(info, info.version); if (compare(info.version, installed) <= 0) throw new Error('Not newer'); }
    catch (_) { info = null; ideFailed = true; }
  }
  const server = serverSummary(serverResult.status === 'fulfilled' ? serverResult.value : null);
  const bundled = release(source?.mesh_release);
  const lines = [
    `Mesh update overview`,
    `IDE: ${installed}; bundled Mesh viewer: ${bundled || 'source release unknown'}.`,
    server.text,
    ideFailed ? 'IDE release check failed; availability unknown. Retry later.' : info ? `IDE ${info.version} is available.` : 'No newer stable IDE release found in the most recent 300 public releases.'
  ];
  if (info?.mesh_release) {
    lines.push(`The IDE update is built from Mesh ${info.mesh_release} (viewer API ${info.viewer_api}). This records a source pairing, not an installed server upgrade.`);
    if (server.api != null && server.api !== info.viewer_api) {
      lines.push('The server reports a different viewer API. IDE installation is blocked until compatibility is resolved.');
      info = null;
    } else if (server.api == null) {
      lines.push('Server compatibility could not be checked. Updating the IDE will not repair or upgrade the server.');
    }
  } else if (info) lines.push('This older IDE release has no Mesh source pairing; compatibility has not been confirmed.');
  lines.push('Update now installs only the IDE after checksum verification. Server deployment and window reload require separate approval.');
  return { info, message: lines.join('\n\n'), serverInstructions: server.instructions };
}

module.exports = { release, serverSummary, coordinatedOverview };
