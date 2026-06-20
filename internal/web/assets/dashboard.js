// dashboard.js: the Dashboard view. The ROI + knowledge-health snapshot a champion
// shows their boss: usage, estimated tokens saved vs naive RAG, coverage, the most
// reused notes, a contributor leaderboard, and lifecycle health. Reads
// GET /api/dashboard. Registers on Mesh.views.dashboard. Vanilla, no deps.
(function () {
  "use strict";
  const Mesh = (window.Mesh = window.Mesh || {});
  Mesh.views = Mesh.views || {};

  function esc(s) { return (s == null ? "" : String(s)).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])); }
  function n(x) { return (x || 0).toLocaleString("en-US"); }

  function stat(label, value, sub) {
    return (
      '<div style="border-top:1px solid var(--hair,#21262d);padding:.9rem 0">' +
      '<div style="font:600 1.8rem/1 ui-monospace,monospace;letter-spacing:-.02em">' + esc(value) + "</div>" +
      '<div style="font-size:.78rem;text-transform:uppercase;letter-spacing:.06em;color:#7d8590;margin-top:.35rem">' + esc(label) + "</div>" +
      (sub ? '<div style="font-size:.8rem;color:#6e7681;margin-top:.2rem">' + esc(sub) + "</div>" : "") +
      "</div>"
    );
  }

  function bars(title, obj, color) {
    const entries = Object.entries(obj || {}).sort((a, b) => b[1] - a[1]);
    if (!entries.length) return "";
    const max = Math.max.apply(null, entries.map((e) => e[1]));
    const rows = entries.map(function (e) {
      const pct = max ? Math.round((e[1] / max) * 100) : 0;
      return (
        '<div style="display:flex;align-items:center;gap:.6rem;margin:.3rem 0">' +
        '<div style="width:9rem;font-size:.85rem;color:#9da7b3;text-align:right">' + esc(e[0]) + "</div>" +
        '<div style="flex:1;height:8px;background:#21262d;border-radius:5px;overflow:hidden"><i style="display:block;height:100%;width:' + pct + '%;background:' + color + '"></i></div>' +
        '<div style="width:3rem;font:.85rem ui-monospace,monospace;color:#e6edf3">' + n(e[1]) + "</div></div>"
      );
    }).join("");
    return '<h2 class="panel-h" style="margin-top:1.6rem">' + esc(title) + "</h2>" + rows;
  }

  Mesh.views.dashboard = function (el, M) {
    const inner = document.createElement("div");
    inner.className = "panel-inner";
    inner.innerHTML =
      '<p class="panel-h">Value</p>' +
      '<h1 class="panel-title">Dashboard</h1>' +
      '<p class="panel-lead">What your team\'s second brain is doing: usage, the tokens it saves your agents versus dumping whole files, what knowledge gets reused, who contributes, and what needs tending.</p>' +
      '<div id="dash-body"><p class="srch-hint">Loading...</p></div>';
    el.replaceChildren(inner);
    const body = inner.querySelector("#dash-body");

    M.api("/api/dashboard").then(function (d) {
      const u = d.usage || {};
      const h = d.health || {};
      const savedM = (d.est_tokens_saved || 0) / 1e6;
      const grid =
        '<div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(8rem,1fr));gap:0 1.6rem">' +
        stat("queries served", n(u.queries)) +
        stat("notes fetched", n(u.fetches)) +
        stat("notes written back", n(u.writes)) +
        stat("notes in vault", n(u.notes)) +
        "</div>";

      const saved =
        '<div style="border:1px solid #1f6f3a;background:#0f1f15;border-radius:12px;padding:1rem 1.1rem;margin-top:1.4rem">' +
        '<div style="font:600 1.6rem/1 ui-monospace,monospace;color:#3fb950">~' + (savedM >= 1 ? savedM.toFixed(2) + "M" : n(d.est_tokens_saved)) + " tokens</div>" +
        '<div style="font-size:.82rem;color:#9da7b3;margin-top:.3rem">estimated saved vs naive whole-file RAG (~' + n(d.tokens_saved_per_query) + ' per query, from the benchmark). Estimate.</div></div>';

      const topFetched = (d.top_fetched || []).length
        ? '<h2 class="panel-h" style="margin-top:1.6rem">Most reused notes</h2>' +
          (d.top_fetched || []).map(function (t) {
            return '<div style="display:flex;justify-content:space-between;border-top:1px solid #21262d;padding:.4rem 0;font-size:.88rem"><span style="color:#9da7b3">' + esc(t.note_id) + '</span><span style="font:.85rem ui-monospace,monospace">' + n(t.count) + "x</span></div>";
          }).join("")
        : "";

      const contrib = (d.contributors || []).length ? bars("Contributors", Object.fromEntries((d.contributors || []).map((c) => [c.name, c.count])), "#388bfd") : "";

      const healthTotal = (h.dead_ref || 0) + (h.overdue || 0) + (h.contradiction || 0);
      const health =
        '<h2 class="panel-h" style="margin-top:1.6rem">Knowledge health</h2>' +
        (healthTotal === 0
          ? '<p style="color:#3fb950;font-size:.9rem">Healthy: no dead refs, overdue reviews, or contradictions.</p>'
          : '<div style="display:flex;gap:1.4rem;font-size:.9rem">' +
            '<span style="color:#f85149">' + n(h.dead_ref || 0) + " dead refs</span>" +
            '<span style="color:#d29922">' + n(h.overdue || 0) + " overdue</span>" +
            '<span style="color:#db61a2">' + n(h.contradiction || 0) + " contradictions</span></div>" +
            '<p style="font-size:.8rem;color:#6e7681;margin-top:.3rem">Run <code>mesh health</code> or the mesh_health tool for details.</p>');

      body.innerHTML = grid + saved + bars("Coverage by type", d.coverage, "#2ea043") + topFetched + contrib + health;
    }).catch(function (e) {
      body.innerHTML = '<p class="srch-hint">Could not load dashboard: ' + esc(e.message) + "</p>";
    });
  };
})();
