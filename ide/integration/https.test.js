// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

'use strict';
const { test, expect } = require('bun:test');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

test('real HTTPS: trusted and untrusted TLS, sign-in, scoped reads, redirects, cancellation and sign-out', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mesh-https-test-'));
  try {
    const cert = path.join(dir, 'cert.pem');
    const generated = spawnSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-subj', '/CN=127.0.0.1', '-addext', 'subjectAltName=IP:127.0.0.1', '-keyout', path.join(dir, 'key.pem'), '-out', cert], { timeout: 15000, encoding: 'utf8' });
    expect(generated.status).toBe(0);
    for (const mode of ['untrusted', 'trusted']) {
      // Trust this disposable certificate only inside the fixture subprocess.
      // Never alter system trust or disable TLS verification.
      const env = { ...process.env };
      delete env.NODE_TLS_REJECT_UNAUTHORIZED;
      delete env.NODE_EXTRA_CA_CERTS;
      if (mode === 'trusted') env.NODE_EXTRA_CA_CERTS = cert;
      if (env.MESH_IDE_NODE_BIN) env.ELECTRON_RUN_AS_NODE = '1';
      const result = spawnSync(env.MESH_IDE_NODE_BIN || process.execPath, [path.join(__dirname, 'https-fixture.cjs'), dir, mode], { env, encoding: 'utf8', timeout: 20000 });
      if (result.status !== 0) throw new Error(`HTTPS ${mode} fixture failed: ${result.error?.code || result.stderr}`);
      expect(result.stdout).toContain('PASS ' + mode);
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true }); // exact test-created directory and disposable keys only
  }
}, 60000);
