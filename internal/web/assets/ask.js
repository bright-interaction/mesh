// ask.js: the Ask view. A natural-language question answered from the team's own notes
// + code (BYOAI, grounded with citations). The conversational second brain. POSTs
// /api/ask. Registers Mesh.views.ask. Vanilla, no deps.
(function () {
  "use strict";
  const Mesh = (window.Mesh = window.Mesh || {});
  Mesh.views = Mesh.views || {};

  function esc(s) { return (s == null ? "" : String(s)).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])); }

  Mesh.views.ask = function (el, M) {
    const inner = document.createElement("div");
    inner.className = "panel-inner";
    inner.innerHTML =
      '<p class="panel-h">Second brain</p>' +
      '<h1 class="panel-title">Ask</h1>' +
      '<p class="panel-lead">Ask a question in plain language. The answer is grounded in your team\'s notes and code, with citations, never invented.</p>' +
      '<form id="ask-form" style="display:flex;gap:.6rem;margin:.5rem 0 1rem">' +
      '<input id="ask-q" type="text" placeholder="e.g. how do we authenticate Mollie webhooks?" autocomplete="off" ' +
      'style="flex:1;background:#0e0c13;border:1px solid var(--hair,#21262d);border-radius:8px;color:#e6edf3;padding:.6rem .8rem;font:14px ui-sans-serif,system-ui">' +
      '<button type="submit" style="background:#238636;color:#fff;border:0;border-radius:8px;padding:.6rem 1.1rem;font-weight:600;cursor:pointer">Ask</button>' +
      '</form><div id="ask-body"></div>';
    el.replaceChildren(inner);
    const body = inner.querySelector("#ask-body");
    const input = inner.querySelector("#ask-q");
    const form = inner.querySelector("#ask-form");

    form.addEventListener("submit", function (e) {
      e.preventDefault();
      const q = input.value.trim();
      if (!q) return;
      body.innerHTML = '<p class="srch-hint">Thinking (reading your notes + code)...</p>';
      M.api("/api/ask", { method: "POST", body: JSON.stringify({ question: q }) })
        .then(function (d) {
          const answer = '<div class="prose" style="white-space:pre-wrap;line-height:1.6">' + esc(d.answer || "") + "</div>";
          let cites = "";
          if ((d.citations || []).length) {
            cites = '<h2 class="panel-h" style="margin-top:1.4rem">Sources</h2>' +
              (d.citations || []).map(function (c) {
                const open = c.kind === "note" && c.id
                  ? ' style="cursor:pointer;color:#58a6ff" data-note="' + esc(c.id) + '"'
                  : "";
                return '<div class="ask-cite"' + open + ' style="font-size:.85rem;border-top:1px solid #21262d;padding:.35rem 0">' +
                  '[' + c.n + '] <span style="color:#7d8590">' + esc(c.kind) + '</span> ' + esc(c.title || c.id) +
                  ' <span style="color:#6e7681">' + esc(c.loc || "") + "</span></div>";
              }).join("");
          }
          body.innerHTML = answer + cites;
          body.querySelectorAll(".ask-cite[data-note]").forEach(function (n) {
            n.addEventListener("click", function () { Mesh.openNote && Mesh.openNote(n.dataset.note); });
          });
        })
        .catch(function (err) { body.innerHTML = '<p class="srch-hint">Could not answer: ' + esc(err.message) + "</p>"; });
    });
    input.focus();
  };
})();
