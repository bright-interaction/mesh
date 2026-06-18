// api.js: the API view. Two references: the agent (MCP) tools, single-sourced from
// the server (GET /api/mcp-tools) with the retrieval contract + a paste-ready agent
// config; and the HTTP API, rendered from the OpenAPI spec (GET /openapi.json).
// Registers on Mesh.views.api. Vanilla, no Swagger CDN.
(function () {
  "use strict";
  const Mesh = (window.Mesh = window.Mesh || {});
  Mesh.views = Mesh.views || {};

  function esc(s) { return (s == null ? "" : String(s)).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])); }

  function toolCard(t) {
    const sc = t.inputSchema || {};
    const props = sc.properties || {};
    const req = (sc.required || []).reduce((m, k) => ((m[k] = true), m), {});
    const params = Object.keys(props).map((k) => {
      const p = props[k] || {};
      const ty = p.type === "array" ? (p.items && p.items.type ? p.items.type + "[]" : "array") : (p.type || "any");
      return '<span class="param"><code>' + esc(k) + '</code><span class="pty">' + esc(ty) + (req[k] ? " &middot; required" : "") + "</span></span>";
    }).join("");
    return (
      '<div class="tool">' +
      '<div class="tool-name"><code>' + esc(t.name) + "</code></div>" +
      '<p class="tool-desc">' + esc(t.description) + "</p>" +
      (params ? '<div class="tool-params">' + params + "</div>" : "") +
      "</div>"
    );
  }

  const METHOD_ORDER = ["get", "post", "put", "delete", "patch"];
  function endpointRows(spec) {
    const paths = spec.paths || {};
    return Object.keys(paths).sort().map((path) => {
      const ops = paths[path] || {};
      return METHOD_ORDER.filter((m) => ops[m]).map((m) => {
        const op = ops[m];
        const params = (op.parameters || []).map((p) => esc(p.name)).join(", ");
        return (
          '<div class="ep">' +
          '<span class="ep-m m-' + m + '">' + m.toUpperCase() + "</span>" +
          '<code class="ep-p">' + esc(path) + "</code>" +
          '<span class="ep-s">' + esc(op.summary || "") + (params ? ' <span class="ep-params">(' + params + ")</span>" : "") + "</span>" +
          "</div>"
        );
      }).join("");
    }).join("");
  }

  Mesh.views.api = async function (el, M) {
    el.innerHTML = '<div class="panel-inner"><p class="panel-h">Reference</p><h1 class="panel-title">API</h1><div class="panel-soon">Loading...</div></div>';
    let mcp, spec;
    try {
      [mcp, spec] = await Promise.all([M.api("/api/mcp-tools"), M.api("/openapi.json")]);
    } catch (e) {
      el.querySelector(".panel-soon").textContent = "Could not load the API reference: " + e.message;
      return;
    }
    const cfg = JSON.stringify({ mcpServers: { "mesh-" + (mcp.vault || "vault").split("/").pop(): mcp.config } }, null, 2);
    const inner = document.createElement("div");
    inner.className = "panel-inner";
    inner.innerHTML =
      '<p class="panel-h">Reference</p>' +
      '<h1 class="panel-title">API</h1>' +
      '<section class="set-group"><h2 class="set-h">Agent (MCP) tools</h2>' +
      '<p class="api-lead">Point your coding agent at the MCP server, then retrieve cheaply with these tools.</p>' +
      '<div class="api-config"><div class="api-config-bar"><span>agent config</span><button class="btn ghost" id="copy-cfg">copy</button></div><pre>' + esc(cfg) + "</pre></div>" +
      '<details class="api-contract"><summary>Retrieval contract</summary><pre>' + esc(mcp.contract || "") + "</pre></details>" +
      '<div class="tools">' + (mcp.tools || []).map(toolCard).join("") + "</div>" +
      "</section>" +
      '<section class="set-group"><h2 class="set-h">HTTP API</h2>' +
      '<p class="api-lead">The local viewer API. <a href="/openapi.json" target="_blank" rel="noopener">openapi.json</a>. Loopback needs no auth; a non-loopback bind requires a bearer token.</p>' +
      '<div class="eps">' + endpointRows(spec) + "</div></section>";
    el.replaceChildren(inner);

    inner.querySelector("#copy-cfg").addEventListener("click", () => {
      navigator.clipboard.writeText(cfg).then(
        () => Mesh.toast && Mesh.toast("Config copied", "ok"),
        () => Mesh.toast && Mesh.toast("Copy failed", "err")
      );
    });
  };
})();
