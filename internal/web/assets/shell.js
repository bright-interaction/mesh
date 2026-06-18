// shell.js: the app-shell router. Owns the left-nav sections (Graph / Search /
// Settings / Docs / API), the auth-aware /api fetch helper, and the rail status.
// The Graph section is driven by app.js (the canvas engine); the other sections are
// rendered by per-view modules that register on window.Mesh.views (filled in by
// settings.js / search.js / docs.js / api.js as those phases land). Vanilla, no deps.
(function () {
  "use strict";
  const Mesh = (window.Mesh = window.Mesh || {});
  Mesh.views = Mesh.views || {};

  // api: fetch /api/* relying on the HttpOnly session cookie (set once via the login
  // card / magic link). No token is ever held in JS. On 401 it reveals the login
  // card so the user can re-authenticate.
  Mesh.api = async function (path, opts) {
    opts = opts || {};
    // Resolve relative to <base href> so the app works under a path (e.g. /app/).
    // Callers pass "/api/...": strip the leading slash so it is relative.
    path = path.replace(/^\//, "");
    const res = await fetch(path, Object.assign({ credentials: "same-origin" }, opts));
    if (res.status === 401) {
      showLogin();
      throw new Error(path + ": 401");
    }
    if (!res.ok) throw new Error(path + ": " + res.status);
    const ct = res.headers.get("content-type") || "";
    return ct.includes("application/json") ? res.json() : res.text();
  };

  // --- Login (HttpOnly cookie session) ----------------------------------------
  // POST the access key; the server validates it constant-time and sets the cookie.
  async function submitKey(key) {
    const res = await fetch("api/login", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ key: key }),
    });
    return res.ok;
  }

  function showLogin() {
    const el = document.getElementById("login");
    if (!el || !el.classList.contains("hidden")) return;
    el.classList.remove("hidden");
    el.setAttribute("aria-hidden", "false");
    const inp = document.getElementById("login-key");
    if (inp) setTimeout(() => inp.focus(), 60);
  }
  Mesh.showLogin = showLogin;

  function wireLogin() {
    const form = document.getElementById("login-form");
    if (!form) return;
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const inp = document.getElementById("login-key");
      const err = document.getElementById("login-err");
      const btn = document.getElementById("login-btn");
      const key = ((inp && inp.value) || "").trim();
      if (!key) return;
      if (err) err.hidden = true;
      if (btn) { btn.disabled = true; btn.textContent = "Checking..."; }
      let ok = false;
      try { ok = await submitKey(key); } catch (_) { ok = false; }
      if (ok) {
        // Cookie is set: reload so every view loads authenticated, cleanly.
        location.reload();
        return;
      }
      if (btn) { btn.disabled = false; btn.textContent = "Unlock"; }
      if (err) { err.hidden = false; err.textContent = "That key was not accepted. Check it and try again."; }
      if (inp) inp.select();
    });
  }

  // A magic link (mesh.../app/#k=<key>) signs you in with one click. The key lives in
  // the URL fragment (never sent to the server, so it is not in access logs) and is
  // stripped from the address bar immediately, before the network call.
  async function consumeMagicLink() {
    const m = (location.hash || "").match(/[#&]k=([^&]+)/);
    if (!m) return false;
    const key = decodeURIComponent(m[1]);
    history.replaceState(null, "", location.pathname + location.search);
    let ok = false;
    try { ok = await submitKey(key); } catch (_) { ok = false; }
    if (ok) { location.reload(); return true; }
    showLogin();
    return false;
  }

  const panel = document.getElementById("panel");
  const overlay = document.getElementById("overlay");
  const navs = Array.from(document.querySelectorAll(".rail-nav .nav"));
  const panelViews = Array.from(document.querySelectorAll(".panel-view"));

  function panelEl(view) {
    return panelViews.find((p) => p.dataset.panel === view);
  }

  // Proper display labels (so a fallback never mangles an acronym like "API" -> "Api").
  const LABELS = { graph: "Graph", search: "Search", settings: "Settings", docs: "Docs", api: "API" };
  function labelFor(view) { return LABELS[view] || (view.charAt(0).toUpperCase() + view.slice(1)); }

  // placeholder for a view whose module did not load (e.g. a stale cache). It tells
  // the user how to recover rather than implying the feature is unfinished.
  function placeholder(el, view) {
    const title = labelFor(view);
    el.innerHTML = "";
    const inner = document.createElement("div");
    inner.className = "panel-inner";
    inner.innerHTML =
      '<p class="panel-h">' + title + '</p>' +
      '<h1 class="panel-title">' + title + '</h1>' +
      '<div class="panel-soon">This view could not load its script (often a stale cache). ' +
      'Try a hard refresh (Cmd-Shift-R).</div>';
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
      placeholder(el, view);
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
  // "search ->" beside the graph filter jumps to the full-text Search view.
  const toSearch = document.getElementById("to-search");
  if (toSearch) toSearch.addEventListener("click", () => { location.hash = "#/search"; route("search"); });

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
      // Surface the empty-state when the vault has no real notes yet (a fresh hub
      // carries a single seed index.md).
      const empty = document.getElementById("empty");
      if (empty) empty.classList.toggle("hidden", (c.notes || 0) > 1);
    } catch (e) {
      foot.textContent = "";
    }
  }
  Mesh.refreshStatus = loadStatus;

  const emptyDocs = document.getElementById("empty-docs");
  if (emptyDocs) emptyDocs.addEventListener("click", () => { location.hash = "#/docs"; route("docs"); });

  wireLogin();
  (async function init() {
    if (await consumeMagicLink()) return; // a successful magic link reloads the page
    route(hashView());
    loadStatus();
  })();
})();
