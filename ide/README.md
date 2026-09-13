# Mesh in VS Code

Open **Cmd+Shift+P → Mesh: Open** (Ctrl+Shift+P on Windows/Linux). Mesh opens as a reusable editor tab, like Stage. The Mesh status-bar item appears after editor startup and opens the same tab. Activation alone starts no viewer or network requests.

This first version includes the existing Graph, Search, Dashboard and Docs views. Click a graph node to read its note. Search runs only when you press Enter or click Search; graph filtering remains local. The extension adds no LLM or answer-generation service. Search still follows your existing Mesh embedding/reranking configuration.

## Local setup

The extension reuses an existing local `mesh ui` viewer. If none is running, either use **Mesh: Configure Viewer Startup** to select a local executable and existing vault and explicitly enable read-only startup, or start it in a terminal:

```sh
mesh ui ~/Corpus --own-index=false
```

Do **not** add `--own-index` when your sync/watch process owns the vault. Opt-in startup uses the selected absolute executable, literal arguments and `--own-index=false`, with inherited web-owner settings removed. It never starts an index owner, reindexes, installs binaries or mutates notes. The native configuration commands write only user-level extension settings. `Mesh: Refresh View` reconnects the viewer, not the index.

The default is `http://127.0.0.1:7474`; a root URL also tries `/app` when the root status endpoint returns 404. Use **Mesh: Set Local Viewer URL** for a different loopback port/base path. Process startup requires a root URL with an explicit nonzero port; existing viewers can still use base paths. Only user-level settings are used; workspace overrides cannot redirect requests or launch programs. This is a local desktop extension; remote/team authentication and browser-only VS Code are not supported.

While the tab is visible, readiness is checked every 30 seconds. Failures retry with bounded backoff, and startup is attempted only for a refused connection: at most three starts per ten minutes. Wrong-service or wrong-vault responses never trigger another process. The status tooltip reports viewer ownership and observed index freshness; legacy viewers explicitly show these as unknown. Automatically started viewers must confirm modern read-only status (Mesh v0.38.0 or later). No health polling occurs while hidden. Hiding stops a child still starting; an already-ready child is reused until settings change or the extension shuts down. External viewers are never stopped.

## Security and limits

The shipped viewer assets are bundled in the extension. A bounded host bridge allows only selected GET endpoints on an explicit HTTP loopback address. There is no iframe, remote executable content, credential handling, arbitrary fetch, Ask, review mutation or settings write. Note links cannot launch commands, files or external sites. Scripts are nonce-only; the graph's inline styles remain allowed. Hidden/closed views abort pending work and the singleton tab restores its selected section when shown again.

## Build and test

```sh
bun install --frozen-lockfile
bun run build
bun test test
bun run package
bun run ci
```

Install `release/mesh-workspace-0.2.2.vsix` with VS Code's **Extensions: Install from VSIX…** command. `media/source.json` records the bundled source revision, dirty state and asset hashes. Packaging includes the Mesh license. The server and editor extension are versioned independently; upgrading this viewer does not upgrade your installed Mesh binary.

Packaging uses the committed lockfile, fixed ZIP timestamps/permissions and sorted entries. It checks the complete archive allowlist, identity, source revision, asset hashes and shipped contents against the build inputs. A release build refuses uncommitted IDE/shared-viewer changes. `bun run ci` audits dependencies, runs the unit suite and requires two packages to be byte-identical. For local development only, `bun run ci --allow-dirty` or `bun run package --allow-dirty` produces an explicitly dirty, non-release artifact. Signing-tool postinstall scripts need not be enabled for this unsigned VSIX workflow.

The ignored `release/` directory also contains `manifest.json` and `SHA256SUMS`, identifying the exact version, source commit, byte count and archive hash. These are release artifacts, not an authenticity signature. Verify them against a trusted release source; do not trust a downloaded checksum from an unrelated source.

## Downloads and updates

