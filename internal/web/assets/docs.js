// docs.js: the Docs view. Lists the embedded doc pages (GET /api/docs) in a side
// nav and renders the selected page's server-rendered HTML (GET /api/docs/{slug}).
// The HTML is produced from trusted, compiled-in markdown, so it is injected as-is.
// Registers on Mesh.views.docs.
(function () {
  "use strict";
  const Mesh = (window.Mesh = window.Mesh || {});
  Mesh.views = Mesh.views || {};

  function esc(s) { return (s == null ? "" : String(s)).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])); }

  Mesh.views.docs = async function (el, M) {
    el.innerHTML = '<div class="panel-inner"><p class="panel-h">Reference</p><h1 class="panel-title">Docs</h1><div class="panel-soon">Loading...</div></div>';
    let pages;
    try {
      const d = await M.api("/api/docs");
      pages = d.pages || [];
    } catch (e) {
      el.querySelector(".panel-soon").textContent = "Could not load docs: " + e.message;
      return;
    }
    if (!pages.length) {
      el.querySelector(".panel-soon").textContent = "No docs found.";
      return;
    }
    const inner = document.createElement("div");
    inner.className = "panel-inner docs-layout";
    inner.innerHTML =
      '<nav class="docs-nav"><p class="panel-h">Docs</p>' +
      pages.map((p) => '<button class="docs-link" data-slug="' + esc(p.slug) + '">' + esc(p.title) + "</button>").join("") +
      "</nav>" +
      '<article class="docs-content prose"></article>';
    el.replaceChildren(inner);
    const content = inner.querySelector(".docs-content");
    const links = Array.from(inner.querySelectorAll(".docs-link"));

    async function open(slug) {
      links.forEach((l) => l.classList.toggle("active", l.dataset.slug === slug));
      content.innerHTML = '<p class="srch-hint">Loading...</p>';
      try {
        const d = await M.api("/api/docs/" + encodeURIComponent(slug));
        content.innerHTML = d.html;
        content.scrollTop = 0;
      } catch (e) {
        content.innerHTML = '<p class="srch-hint">Could not load: ' + esc(e.message) + "</p>";
      }
    }
    links.forEach((l) => l.addEventListener("click", () => open(l.dataset.slug)));
    open(pages[0].slug);
  };
})();
