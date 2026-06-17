// mesh ui: a sovereign canvas graph viewer. No dependencies. Two views over one
// graph: an Obsidian-style force layout and a galaxy orbiting the index note.
(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const canvas = $("stage"), ctx = canvas.getContext("2d");
  const overlay = $("overlay"), overlayMsg = $("overlay-msg");
  const TAU = Math.PI * 2;
  const HEX = /^#[0-9a-fA-F]{3,8}$/;

  let G = null;
  const byId = new Map();
  const nodeIndex = new Map(); // id -> array index, built once
  const commColor = new Map();
  let sp = [];                 // reused screen-position buffer (no per-frame alloc)
  let view = "graph";
  const cam = { x: 0, y: 0, zoom: 1 };
  let hover = null, selected = null, query = "";
  let dpr = Math.max(1, window.devicePixelRatio || 1);
  let W = 0, H = 0;
  let galaxyAngle = 0;
  let neighborSet = null; // ids adjacent to hover/selection, for emphasis
  let dirty = true;       // redraw gate: static graph view idles, galaxy animates
  const markDirty = () => { dirty = true; };
  const safeColor = (c) => (HEX.test(c) ? c : "#7c766e");

  // ---- boot ----
  fetch("/graph.json").then((r) => {
    if (!r.ok) throw new Error("graph.json " + r.status);
    return r.json();
  }).then(boot).catch(fail);

  function boot(data) {
    G = data;
    resize();
    if (!G.nodes || G.nodes.length === 0) return showEmpty();
    for (const c of G.communities) { c.color = safeColor(c.color); commColor.set(c.id, c.color); }
    G.nodes.forEach((n, i) => { byId.set(n.id, n); nodeIndex.set(n.id, i); });
    sp = new Array(G.nodes.length);
    buildAdjacency();
    setStats();
    buildLegend();
    layoutGalaxy();
    layoutGraph(() => { fitView(); doneOverlay(); markDirty(); requestAnimationFrame(loop); });
    wire();
  }

  // ---- adjacency (for emphasis + force springs) ----
  const adj = new Map();
  function buildAdjacency() {
    for (const n of G.nodes) adj.set(n.id, []);
    for (const e of G.edges) {
      if (e.source === e.target) continue;
      if (adj.has(e.source) && adj.has(e.target)) {
        adj.get(e.source).push(e.target);
        adj.get(e.target).push(e.source);
      }
    }
  }

  // ---- galaxy layout: deterministic radial by orbit, communities as arms ----
  function layoutGalaxy() {
    const ringGap = 130;
    const byOrbit = new Map();
    for (const n of G.nodes) {
      const o = n.orbit || 0;
      if (!byOrbit.has(o)) byOrbit.set(o, []);
      byOrbit.get(o).push(n);
    }
    for (const [o, ring] of byOrbit) {
      ring.sort((a, b) => a.community - b.community || (a.id < b.id ? -1 : 1));
      const r = o * ringGap;
      ring.forEach((n, i) => {
        n.theta0 = ring.length <= 1 ? 0 : (i / ring.length) * TAU;
        n.radius0 = r;
        n.speed = 1 / Math.sqrt(o + 1); // inner rings revolve faster
      });
    }
  }
  function galaxyPos(n) {
    const r0 = n.radius0 || 0;
    if (r0 === 0) return { x: 0, y: 0 };
    const a = (n.theta0 || 0) + galaxyAngle * (n.speed || 0);
    return { x: Math.cos(a) * r0, y: Math.sin(a) * r0 };
  }

  // ---- force layout (Fruchterman-Reingold, grid repulsion, chunked) ----
  function layoutGraph(done) {
    const n = G.nodes.length;
    const cached = loadCachedLayout();
    if (cached) {
      for (const node of G.nodes) {
        const p = cached[node.id];
        if (p && Number.isFinite(p[0]) && Number.isFinite(p[1])) { node.gx = p[0]; node.gy = p[1]; }
      }
      if (G.nodes.every((nd) => Number.isFinite(nd.gx))) return done();
    }
    const k = Math.sqrt((Math.max(1, n) * 16000) / Math.max(1, n));
    const commIndex = new Map();
    G.communities.forEach((c, i) => commIndex.set(c.id, i));
    G.nodes.forEach((nd, i) => {
      const ci = commIndex.get(nd.community) || 0;
      const base = (ci / Math.max(1, G.communities.length)) * TAU;
      const rr = k * (1 + (i % 17) * 0.18);
      nd.gx = Math.cos(base + (i % 7) * 0.21) * rr;
      nd.gy = Math.sin(base + (i % 7) * 0.21) * rr;
    });
    const iters = n > 1200 ? 90 : n > 400 ? 140 : 200;
    let t = k * 1.6;
    const cool = t / (iters + 1);
    let it = 0;
    const stepBatch = () => {
      const batch = Math.min(8, iters - it);
      for (let b = 0; b < batch; b++) { frIteration(k, t); t -= cool; }
      it += batch;
      overlayMsg.textContent = "building the graph " + Math.round((it / iters) * 100) + "%";
      if (it < iters) return setTimeout(stepBatch, 0);
      saveCachedLayout();
      done();
    };
    stepBatch();
  }

  function frIteration(k, temp) {
    const nodes = G.nodes;
    for (const v of nodes) { v.dx = 0; v.dy = 0; }
    const cell = k, grid = new Map();
    for (const v of nodes) {
      const kk = ((v.gx / cell) | 0) + ":" + ((v.gy / cell) | 0);
      if (!grid.has(kk)) grid.set(kk, []);
      grid.get(kk).push(v);
    }
    for (const v of nodes) {
      const cx = (v.gx / cell) | 0, cy = (v.gy / cell) | 0;
      for (let gx = cx - 1; gx <= cx + 1; gx++)
        for (let gy = cy - 1; gy <= cy + 1; gy++) {
          const bucket = grid.get(gx + ":" + gy);
          if (!bucket) continue;
          for (const u of bucket) {
            if (u === v) continue;
            let ddx = v.gx - u.gx, ddy = v.gy - u.gy;
            let d2 = ddx * ddx + ddy * ddy;
            if (d2 < 0.01) { ddx = Math.random() - 0.5; ddy = Math.random() - 0.5; d2 = 0.01; }
            const d = Math.sqrt(d2), rep = (k * k) / d;
            v.dx += (ddx / d) * rep; v.dy += (ddy / d) * rep;
          }
        }
    }
    for (const e of G.edges) {
      const a = byId.get(e.source), b = byId.get(e.target);
      if (!a || !b || a === b) continue;
      let ddx = a.gx - b.gx, ddy = a.gy - b.gy;
      const d = Math.sqrt(ddx * ddx + ddy * ddy) || 0.01;
      const att = (d * d) / k, fx = (ddx / d) * att, fy = (ddy / d) * att;
      a.dx -= fx; a.dy -= fy; b.dx += fx; b.dy += fy;
    }
    for (const v of nodes) { v.dx -= v.gx * 0.012; v.dy -= v.gy * 0.012; }
    for (const v of nodes) {
      const d = Math.sqrt(v.dx * v.dx + v.dy * v.dy) || 0.01;
      v.gx += (v.dx / d) * Math.min(d, temp);
      v.gy += (v.dy / d) * Math.min(d, temp);
    }
  }

  function layoutSig() { return G.meta.node_count + "x" + G.meta.edge_count + "@" + G.meta.index_id; }
  function loadCachedLayout() {
    try { const j = localStorage.getItem("mesh-layout:" + layoutSig()); return j ? JSON.parse(j) : null; } catch { return null; }
  }
  function saveCachedLayout() {
    try {
      const o = {};
      for (const n of G.nodes) o[n.id] = [Math.round(n.gx), Math.round(n.gy)];
      localStorage.setItem("mesh-layout:" + layoutSig(), JSON.stringify(o));
    } catch { /* private mode / quota: layout just recomputes next time */ }
  }

  // ---- positions ----
  function pos(n) { return view === "galaxy" ? galaxyPos(n) : { x: n.gx, y: n.gy }; }
  function toScreen(p) { return { x: (p.x - cam.x) * cam.zoom + W / 2, y: (p.y - cam.y) * cam.zoom + H / 2 }; }
  function radius(n) { return Math.max(2.2, (n.size || 1) * 2.4); }
  function nodeScale() { return Math.max(0.7, Math.min(cam.zoom, 2.2)); }

  // ---- render loop ----
  function loop() {
    if (view === "galaxy") { galaxyAngle += 0.0016; dirty = true; }
    if (dirty) { draw(); dirty = false; }
    requestAnimationFrame(loop);
  }

  function draw() {
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, W, H);
    const z = cam.zoom, ox = W / 2 - cam.x * z, oy = H / 2 - cam.y * z;
    const nodes = G.nodes, N = nodes.length;
    for (let i = 0; i < N; i++) {
      const p = pos(nodes[i]);
      let s = sp[i]; if (!s) { s = sp[i] = { x: 0, y: 0 }; }
      s.x = p.x * z + ox; s.y = p.y * z + oy;
    }
    const onScreen = (s) => s.x >= -60 && s.x <= W + 60 && s.y >= -60 && s.y <= H + 60;

    // faint edges batched into one stroke; emphasized edges brighter, separately
    ctx.lineWidth = Math.max(0.4, 0.6 * z);
    ctx.strokeStyle = "rgba(245,242,236,0.06)";
    ctx.beginPath();
    const emph = [];
    for (const e of G.edges) {
      const ai = nodeIndex.get(e.source), bi = nodeIndex.get(e.target);
      if (ai === undefined || bi === undefined) continue;
      const a = sp[ai], b = sp[bi];
      if (!onScreen(a) && !onScreen(b)) continue;
      if (neighborSet && (isFocus(e.source) || isFocus(e.target))) { emph.push(a, b); continue; }
      ctx.moveTo(a.x, a.y); ctx.lineTo(b.x, b.y);
    }
    ctx.stroke();
    if (emph.length) {
      ctx.strokeStyle = "rgba(245,242,236,0.35)";
      ctx.beginPath();
      for (let i = 0; i < emph.length; i += 2) { ctx.moveTo(emph[i].x, emph[i].y); ctx.lineTo(emph[i + 1].x, emph[i + 1].y); }
      ctx.stroke();
    }

    // nodes + labels (off-screen culled)
    const labelZoom = z > 1.4, scale = nodeScale();
    ctx.font = "11px Geist, sans-serif";
    const hoverId = hover && hover.id;
    for (let i = 0; i < N; i++) {
      const n = nodes[i], s = sp[i];
      if (!onScreen(s)) continue;
      const r = radius(n) * scale;
      const dim = (query && !matches(n)) || (neighborSet && !isFocus(n.id));
      ctx.globalAlpha = dim ? 0.12 : 1;
      ctx.fillStyle = commColor.get(n.community) || "#7c766e";
      ctx.beginPath(); ctx.arc(s.x, s.y, r, 0, TAU); ctx.fill();
      if (n.id === G.meta.index_id) {
        ctx.globalAlpha = dim ? 0.2 : 1;
        ctx.strokeStyle = "#f5f2ec"; ctx.lineWidth = 1.5;
        ctx.beginPath(); ctx.arc(s.x, s.y, r + 3, 0, TAU); ctx.stroke();
      }
      if (!dim && (n.degree >= 8 || labelZoom || isFocus(n.id) || n.id === hoverId)) {
        ctx.globalAlpha = 0.9; ctx.fillStyle = "#f5f2ec";
        ctx.fillText(n.label || n.id, s.x + r + 4, s.y + 4);
      }
    }
    ctx.globalAlpha = 1;
  }

  function isFocus(id) {
    if (!neighborSet) return false;
    const f = selected || hover;
    return neighborSet.has(id) || (f && id === f.id);
  }

  // ---- camera ----
  function fitView() {
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    for (const n of G.nodes) {
      const p = pos(n);
      if (!Number.isFinite(p.x) || !Number.isFinite(p.y)) continue;
      minX = Math.min(minX, p.x); minY = Math.min(minY, p.y);
      maxX = Math.max(maxX, p.x); maxY = Math.max(maxY, p.y);
    }
    if (!isFinite(minX)) { cam.x = 0; cam.y = 0; cam.zoom = 1; markDirty(); return; }
    cam.x = (minX + maxX) / 2; cam.y = (minY + maxY) / 2;
    const spanX = (maxX - minX) || 1, spanY = (maxY - minY) || 1;
    cam.zoom = Math.min(0.95, (W - 120) / spanX, (H - 160) / spanY);
    if (!isFinite(cam.zoom) || cam.zoom <= 0) cam.zoom = 1;
    markDirty();
  }

  // ---- hit test ----
  function nodeAt(sx, sy) {
    let best = null, bestD = Infinity;
    const scale = nodeScale();
    for (const n of G.nodes) {
      if (query && !matches(n)) continue;
      const p = toScreen(pos(n));
      const r = radius(n) * scale + 4;
      const d = (p.x - sx) ** 2 + (p.y - sy) ** 2;
      if (d <= r * r && d < bestD) { best = n; bestD = d; }
    }
    return best;
  }

  // ---- card + focus ----
  function setFocus(n) { neighborSet = n ? new Set(adj.get(n.id) || []) : null; }
  function showCard(n) {
    const card = $("card");
    const color = commColor.get(n.community) || "#7c766e"; // sanitized at boot
    const comm = G.communities.find((c) => c.id === n.community) || {};
    const editor = "vscode://file/" + joinPath(G.meta.vault, n.path) + ":" + (n.line || 1);
    const tags = (n.tags || []).map((t) => `<span class="chip">#${esc(t)}</span>`).join("");
    card.innerHTML = `
      <h2><span class="swatch" style="background:${esc(color)}"></span>${esc(n.label || n.id)}</h2>
      <div class="path">${esc(n.path)}</div>
      <div class="meta">
        ${n.type ? `<span><span class="chip">${esc(n.type)}</span></span>` : ""}
        <span>links <b>${n.degree | 0}</b></span>
        <span>cluster <b>${esc(comm.label || ("#" + n.community))}</b></span>
        <span>orbit <b>${n.orbit | 0}</b></span>
      </div>
      ${tags ? `<div class="tags">${tags}</div>` : ""}
      <div class="actions">
        <a class="btn" href="${esc(editor)}">open in editor</a>
        <button class="btn ghost" id="copy">copy path</button>
      </div>`;
    card.classList.remove("hidden");
    const cp = $("copy");
    if (cp) cp.onclick = () => navigator.clipboard && navigator.clipboard.writeText(joinPath(G.meta.vault, n.path));
  }
  function hideCard() { if (!selected) $("card").classList.add("hidden"); }

  // ---- search ----
  function matches(n) { return !query || (n.label || "").toLowerCase().includes(query) || (n.path || "").toLowerCase().includes(query); }
  function runSearch(q) {
    query = q.trim().toLowerCase();
    if (query) {
      const first = G.nodes.find(matches);
      if (first) { const p = pos(first); cam.x = p.x; cam.y = p.y; cam.zoom = Math.max(cam.zoom, 1.6); }
    }
    markDirty();
  }

  // ---- interaction ----
  function wire() {
    let dragging = false, lastX = 0, lastY = 0, moved = false;
    canvas.addEventListener("mousedown", (e) => { dragging = true; moved = false; lastX = e.clientX; lastY = e.clientY; canvas.classList.add("panning"); });
    window.addEventListener("mouseup", () => { dragging = false; canvas.classList.remove("panning"); });
    window.addEventListener("mousemove", (e) => {
      if (dragging) {
        const ddx = e.clientX - lastX, ddy = e.clientY - lastY;
        if (Math.abs(ddx) + Math.abs(ddy) > 2) moved = true;
        cam.x -= ddx / cam.zoom; cam.y -= ddy / cam.zoom; lastX = e.clientX; lastY = e.clientY;
        markDirty();
        return;
      }
      const rect = canvas.getBoundingClientRect();
      const n = nodeAt(e.clientX - rect.left, e.clientY - rect.top);
      canvas.classList.toggle("hovering", !!n);
      if (n !== hover) {
        hover = n;
        if (!selected) { setFocus(n); n ? showCard(n) : hideCard(); }
        markDirty();
      }
    });
    canvas.addEventListener("click", (e) => {
      if (moved) return;
      const rect = canvas.getBoundingClientRect();
      const n = nodeAt(e.clientX - rect.left, e.clientY - rect.top);
      if (n) { selected = n; setFocus(n); showCard(n); }
      else { selected = null; setFocus(null); hideCard(); }
      markDirty();
    });
    canvas.addEventListener("wheel", (e) => {
      e.preventDefault();
      const rect = canvas.getBoundingClientRect();
      const sx = e.clientX - rect.left, sy = e.clientY - rect.top;
      const wx = (sx - W / 2) / cam.zoom + cam.x, wy = (sy - H / 2) / cam.zoom + cam.y;
      cam.zoom = Math.max(0.1, Math.min(8, cam.zoom * Math.exp(-e.deltaY * 0.0012)));
      cam.x = wx - (sx - W / 2) / cam.zoom; cam.y = wy - (sy - H / 2) / cam.zoom;
      markDirty();
    }, { passive: false });

    $("view-graph").onclick = () => setView("graph");
    $("view-galaxy").onclick = () => setView("galaxy");
    $("q").addEventListener("input", (e) => runSearch(e.target.value));
    window.addEventListener("resize", () => { resize(); markDirty(); });
    window.addEventListener("keydown", (e) => {
      if (e.key === "Escape") { selected = null; setFocus(null); hideCard(); $("q").value = ""; query = ""; markDirty(); }
    });
  }

  function setView(v) {
    if (v === view) return;
    view = v;
    $("view-graph").classList.toggle("active", v === "graph");
    $("view-galaxy").classList.toggle("active", v === "galaxy");
    $("view-graph").setAttribute("aria-selected", v === "graph");
    $("view-galaxy").setAttribute("aria-selected", v === "galaxy");
    fitView();
    if (query) runSearch(query); // keep the search framing across a view switch
  }

  // ---- chrome / states ----
  function setStats() { $("stats").textContent = `${G.meta.node_count} notes / ${G.meta.edge_count} links / ${G.communities.length} clusters`; }
  function buildLegend() {
    const top = G.communities.slice(0, 8).filter((c) => c.label);
    if (!top.length) return;
    $("legend").innerHTML = top.map((c) => `<div class="row"><i style="background:${esc(c.color)}"></i><span>${esc(c.label)} (${c.size | 0})</span></div>`).join("");
    $("legend").classList.remove("hidden");
  }
  function resize() {
    dpr = Math.max(1, window.devicePixelRatio || 1);
    W = window.innerWidth; H = window.innerHeight;
    canvas.width = W * dpr; canvas.height = H * dpr;
    canvas.style.width = W + "px"; canvas.style.height = H + "px";
  }
  function doneOverlay() { overlay.classList.add("done", "hidden"); }
  function showEmpty() { overlay.classList.add("done"); overlayMsg.textContent = "no notes indexed yet. run: mesh index"; }
  function fail(err) { overlay.classList.add("done"); overlayMsg.textContent = "could not load the graph: " + (err && err.message ? err.message : err); }

  // ---- utils ----
  function joinPath(root, rel) { return (root || "").replace(/\/$/, "") + "/" + (rel || ""); }
  function esc(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }
})();