Use **Mesh: Check for IDE Updates** to explicitly contact the public `bright-interaction/mesh` GitHub releases API. Activation, opening the Mesh tab and viewer polling do not check for updates. The command sends no vault content, workspace paths, credentials or telemetry. It reads at most 300 recent releases, selects a newer stable `ide-vX.Y.Z` release (ignoring core tags, drafts and prereleases), and checks its manifest and checksum list. A missing release, offline connection or rate limit does not interrupt the viewer. No update endpoint can be supplied by workspace settings.

After you approve **Download VSIX**, choose a new local filename. Mesh downloads at most 16 MiB from the fixed release URL, permitting only HTTPS redirects to GitHub's release-asset CDN. It checks the exact byte count and SHA-256 before saving with exclusive creation: existing files and symlinks are never overwritten. Network operations have 20-second deadlines and can be cancelled. Use **Extensions: Install from VSIX…** to install the saved file yourself. There is no automatic install, binary upgrade, viewer restart or marketplace integration. Older IDE versions without this command require a manual first download from the reviewed [public release page](https://github.com/bright-interaction/mesh/releases).

Checksums establish integrity relative to the official HTTPS release, not an independent publisher signature. A compromised release account remains a trust risk. No release is available through this command until its three reviewed assets have actually been published. A bounded search finding nothing is not proof that no older release exists outside the search window.

## Release handoff (operator)

The CI static-artifact gate includes IDE source, shared viewer assets and the Mesh license, with frozen installation and no deploy credentials. Confirm the exact source SHA's remote gate result before publication; local success alone is not a remote CI receipt.

1. Commit reviewed source and pass `bun run ci` plus the required remote checks. An IDE change gets a new independent version; never reuse a published version for different bytes.
2. Run `bun run release:plan`. It refuses dirty source, stale provenance, inconsistent manifests/checksums and archive contents that differ from the reviewed build. It prints a JSON handoff including exact `gh` arguments; it does not execute them, use credentials, create tags, upload or publish. There is no dirty-source bypass.
3. After publication approval, verify the public mirror's tree matches the reviewed monorepo `mesh/` subtree and create the independent `ide-vX.Y.Z` tag at that public source commit. The recorded monorepo source SHA is not necessarily the public split-repository SHA. Do not blindly tag public `main`.
4. Review and run the printed draft command from `mesh/ide`. GitHub CLI's [`--verify-tag`, `--draft` and `--latest=false`](https://cli.github.com/manual/gh_release_create) require the existing tag, stage reviewable assets, and keep the IDE release separate from core latest. The command never overwrites existing assets. A failed partial draft upload needs manual review, not a clobber retry.
5. Download the draft's VSIX, `manifest.json` and `SHA256SUMS` into a fresh directory and compare all three to the approved local bundle. After explicit approval, publish the reviewed draft with `latest=false`; confirm the public downloads and a disposable IDE install/update journey. Only then report delivery as live.

Public Mesh core tags and IDE versions remain independent. Keep core `vX.Y.Z` and its latest-release channel unchanged: the existing web/TUI banners are not an IDE delivery channel. Source merge, tag creation, draft upload, publication and normal-profile installation are separate operations; none is performed by packaging or CI.

`scripts/browser-smoke.mjs` checks fixture journeys in installed Chrome; set `MESH_PLAYWRIGHT_MODULE` to your Playwright module if needed. `test/editor.cjs` runs with VS Code's `--extensionDevelopmentPath` and `--extensionTestsPath` in an isolated `--user-data-dir`/`--extensions-dir` profile; set `MESH_IDE_TEST_URL` to your local test viewer. Optionally set `MESH_IDE_TEST_STARTUP_BINARY` to a built Mesh executable to test an isolated temporary vault, read-only child startup, exit/reconnect and disposal. This test changes only that disposable editor profile and deletes its own temporary vault after child shutdown. It writes a content-free receipt to ignored `test-results/editor-result.json`. Implementation follows the official [VS Code webview guidance](https://code.visualstudio.com/api/extension-guides/webview).
