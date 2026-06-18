// shell.js: the app-shell router. Owns the left-nav sections (Graph / Search /
// Settings / Docs / API), the auth-aware /api fetch helper, and the rail status.
// The Graph section is driven by app.js (the canvas engine); the other sections are
// rendered by per-view modules that register on window.Mesh.views (filled in by
// settings.js / search.js / docs.js / api.js as those phases land). Vanilla, no deps.
(function () {
  "use strict";
  const TOKEN_KEY = "mesh_ui_token";
  const Mesh = (window.Mesh = window.Mesh || {});
  Mesh.views = Mesh.views || {};

  // api: fetch /api/* with the bearer token when one is set. On 401 it prompts once
  // for a token (non-loopback binds require it), stores it, and retries.
  Mesh.api = async function (path, opts) {
    opts = opts || {};
    const headers = Object.assign({}, opts.headers);
    const tok = sessionStorage.getItem(TOKEN_KEY);
    if (tok) headers["Authorization"] = "Bearer " + tok;
    let res = await fetch(path, Object.assign({}, opts, { headers }));
    if (res.status === 401) {
      const entered = window.prompt("This Mesh viewer requires an access token:");
      if (entered) {
        sessionStorage.setItem(TOKEN_KEY, entered.trim());
        headers["Authorization"] = "Bearer " + entered.trim();
        res = await fetch(path, Object.assign({}, opts, { headers }));
      }
    }
    if (!res.ok) throw new Error(path + ": " + res.status);
    const ct = res.headers.get("content-type") || "";
    return ct.includes("application/json") ? res.json() : res.text();
  };

  const panel = document.getElementById("panel");
  const overlay = document.getElementById("overlay");
  const navs = Array.from(document.querySelectorAll(".rail-nav .nav"));
  const panelViews = Array.from(document.querySelectorAll(".panel-view"));

  function panelEl(view) {
    return panelViews.find((p) => p.dataset.panel === view);
  }

  // placeholder for views whose module has not loaded yet (future phases).
  function placeholder(el, title) {
    el.innerHTML = "";
    const inner = document.createElement("div");
    inner.className = "panel-inner";
    inner.innerHTML =
      '<p class="panel-h">' + title + '</p>' +
      '<h1 class="panel-title">' + title + '</h1>' +
      '<div class="panel-soon">This section is part of the web app build and lands in an upcoming phase. ' +
      'The shell, navigation, and the live graph are in place.</div>';
    el.appendChild(inner);
  }

  let current = "graph";
  function route(view) {
    if (!view) view = "graph";
    current = view;
    navs.forEach((b) => {
      const on = b.dataset.view === view;
      b.classList.toggle("active", on);
      if (on) b.setAttribute("aria-current", "page");
      else b.removeAttribute("aria-current");
    });
    if (view === "graph") {
      document.body.classList.remove("panel-active");
      panel.hidden = true;
      panelViews.forEach((p) => p.classList.remove("active"));
      return;
    }
    // a feature section: cover the canvas with its panel.
    document.body.classList.add("panel-active");
    if (overlay) overlay.classList.add("hidden"); // the graph overlay is irrelevant here
    panel.hidden = false;
    panelViews.forEach((p) => p.classList.toggle("active", p.dataset.panel === view));
    const el = panelEl(view);
    if (!el) return;
    const render = Mesh.views[view];
    if (typeof render === "function") {
      try { render(el, Mesh); } catch (e) { el.innerHTML = '<div class="panel-inner"><div class="panel-soon">Failed to render: ' + e.message + "</div></div>"; }
    } else {
      placeholder(el, view.charAt(0).toUpperCase() + view.slice(1));
    }
  }
  Mesh.route = route;

  navs.forEach((b) =>
    b.addEventListener("click", () => {
      const v = b.dataset.view;
      location.hash = v === "graph" ? "" : "#/" + v;
      route(v);
    })
  );
  window.addEventListener("hashchange", () => route(hashView()));
  function hashView() {
    const m = (location.hash || "").match(/^#\/(\w+)/);
    return m ? m[1] : "graph";
  }

  // live status in the rail foot.
  async function loadStatus() {
    const foot = document.getElementById("rail-status");
    if (!foot) return;
    try {
      const s = await Mesh.api("/api/status");
      Mesh.status = s;
      const c = s.counts || {};
      const sig = s.signals || {};
      const dot = (k) => '<span class="sig ' + (sig[k] ? "on" : "") + '" title="' + k + (sig[k] ? " on" : " off") + '"></span>';
      foot.innerHTML =
        (c.notes || 0) + " notes &middot; " + (c.edges || 0) + " links<br>" +
        dot("fts") + dot("graph") + dot("vector") + dot("rerank") + dot("ann");
    } catch (e) {
      foot.textContent = "";
    }
  }
  Mesh.refreshStatus = loadStatus;

  route(hashView());
  loadStatus();
})();
