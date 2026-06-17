// mesh ui: a sovereign canvas graph viewer. No dependencies. Two living views over
// one graph: an Obsidian-style force graph (a continuous velocity sim you can grab
// and fling) and a galaxy orbiting the index note. Nodes are additive glow blobs;
// the index is a small sun.
(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const canvas = $("stage"), ctx = canvas.getContext("2d");
  const overlay = $("overlay"), overlayMsg = $("overlay-msg");
  const TAU = Math.PI * 2;
  const HEX = /^#[0-9a-fA-F]{3,8}$/;

  let G = null;
  const byId = new Map();
  const nodeIndex = new Map();
  const commColor = new Map();
  const sprites = new Map(); // community color -> offscreen glow sprite
  let sunSprite = null;
  let sp = [];               // reused screen-position buffer
  let view = "graph";
  const cam = { x: 0, y: 0, zoom: 1 };
  let hover = null, selected = null, query = "";
  let dpr = Math.max(1, window.devicePixelRatio || 1);
  let W = 0, H = 0;
  let galaxyAngle = 0;
  let alpha = 1;             // sim energy: cools to a floor so the graph stays alive
  let neighborSet = null;
  let drag = null;           // { node, vx, vy } while dragging/flinging a node
  let running = true;        // rAF gate; paused when the tab is hidden
  let stars = [];
  let vignette = null;
  let t = 0;                 // frame clock for the sun pulse

  const safeColor = (c) => (HEX.test(c) ? c : "#7c766e");

  fetch("/graph.json").then((r) => {
    if (!r.ok) throw new Error("graph.json " + r.status);
    return r.json();
  }).then(boot).catch(fail);

  function boot(data) {
    G = data;
    G.communities = G.communities || []; // tolerate a graph indexed before community detection
    G.edges = G.edges || [];
    resize();
    if (!G.nodes || G.nodes.length === 0) return showEmpty();
    for (const c of G.communities) { c.color = safeColor(c.color); commColor.set(c.id, c.color); }
    G.nodes.forEach((n, i) => { byId.set(n.id, n); nodeIndex.set(n.id, i); n.vx = 0; n.vy = 0; });
    sp = new Array(G.nodes.length);
    buildAdjacency();
    buildSprites();
    seedLayout();
    layoutGalaxy();
    setStats();
    buildLegend();
    fitView();
    doneOverlay();
    wire();
    requestAnimationFrame(loop);
  }

  const adj = new Map();
  function buildAdjacency() {
    for (const n of G.nodes) adj.set(n.id, []);
    for (const e of G.edges) {
      if (e.source === e.target) continue;
      if (adj.has(e.source) && adj.has(e.target)) { adj.get(e.source).push(e.target); adj.get(e.target).push(e.source); }
    }
  }

  // ---- glow sprites (built once per community color; drawn additively) ----
  function buildSprites() {
    for (const c of G.communities) sprites.set(c.color, haloSprite(c.color));
    sprites.set("#7c766e", haloSprite("#7c766e"));
    sunSprite = haloSprite("#fff1dc", true);
  }
  function haloSprite(color, sun) {
    const R = 48;
    const cv = document.createElement("canvas");
    cv.width = cv.height = R * 2;
    const g = cv.getContext("2d");
    const grad = g.createRadialGradient(R, R, 0, R, R, R);
    const { r, gg, b } = rgb(color);
    if (sun) {
      grad.addColorStop(0, `rgba(255,255,255,0.95)`);
      grad.addColorStop(0.18, `rgba(${r},${gg},${b},0.85)`);
      grad.addColorStop(0.45, `rgba(${r},${gg},${b},0.28)`);
      grad.addColorStop(1, `rgba(${r},${gg},${b},0)`);
    } else {
      grad.addColorStop(0, `rgba(${r},${gg},${b},0.55)`);
      grad.addColorStop(0.5, `rgba(${r},${gg},${b},0.16)`);
      grad.addColorStop(1, `rgba(${r},${gg},${b},0)`);
    }
    g.fillStyle = grad;
    g.beginPath(); g.arc(R, R, R, 0, TAU); g.fill();
    return cv;
  }

  // ---- layouts ----
  function seedLayout() {
    const k = Math.sqrt(120000);
    const ci = new Map();
    G.communities.forEach((c, i) => ci.set(c.id, i));
    G.nodes.forEach((n, i) => {
      const base = ((ci.get(n.community) || 0) / Math.max(1, G.communities.length)) * TAU;
      const rr = k * (0.2 + (i % 23) * 0.05);
      n.gx = Math.cos(base + (i % 9) * 0.3) * rr;
      n.gy = Math.sin(base + (i % 9) * 0.3) * rr;
    });
  }
  function layoutGalaxy() {
    const ringGap = 150;
    const byOrbit = new Map();
    for (const n of G.nodes) { const o = n.orbit || 0; (byOrbit.get(o) || byOrbit.set(o, []).get(o)).push(n); }
    for (const [o, ring] of byOrbit) {
      ring.sort((a, b) => a.community - b.community || (a.id < b.id ? -1 : 1));
      ring.forEach((n, i) => {
        n.theta0 = ring.length <= 1 ? 0 : (i / ring.length) * TAU;
        n.radius0 = o * ringGap;
        n.speed = 1 / Math.sqrt(o + 1);
      });
    }
  }
  function galaxyPos(n) {
    const r0 = n.radius0 || 0;
    if (r0 === 0) return { x: 0, y: 0 };
    const a = (n.theta0 || 0) + galaxyAngle * (n.speed || 0);
    return { x: Math.cos(a) * r0, y: Math.sin(a) * r0 };
  }
  function pos(n) { return view === "galaxy" ? galaxyPos(n) : { x: n.gx, y: n.gy }; }

  // ---- velocity force sim (graph view): repulsion (grid) + springs + gravity ----
  function simStep() {
    const nodes = G.nodes, k = 150;
    const cell = k, grid = new Map();
    for (const v of nodes) {
      const key = ((v.gx / cell) | 0) + ":" + ((v.gy / cell) | 0);
      (grid.get(key) || grid.set(key, []).get(key)).push(v);
    }
    for (const v of nodes) { v.fx = 0; v.fy = 0; }
    for (const v of nodes) {
      const cx = (v.gx / cell) | 0, cy = (v.gy / cell) | 0;
      for (let gx = cx - 1; gx <= cx + 1; gx++)
        for (let gy = cy - 1; gy <= cy + 1; gy++) {
          const bucket = grid.get(gx + ":" + gy);
          if (!bucket) continue;
          for (const u of bucket) {
            if (u === v) continue;
            let dx = v.gx - u.gx, dy = v.gy - u.gy, d2 = dx * dx + dy * dy;
            if (d2 < 1) { dx = (v.id < u.id ? 1 : -1) * 0.5; dy = 0.5; d2 = 1; }
            if (d2 > k * k * 9) continue;
            const inv = (k * k) / d2;
            v.fx += dx * inv * 0.02; v.fy += dy * inv * 0.02;
          }
        }
    }
    for (const e of G.edges) {
      const a = byId.get(e.source), b = byId.get(e.target);
      if (!a || !b || a === b) continue;
      const dx = b.gx - a.gx, dy = b.gy - a.gy, d = Math.sqrt(dx * dx + dy * dy) || 1;
      const f = (d - k) * 0.012;
      const fx = (dx / d) * f, fy = (dy / d) * f;
      a.fx += fx; a.fy += fy; b.fx -= fx; b.fy -= fy;
    }
    for (const v of nodes) { v.fx -= v.gx * 0.0016; v.fy -= v.gy * 0.0016; }
    const damp = 0.82;
    for (const v of nodes) {
      if (drag && v === drag.node) continue;
      v.vx = (v.vx + v.fx * alpha) * damp;
      v.vy = (v.vy + v.fy * alpha) * damp;
      v.gx += v.vx; v.gy += v.vy;
    }
    if (alpha > 0.06) alpha *= 0.985; // cool toward a living floor, never fully freeze
  }

  // ---- render ----
  function loop() {
    if (document.hidden) { running = false; return; } // pause when the tab is hidden (battery)
    t += 1;
    if (view === "graph") simStep();
    else galaxyAngle += 0.0014;
    draw();
    requestAnimationFrame(loop);
  }

  function draw() {
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.fillStyle = "#06050a";
    ctx.fillRect(0, 0, W, H);
    const z = cam.zoom, ox = W / 2 - cam.x * z, oy = H / 2 - cam.y * z;

    if (view === "galaxy") drawStars(z);

    const nodes = G.nodes, N = nodes.length, galaxy = view === "galaxy";
    for (let i = 0; i < N; i++) {
      const n = nodes[i];
      let px, py; // inline pos() to avoid N object allocations per frame
      if (galaxy) {
        const r0 = n.radius0 || 0;
        if (r0 === 0) { px = 0; py = 0; }
        else { const a = (n.theta0 || 0) + galaxyAngle * (n.speed || 0); px = Math.cos(a) * r0; py = Math.sin(a) * r0; }
      } else { px = n.gx; py = n.gy; }
      const s = sp[i] || (sp[i] = { x: 0, y: 0 });
      s.x = px * z + ox; s.y = py * z + oy;
    }
    const on = (s, m) => s.x >= -m && s.x <= W + m && s.y >= -m && s.y <= H + m;

    // curved edges, batched; emphasized brighter
    ctx.lineWidth = Math.max(0.5, 0.7 * z);
    ctx.strokeStyle = "rgba(170,190,230,0.05)";
    ctx.beginPath();
    const emph = [];
    for (const e of G.edges) {
      const ai = nodeIndex.get(e.source), bi = nodeIndex.get(e.target);
      if (ai === undefined || bi === undefined) continue;
      const a = sp[ai], b = sp[bi];
      if (!on(a, 40) && !on(b, 40)) continue;
      if (neighborSet && (isFocus(e.source) || isFocus(e.target))) { emph.push(a, b); continue; }
      curve(a, b);
    }
    ctx.stroke();
    if (emph.length) {
      ctx.strokeStyle = "rgba(245,242,236,0.4)"; ctx.lineWidth = Math.max(0.8, 1.1 * z); ctx.beginPath();
      for (let i = 0; i < emph.length; i += 2) curve(emph[i], emph[i + 1]);
      ctx.stroke();
    }

    // glow halos (additive bloom)
    ctx.globalCompositeOperation = "lighter";
    for (let i = 0; i < N; i++) {
      const n = nodes[i], s = sp[i];
      if (!on(s, 80)) continue;
      const dim = (query && !matches(n)) || (neighborSet && !isFocus(n.id));
      const core = nodeRadius(n) * z;
      if (n.id === G.meta.index_id) {
        const pulse = 1 + 0.07 * Math.sin(t * 0.05);
        blit(sunSprite, s, core * 7 * pulse, dim ? 0.25 : 1);
      } else {
        // moderate halo alpha + extent so a dense single community blooms hot
        // without additive-summing to a white-out (the real 271-node Hive cluster).
        blit(sprites.get(commColor.get(n.community)) || sprites.get("#7c766e"), s, core * 4.5, dim ? 0.1 : 0.5);
      }
    }
    ctx.globalCompositeOperation = "source-over";

    // crisp cores + labels
    const labelZoom = z > 1.3, hoverId = hover && hover.id;
    ctx.font = "11px Geist, sans-serif"; ctx.textBaseline = "middle";
    for (let i = 0; i < N; i++) {
      const n = nodes[i], s = sp[i];
      if (!on(s, 10)) continue;
      const dim = (query && !matches(n)) || (neighborSet && !isFocus(n.id));
      const r = Math.max(1.6, nodeRadius(n) * z);
      ctx.globalAlpha = dim ? 0.18 : 1;
      ctx.fillStyle = n.id === G.meta.index_id ? "#fff6e8" : lighten(commColor.get(n.community) || "#7c766e", 0.45);
      ctx.beginPath(); ctx.arc(s.x, s.y, r, 0, TAU); ctx.fill();
      if (!dim && (n.degree >= 8 || labelZoom || isFocus(n.id) || n.id === hoverId)) {
        ctx.globalAlpha = 0.92; ctx.fillStyle = "#0a0908";
        ctx.fillText(n.label || n.id, s.x + r + 5, s.y + 1); // shadow
        ctx.fillStyle = "#f3efe7";
        ctx.fillText(n.label || n.id, s.x + r + 4, s.y);
      }
    }
    ctx.globalAlpha = 1;

    if (vignette) { ctx.fillStyle = vignette; ctx.fillRect(0, 0, W, H); }
  }

  function curve(a, b) {
    const dx = b.x - a.x, dy = b.y - a.y, len = Math.hypot(dx, dy) || 1;
    const off = Math.min(len * 0.12, 36); // gentle, capped bow (long edges do not over-arc)
    const mx = (a.x + b.x) / 2 - (dy / len) * off, my = (a.y + b.y) / 2 + (dx / len) * off;
    ctx.moveTo(a.x, a.y);
    ctx.quadraticCurveTo(mx, my, b.x, b.y);
  }
  function blit(spr, s, size, a) {
    if (!spr || size <= 0) return;
    ctx.globalAlpha = a;
    ctx.drawImage(spr, s.x - size, s.y - size, size * 2, size * 2);
  }
  function drawStars(z) {
    ctx.fillStyle = "#cdd6f4";
    const px = -cam.x * z * 0.25 + W / 2, py = -cam.y * z * 0.25 + H / 2;
    for (const st of stars) {
      const x = ((st.x + px) % W + W) % W, y = ((st.y + py) % H + H) % H;
      ctx.globalAlpha = st.a;
      ctx.fillRect(x, y, st.r, st.r);
    }
    ctx.globalAlpha = 1;
  }
  function nodeRadius(n) { return Math.max(1.4, (n.size || 1) * 1.7); }
  function isFocus(id) { if (!neighborSet) return false; const f = selected || hover; return neighborSet.has(id) || (f && id === f.id); }

  // ---- camera ----
  function fitView() {
    let a = Infinity, b = Infinity, c = -Infinity, d = -Infinity;
    for (const n of G.nodes) {
      const p = pos(n);
      if (!Number.isFinite(p.x) || !Number.isFinite(p.y)) continue;
      a = Math.min(a, p.x); b = Math.min(b, p.y); c = Math.max(c, p.x); d = Math.max(d, p.y);
    }
    if (!isFinite(a)) { cam.x = cam.y = 0; cam.zoom = 1; return; }
    cam.x = (a + c) / 2; cam.y = (b + d) / 2;
    cam.zoom = Math.min(1.1, (W - 140) / ((c - a) || 1), (H - 180) / ((d - b) || 1));
    if (!isFinite(cam.zoom) || cam.zoom <= 0) cam.zoom = 1;
  }

  function nodeAt(sx, sy) {
    let best = null, bestD = Infinity;
    for (let i = 0; i < G.nodes.length; i++) {
      const n = G.nodes[i];
      if (query && !matches(n)) continue;
      const s = sp[i]; if (!s) continue;
      const r = nodeRadius(n) * cam.zoom + 6;
      const dd = (s.x - sx) ** 2 + (s.y - sy) ** 2;
      if (dd <= r * r && dd < bestD) { best = n; bestD = dd; }
    }
    return best;
  }
  function worldAt(sx, sy) { return { x: (sx - W / 2) / cam.zoom + cam.x, y: (sy - H / 2) / cam.zoom + cam.y }; }

  // ---- card + focus ----
  function setFocus(n) { neighborSet = n ? new Set(adj.get(n.id) || []) : null; }
  function showCard(n) {
    const card = $("card");
    const color = commColor.get(n.community) || "#7c766e";
    const comm = G.communities.find((c) => c.id === n.community) || {};
    const editor = "vscode://file/" + joinPath(G.meta.vault, n.path) + ":" + (n.line || 1);
    const tags = (n.tags || []).map((x) => `<span class="chip">#${esc(x)}</span>`).join("");
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
    if (query) { const f = G.nodes.find(matches); if (f) { const p = pos(f); cam.x = p.x; cam.y = p.y; cam.zoom = Math.max(cam.zoom, 1.6); } }
  }

  // ---- interaction (grab + fling a node, pan empty space, zoom, hover, click) ----
  function wire() {
    let panning = false, lastX = 0, lastY = 0, moved = false;
    canvas.addEventListener("mousedown", (e) => {
      const rect = canvas.getBoundingClientRect(), mx = e.clientX - rect.left, my = e.clientY - rect.top;
      moved = false; lastX = e.clientX; lastY = e.clientY;
      const n = view === "graph" ? nodeAt(mx, my) : null;
      if (n) { drag = { node: n, vx: 0, vy: 0 }; alpha = Math.max(alpha, 0.9); canvas.classList.add("panning"); }
      else { panning = true; canvas.classList.add("panning"); }
    });
    window.addEventListener("mouseup", (e) => {
      if (drag) { drag.node.vx = drag.vx; drag.node.vy = drag.vy; drag = null; }
      panning = false; canvas.classList.remove("panning");
    });
    window.addEventListener("mousemove", (e) => {
      const ddx = e.clientX - lastX, ddy = e.clientY - lastY;
      if (Math.abs(ddx) + Math.abs(ddy) > 2) moved = true;
      lastX = e.clientX; lastY = e.clientY;
      if (panning) { cam.x -= ddx / cam.zoom; cam.y -= ddy / cam.zoom; return; }
      const rect = canvas.getBoundingClientRect(); // once per event, not per axis
      const mx = e.clientX - rect.left, my = e.clientY - rect.top;
      if (drag) {
        const w = worldAt(mx, my);
        drag.vx = w.x - drag.node.gx; drag.vy = w.y - drag.node.gy;
        drag.node.gx = w.x; drag.node.gy = w.y; drag.node.vx = 0; drag.node.vy = 0;
        return;
      }
      const n = nodeAt(mx, my);
      canvas.classList.toggle("hovering", !!n);
      if (n !== hover) { hover = n; if (!selected) { setFocus(n); n ? showCard(n) : hideCard(); } }
    });
    canvas.addEventListener("click", (e) => {
      if (moved) return;
      const rect = canvas.getBoundingClientRect();
      const n = nodeAt(e.clientX - rect.left, e.clientY - rect.top);
      if (n) { selected = n; setFocus(n); showCard(n); } else { selected = null; setFocus(null); hideCard(); }
    });
    canvas.addEventListener("wheel", (e) => {
      e.preventDefault();
      const rect = canvas.getBoundingClientRect(), sx = e.clientX - rect.left, sy = e.clientY - rect.top;
      const w = worldAt(sx, sy);
      cam.zoom = Math.max(0.08, Math.min(8, cam.zoom * Math.exp(-e.deltaY * 0.0012)));
      cam.x = w.x - (sx - W / 2) / cam.zoom; cam.y = w.y - (sy - H / 2) / cam.zoom;
    }, { passive: false });

    $("view-graph").onclick = () => setView("graph");
    $("view-galaxy").onclick = () => setView("galaxy");
    $("q").addEventListener("input", (e) => runSearch(e.target.value));
    window.addEventListener("resize", resize);
    window.addEventListener("keydown", (e) => {
      if (e.key === "Escape") { selected = null; setFocus(null); hideCard(); $("q").value = ""; query = ""; }
    });
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden && !running) { running = true; requestAnimationFrame(loop); }
    });
  }

  function setView(v) {
    if (v === view) return;
    view = v;
    if (v === "graph") alpha = Math.max(alpha, 0.5); // re-energize the sim on return
    $("view-graph").classList.toggle("active", v === "graph");
    $("view-galaxy").classList.toggle("active", v === "galaxy");
    $("view-graph").setAttribute("aria-selected", v === "graph");
    $("view-galaxy").setAttribute("aria-selected", v === "galaxy");
    fitView();
    if (query) runSearch(query);
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
    const g = ctx.createRadialGradient(W / 2, H / 2, Math.min(W, H) * 0.3, W / 2, H / 2, Math.max(W, H) * 0.75);
    g.addColorStop(0, "rgba(6,5,10,0)"); g.addColorStop(1, "rgba(0,0,0,0.55)");
    vignette = g;
    stars = []; const n = Math.min(260, Math.round(W * H / 9000));
    for (let i = 0; i < n; i++) stars.push({ x: rand(i * 7) * W, y: rand(i * 13 + 3) * H, r: rand(i * 5) > 0.85 ? 2 : 1, a: 0.08 + rand(i * 3) * 0.22 });
  }
  function doneOverlay() { overlay.classList.add("done", "hidden"); }
  function showEmpty() { overlay.classList.add("done"); overlayMsg.textContent = "no notes indexed yet. run: mesh index"; }
  function fail(err) { overlay.classList.add("done"); overlayMsg.textContent = "could not load the graph: " + (err && err.message ? err.message : err); }

  // ---- utils ----
  function rgb(hex) {
    let h = hex.replace("#", "");
    if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
    return { r: parseInt(h.slice(0, 2), 16) || 0, gg: parseInt(h.slice(2, 4), 16) || 0, b: parseInt(h.slice(4, 6), 16) || 0 };
  }
  function lighten(hex, amt) {
    const { r, gg, b } = rgb(hex);
    const m = (v) => Math.round(v + (255 - v) * amt);
    return `rgb(${m(r)},${m(gg)},${m(b)})`;
  }
  function rand(seed) { const x = Math.sin(seed * 99.13 + 17.7) * 43758.5453; return x - Math.floor(x); }
  function joinPath(root, rel) { return (root || "").replace(/\/$/, "") + "/" + (rel || ""); }
  function esc(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }
})();
