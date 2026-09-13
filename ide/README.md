# Mesh in VS Code

Open **Cmd+Shift+P → Mesh: Open** (Ctrl+Shift+P on Windows/Linux). Mesh opens as a reusable editor tab, like Stage. After first activation, the Mesh status-bar item opens the same tab.

This first version includes the existing Graph, Search, Dashboard and Docs views. Click a graph node to read its note. Search runs only when you press Enter or click Search; graph filtering remains local. The extension adds no LLM or answer-generation service. Search still follows your existing Mesh embedding/reranking configuration.

## Local setup

The extension connects to an existing local `mesh ui` viewer. If none is running, start it in a terminal:

```sh
mesh ui ~/Corpus
```

Do **not** add `--own-index` when your sync/watch process owns the vault. The extension never starts processes, reindexes, installs binaries or mutates notes/settings. `Mesh: Refresh View` reloads the viewer, not the index.

The default is `http://127.0.0.1:7474`; a root URL also tries `/app` when the root status endpoint returns 404. Use **Mesh: Set Local Viewer URL** for a different loopback port/base path. Only the user-level setting is used; workspace overrides cannot redirect requests. This is a local desktop extension; remote/team authentication and browser-only VS Code are not supported in v0.1.

## Security and limits

The shipped viewer assets are bundled in the extension. A bounded host bridge allows only selected GET endpoints on an explicit HTTP loopback address. There is no iframe, remote executable content, credential handling, arbitrary fetch, Ask, review mutation or settings write. Note links cannot launch commands, files or external sites. Scripts are nonce-only; the graph's inline styles remain allowed. Hidden/closed views abort pending work and the singleton tab restores its selected section when shown again.

## Build and test

```sh
bun run build
bun test test
bun run package
```

Install `mesh-workspace-0.1.0.vsix` with VS Code's **Extensions: Install from VSIX…** command. `media/source.json` records the bundled source revision, dirty state and asset hashes. Packaging includes the Mesh license. The server and editor extension are versioned independently; upgrading this viewer does not upgrade your installed Mesh binary.

`scripts/browser-smoke.mjs` checks fixture journeys in installed Chrome; set `MESH_PLAYWRIGHT_MODULE` to your Playwright module if needed. `test/editor.cjs` runs with VS Code's `--extensionDevelopmentPath` and `--extensionTestsPath` in an isolated `--user-data-dir`/`--extensions-dir` profile; set `MESH_IDE_TEST_URL` to your local test viewer. It writes a content-free receipt to ignored `test-results/editor-result.json`. Implementation follows the official [VS Code webview guidance](https://code.visualstudio.com/api/extension-guides/webview).
