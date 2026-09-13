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
bun run build
bun test test
bun run package
```

Install `mesh-workspace-0.2.0.vsix` with VS Code's **Extensions: Install from VSIX…** command. `media/source.json` records the bundled source revision, dirty state and asset hashes. Packaging includes the Mesh license. The server and editor extension are versioned independently; upgrading this viewer does not upgrade your installed Mesh binary. Marketplace publication and an extension release/update channel remain separate work.

`scripts/browser-smoke.mjs` checks fixture journeys in installed Chrome; set `MESH_PLAYWRIGHT_MODULE` to your Playwright module if needed. `test/editor.cjs` runs with VS Code's `--extensionDevelopmentPath` and `--extensionTestsPath` in an isolated `--user-data-dir`/`--extensions-dir` profile; set `MESH_IDE_TEST_URL` to your local test viewer. Optionally set `MESH_IDE_TEST_STARTUP_BINARY` to a built Mesh executable to test an isolated temporary vault, read-only child startup, exit/reconnect and disposal. This test changes only that disposable editor profile and deletes its own temporary vault after child shutdown. It writes a content-free receipt to ignored `test-results/editor-result.json`. Implementation follows the official [VS Code webview guidance](https://code.visualstudio.com/api/extension-guides/webview).
