// mesh ui: a sovereign canvas graph viewer. No dependencies. Two living views over
// one graph: an Obsidian-style force graph (a continuous velocity sim you can grab
// and fling) and a galaxy orbiting the index note. Nodes are additive glow blobs;
// the index is a small sun.
(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const canvas = $("stage"), ctx = canvas.getContext("2d");
  const canvas3d = $("stage3d");
  let gl3d = null; // lazy WebGL2 galaxy; null until the 3D tab is first opened (or if unavailable)
  let captureStill = false; // ?still=1 capture mode: skip the galaxy fly-in + hide the build overlay
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
  // 2D fly-into-node + lock-on (mirrors the 3D camera): while camFollow is set the
  // layout freezes and the camera eases onto the node; camReturn eases back out.
  let camFollow = null, camReturn = null, preFocus = null;
  const FOCUS_ZOOM = 2.6;
  let hover = null, selected = null, query = "";
  let dpr = Math.max(1, window.devicePixelRatio || 1);
  let W = 0, H = 0;
  let galaxyAngle = 0;
  let galaxyScale = 1;       // scales the galaxy to the graph's extent so dots match size
  let alpha = 1;             // sim energy: cools to a floor so the graph stays alive
  let neighborSet = null;
  let spotlight = null; // community id to spotlight (dim the rest); set by the legend
  let drag = null;           // { node, vx, vy } while dragging/flinging a node
  let running = true;        // rAF gate; paused when the tab is hidden
  let lastInteract = 0;      // for easing galaxy rotation to a near-stop when idle
  let labelOrder = [];       // node indices by descending importance, for ambient labels
  const boxes = [];          // per-frame label rects, for the declutter pass
  let stars = [];
  const now = () => (window.performance && performance.now ? performance.now() : 0);
  let vignette = null;
  let t = 0;                 // frame clock for the sun pulse

  const safeColor = (c) => (HEX.test(c) ? c : "#7c766e");

  // ---- topic domains: an alternate grouping to the emergent link communities ----
  // Each note is bucketed into a business-area domain from its tags, so the galaxy can
  // be grouped + colored by topic (Engineering, Infra, Marketing, ...) with stable
  // colors and clean labels. This is a VIEW only: retrieval + the community graph are
  // untouched. A note's domain = the domain its tags vote for most (ties: first listed).
  const DOMAIN_DEFS = [
    { label: "Engineering", color: "#6ea8ff", tags: ["mesh","brightcrm","atomicsite","cookieproof","arachne","mithras","site-inspector","svar","frontend","backend","sqlc","mcp","webgl","workflow"] },
    { label: "Infra / DevOps", color: "#54c489", tags: ["dockyard","hephaestus","deploy","self-hosted","docker","tailscale","infra","dns","proxy","monitoring","coolify","zitadel"] },
    { label: "Marketing / SEO", color: "#f0a93b", tags: ["marketing","seo","cro","content","social","geo","aeo","growth","newsletter","copywriting","hormozi"] },
    { label: "Sales / Outreach", color: "#e8629a", tags: ["sales","outreach","leads","prospecting","law","advokat","crm"] },
    { label: "Security / Compliance", color: "#e0524e", tags: ["security","gdpr","audit","compliance","owasp","pii","encryption","secrets","vault","trustissues"] },
    { label: "Design", color: "#38c5d0", tags: ["design","branding","typography","visual","taste","css","font"] },
    { label: "Knowledge / Learning", color: "#b08cf0", tags: ["claude-skill","learning","networking","fundamentals","reference","concept","skill","basics","guide","patterns"] },
    { label: "General", color: "#8a8f9a", tags: [] },
  ];
  const GENERAL_DOMAIN = DOMAIN_DEFS.length - 1;
  const DOMAIN_TAG = new Map();
  DOMAIN_DEFS.forEach((d, i) => d.tags.forEach((t) => DOMAIN_TAG.set(t, i)));
  const domainColor = new Map();
  let grouping = "community"; // "community" (emergent link clusters) | "domain" (topic)

  function domainIndexFor(n) {
    // vote from tags + tokens of the id/title (so untagged notes that name their
    // project, e.g. "hephaestus-log", still classify).
    const idt = ((n.id || "") + " " + (n.label || "")).toLowerCase().split(/[^a-z0-9]+/);
    const toks = (n.tags || []).map((t) => String(t).toLowerCase()).concat(idt);
    const score = {};
    let best = -1, bestS = 0;
    for (const t of toks) {
      const di = DOMAIN_TAG.get(t);
      if (di == null) continue;
      score[di] = (score[di] || 0) + 1;
      if (score[di] > bestS) { bestS = score[di]; best = di; }
    }
    return best < 0 ? GENERAL_DOMAIN : best;
  }
  function computeDomains() {
    for (const n of G.nodes) n.domain = domainIndexFor(n);
    // pass 2: a note that matched nothing inherits its community's dominant domain
    // (communities are topical, so an untagged note in an infra cluster is Infra).
    const byComm = new Map();
    for (const n of G.nodes) {
      if (n.domain === GENERAL_DOMAIN) continue;
      const m = byComm.get(n.community) || {};
      m[n.domain] = (m[n.domain] || 0) + 1; byComm.set(n.community, m);
    }
    const commDom = new Map();
    for (const [comm, m] of byComm) {
      let best = -1, bs = 0;
      for (const k in m) if (m[k] > bs) { bs = m[k]; best = +k; }
      if (best >= 0) commDom.set(comm, best);
    }
    for (const n of G.nodes) if (n.domain === GENERAL_DOMAIN && commDom.has(n.community)) n.domain = commDom.get(n.community);
    const counts = new Array(DOMAIN_DEFS.length).fill(0);
    for (const n of G.nodes) counts[n.domain]++;
    G.domains = DOMAIN_DEFS.map((d, i) => ({ id: i, color: d.color, label: d.label, size: counts[i] }))
      .filter((d) => d.size > 0).sort((a, b) => b.size - a.size);
    domainColor.clear();
    for (const d of G.domains) domainColor.set(d.id, d.color);
  }
  const activeGroups = () => (grouping === "domain" ? (G.domains || []) : G.communities);
  const groupKeyOf = (n) => (grouping === "domain" ? n.domain : n.community);
  const groupColorOf = (n) => (grouping === "domain" ? domainColor : commColor).get(groupKeyOf(n)) || "#7c766e";

  fetch("graph.json").then((r) => {
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
    G.nodes.forEach((n, i) => { byId.set(n.id, n); nodeIndex.set(n.id, i); n.vx = 0; n.vy = 0; n.dispX = 0; n.dispY = 0; });
    computeDomains(); // assign each note a topic domain for the alternate grouping
    sp = new Array(G.nodes.length);
    buildAdjacency();
    labelOrder = G.nodes.map((_, i) => i).sort((a, b) =>
      G.nodes[b].degree - G.nodes[a].degree || nodeRadius(G.nodes[b]) - nodeRadius(G.nodes[a]) || (G.nodes[a].id < G.nodes[b].id ? -1 : 1));
    buildSprites();
    seedLayout();
    layoutGalaxy();
    setStats();
    buildLegend();
    buildExplorer();
    fitView();
    doneOverlay();
    wire();
    // closing the note reader eases the 3D camera back out of the star it flew into.
    if (window.Mesh) Mesh.onNoteClose = () => { if (view === "galaxy3d") { if (gl3d) gl3d.clearFocus(); } else clearFocus2d(); };
    lastInteract = now();
    requestAnimationFrame(loop);
    // Deep-link a view: ?v=galaxy3d (or galaxy) opens straight into it - shareable,
    // and lets a headless capture land on the 3D galaxy without a click.
    const params = new URLSearchParams(location.search);
    captureStill = params.get("still") === "1";
    const dv = params.get("v");
    if (dv === "galaxy3d" || dv === "galaxy") setView(dv);
    if (captureStill) overlay.style.display = "none"; // deterministic still capture
  }

  const adj = new Map();
  function buildAdjacency() {
    for (const n of G.nodes) adj.set(n.id, []);
    for (const e of G.edges) {
      if (e.source === e.target) continue;
      if (adj.has(e.source) && adj.has(e.target)) { adj.get(e.source).push(e.target); adj.get(e.target).push(e.source); }
    }
  }

  // buildInfluence weights the dragged node (1) + its 1-hop (0.55) and 2-hop
  // (0.22) neighbors, so a galaxy drag pulls the local cluster elastically. Hub
  // fan-out is capped so dragging a 382-link node does not haul a third of the map.
  function buildInfluence(root) {
    const m = new Map([[root.id, 1]]);
    let n1 = adj.get(root.id) || [];
    if (n1.length > 90) n1 = n1.slice(0, 90);
    for (const a of n1) if (!m.has(a)) m.set(a, 0.72); // 1-hop comes along strongly
    for (const a of n1) for (const b of (adj.get(a) || [])) {
      if (!m.has(b)) m.set(b, 0.42); // 2-hop follows clearly
      for (const c of (adj.get(b) || [])) if (!m.has(c)) m.set(c, 0.18); // 3-hop drifts a little
    }
    return m;
  }

  // ---- glow sprites (built once per community color; drawn additively) ----
  function buildSprites() {
    for (const c of G.communities) sprites.set(c.color, haloSprite(c.color));
    for (const d of DOMAIN_DEFS) sprites.set(d.color, haloSprite(d.color)); // topic-grouping colors
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
      // a real star: hot bright core fading through the community hue (the "sun
      // pulling planets" look), bright but not the old nuclear white-out.
      grad.addColorStop(0, `rgba(255,252,246,0.95)`);
      grad.addColorStop(0.08, `rgba(255,243,224,0.78)`);
      grad.addColorStop(0.26, `rgba(${r},${gg},${b},0.4)`);
      grad.addColorStop(0.55, `rgba(${r},${gg},${b},0.14)`);
      grad.addColorStop(1, `rgba(${r},${gg},${b},0)`);
    } else {
      // glow back, a touch: a bit more deposit than the de-tackify low, still under
      // 1.0 for a moderately dense cluster (the force swirl keeps it from collapsing).
      grad.addColorStop(0, `rgba(${r},${gg},${b},0.18)`);
      grad.addColorStop(0.35, `rgba(${r},${gg},${b},0.06)`);
      grad.addColorStop(1, `rgba(${r},${gg},${b},0)`);
    }
    g.fillStyle = grad;
    g.beginPath(); g.arc(R, R, R, 0, TAU); g.fill();
    return cv;
  }

  // ---- layouts ----
  function seedLayout() {
    // Scatter randomly across a disk so the sim resolves into an organic shape
    // instead of betraying a seeded ring/arc. The springs still pull communities
    // together as it settles.
    const spread = Math.sqrt(G.nodes.length + 1) * 95;
    for (const n of G.nodes) {
      const a = Math.random() * TAU, r = Math.sqrt(Math.random()) * spread;
      n.gx = Math.cos(a) * r; n.gy = Math.sin(a) * r;
    }
  }
  function layoutGalaxy() {
    const ringGap = 150;
    const byOrbit = new Map();
    for (const n of G.nodes) { const o = n.orbit || 0; (byOrbit.get(o) || byOrbit.set(o, []).get(o)).push(n); }
    for (const [o, ring] of byOrbit) {
      ring.sort((a, b) => a.community - b.community || (a.id < b.id ? -1 : 1));
      ring.forEach((n, i) => {
        // Scatter, not a perfect ring: jitter each planet's angle within its slot
        // and its radius, and give it its own orbital speed, so the galaxy looks
        // organic (uneven bands) rather than concentric circles. Deterministic per
        // node (seeded by orbit+index) so it is stable across frames.
        const seed = o * 131 + i;
        const slot = ring.length <= 1 ? 0 : (i / ring.length) * TAU;
        n.theta0 = slot + (rand(seed) - 0.5) * (TAU / Math.max(1, ring.length)) * 1.4;
        n.radius0 = o === 0 ? 0 : o * ringGap + (rand(seed + 7) - 0.5) * ringGap * 0.85;
        n.speed = (1 / Math.sqrt(o + 1)) * (0.8 + rand(seed + 13) * 0.5);
      });
    }
  }
  function galaxyPos(n) {
    const r0 = (n.radius0 || 0) * galaxyScale;
    if (r0 === 0) return { x: 0, y: 0 };
    const a = (n.theta0 || 0) + galaxyAngle * (n.speed || 0);
    return { x: Math.cos(a) * r0, y: Math.sin(a) * r0 };
  }

  // syncGalaxyScale resizes the galaxy so its extent matches the graph's, so both
  // views fit to the same zoom and the dots render at the same size.
  function syncGalaxyScale() {
    let rg = 0, rgal = 0;
    for (const n of G.nodes) {
      rg = Math.max(rg, Math.hypot(n.gx || 0, n.gy || 0));
      rgal = Math.max(rgal, n.radius0 || 0);
    }
    if (rgal > 0 && rg > 0) galaxyScale = rg / rgal;
  }
  function pos(n) { return view === "galaxy" ? galaxyPos(n) : { x: n.gx, y: n.gy }; }

  // ---- velocity force sim (graph view): repulsion (grid) + springs + gravity ----
  function simStep() {
    const nodes = G.nodes, N = nodes.length, k = 150;
    for (const v of nodes) { v.fx = 0; v.fy = 0; }
    if (N <= 1500) {
      // all-pairs repulsion: full long-range force, so the graph settles into an
      // even ROUND organic disk (the reference look) instead of the blocky shape
      // the local-grid cutoff produced. O(N^2) is trivial at this scale.
      for (let i = 0; i < N; i++) {
        const v = nodes[i];
        for (let j = i + 1; j < N; j++) {
          const u = nodes[j];
          let dx = v.gx - u.gx, dy = v.gy - u.gy, d2 = dx * dx + dy * dy;
          if (d2 < 1) { dx = (i < j ? 1 : -1) * 0.5; dy = 0.5; d2 = 1; }
          const inv = (k * k) / d2 * 0.02, fx = dx * inv, fy = dy * inv;
          v.fx += fx; v.fy += fy; u.fx -= fx; u.fy -= fy;
        }
      }
    } else {
      const cell = k, grid = new Map(); // big graphs: local grid for perf
      for (const v of nodes) {
        const key = ((v.gx / cell) | 0) + ":" + ((v.gy / cell) | 0);
        (grid.get(key) || grid.set(key, []).get(key)).push(v);
      }
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
    }
    for (const e of G.edges) {
      const a = byId.get(e.source), b = byId.get(e.target);
      if (!a || !b || a === b) continue;
      const dx = b.gx - a.gx, dy = b.gy - a.gy, d = Math.sqrt(dx * dx + dy * dy) || 1;
      const f = (d - k) * 0.012;
      const fx = (dx / d) * f, fy = (dy / d) * f;
      a.fx += fx; a.fy += fy; b.fx -= fx; b.fy -= fy;
    }
    for (const v of nodes) { v.fx -= v.gx * 0.0022; v.fy -= v.gy * 0.0022; } // central gravity -> a contained ROUND disk
    // Slow RIGID rotation only (no inner-faster shear: shear distorted the disk into
    // the blocky/rectangular shape). Pure rotation preserves the round organic shape
    // while it spins at the galaxy's cadence; eased when idle like the galaxy.
    const idle = (now() - lastInteract) > 4000;
    const damp = 0.9, OMEGA = 0.00005 * (idle ? 0.43 : 1);
    for (const v of nodes) {
      if (drag && v === drag.node) continue;
      v.vx = (v.vx + v.fx * alpha) * damp + (-v.gy * OMEGA);
      v.vy = (v.vy + v.fy * alpha) * damp + (v.gx * OMEGA);
      v.gx += v.vx; v.gy += v.vy;
    }
    if (alpha > 0.045) alpha *= 0.99; // keep the field gently responsive so it flows, not frozen
  }

  // ---- render ----
  function loop() {
    if (document.hidden) { running = false; return; } // pause when the tab is hidden (battery)
    if (view === "galaxy3d") { if (gl3d) gl3d.frame(); requestAnimationFrame(loop); return; } // 3D owns its own render; skip the 2D sim+draw
    t += 1;
    if (!camFollow) { // the layout freezes while locked onto a note, so it holds still
      if (view === "graph") simStep();
      else {
        const idle = (now() - lastInteract) > 4000;
        galaxyAngle += idle ? 0.0003 : 0.0007; // same cadence as the graph swirl; eases when idle
        const decay = (n) => {
          if (n.dispX || n.dispY) {
            n.dispX *= 0.86; n.dispY *= 0.86;
            if (Math.abs(n.dispX) < 0.5 && Math.abs(n.dispY) < 0.5) { n.dispX = 0; n.dispY = 0; }
          }
        };
        if (drag && drag.influence) {
          // elastic pull: the held node + its neighborhood spring toward the cursor
          // (1-hop at 0.55, 2-hop at 0.22), the rest decay back, all keep orbiting.
          const o = galaxyPos(drag.node), tgtX = drag.wx - o.x, tgtY = drag.wy - o.y;
          for (const n of G.nodes) {
            const w = drag.influence.get(n.id);
            if (w) { n.dispX += (w * tgtX - n.dispX) * 0.32; n.dispY += (w * tgtY - n.dispY) * 0.32; }
            else decay(n);
          }
        } else for (const n of G.nodes) decay(n);
      }
    }
    // fly-into-node lock-on / ease-back-out
    if (camFollow) {
      const p = pos(camFollow);
      cam.x += (p.x - cam.x) * 0.12; cam.y += (p.y - cam.y) * 0.12;
      cam.zoom += (FOCUS_ZOOM - cam.zoom) * 0.12;
    } else if (camReturn) {
      cam.x += (camReturn.x - cam.x) * 0.1; cam.y += (camReturn.y - cam.y) * 0.1;
      cam.zoom += (camReturn.zoom - cam.zoom) * 0.1;
      if (Math.hypot(cam.x - camReturn.x, cam.y - camReturn.y) < 2 && Math.abs(cam.zoom - camReturn.zoom) < 0.02) camReturn = null;
    }
    draw();
    requestAnimationFrame(loop);
  }

  function draw() {
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.fillStyle = "#050409";
    ctx.fillRect(0, 0, W, H);
    const z = cam.zoom, ox = W / 2 - cam.x * z, oy = H / 2 - cam.y * z;

    drawStars(z); // open deep-space starfield in BOTH views (no framed floor disc)

    const nodes = G.nodes, N = nodes.length, galaxy = view === "galaxy";
    for (let i = 0; i < N; i++) {
      const n = nodes[i];
      let px, py; // inline pos() to avoid N object allocations per frame
      if (galaxy) {
        const r0 = (n.radius0 || 0) * galaxyScale;
        if (r0 === 0) { px = 0; py = 0; }
        else { const a = (n.theta0 || 0) + galaxyAngle * (n.speed || 0); px = Math.cos(a) * r0; py = Math.sin(a) * r0; }
        px += n.dispX || 0; py += n.dispY || 0; // grab/spring-back offset
      } else { px = n.gx; py = n.gy; }
      const s = sp[i] || (sp[i] = { x: 0, y: 0 });
      s.x = px * z + ox; s.y = py * z + oy;
    }
    const on = (s, m) => s.x >= -m && s.x <= W + m && s.y >= -m && s.y <= H + m;

    // edges: always draw the faint web (that connective tissue IS the Obsidian
    // look), just very low alpha so it reads as structure not a hairball; the
    // focused subgraph lights up bright. Neutral grey, no chroma.
    const drawBase = true;
    const emph = [];
    ctx.lineWidth = Math.max(0.5, 0.65 * z);
    ctx.strokeStyle = neighborSet ? "rgba(150,150,165,0.022)" : "rgba(150,150,165,0.04)";
    ctx.beginPath();
    for (const e of G.edges) {
      const ai = nodeIndex.get(e.source), bi = nodeIndex.get(e.target);
      if (ai === undefined || bi === undefined) continue;
      const a = sp[ai], b = sp[bi];
      if (!on(a, 40) && !on(b, 40)) continue;
      if (neighborSet && (isFocus(e.source) || isFocus(e.target))) { emph.push(a, b); continue; }
      if (drawBase) curve(a, b);
    }
    if (drawBase) ctx.stroke();
    if (emph.length) {
      ctx.strokeStyle = "rgba(245,242,236,0.55)"; ctx.lineWidth = Math.max(0.9, 1.2 * z); ctx.beginPath();
      for (let i = 0; i < emph.length; i += 2) curve(emph[i], emph[i + 1]);
      ctx.stroke();
    }

    // glow halos (additive, low-deposit so a dense cluster saturates to a hue, not white)
    ctx.globalCompositeOperation = "lighter";
    for (let i = 0; i < N; i++) {
      const n = nodes[i], s = sp[i];
      if (!on(s, 80)) continue;
      const dim = dimmed(n);
      const core = nodeRadius(n) * z;
      if (n.id === G.meta.index_id) {
        const pulse = 1 + 0.035 * Math.sin(t * 0.02);
        blit(sunSprite, s, core * 5.5 * pulse, dim ? 0.3 : 0.92); // the sun glow, back
      } else {
        let halo = core * (2.5 + Math.min(1.6, n.degree * 0.022)); // a bit more reach
        if (z <= 0.6) halo *= 0.7;
        blit(sprites.get(groupColorOf(n)) || sprites.get("#7c766e"), s, halo, dim ? 0.06 : 0.3);
      }
    }
    ctx.globalCompositeOperation = "source-over";

    // crisp cores: clean glowing dots, NO border ring (shiny + spacy).
    for (let i = 0; i < N; i++) {
      const n = nodes[i], s = sp[i];
      if (!on(s, 10)) continue;
      const dim = dimmed(n);
      const r = coreR(n, z);
      ctx.globalAlpha = dim ? 0.18 : 1;
      ctx.fillStyle = n.id === G.meta.index_id ? "#fff6e8" : lighten(groupColorOf(n), 0.6);
      ctx.beginPath(); ctx.arc(s.x, s.y, r, 0, TAU); ctx.fill();
    }
    ctx.globalAlpha = 1;

    if (vignette) { ctx.fillStyle = vignette; ctx.fillRect(0, 0, W, H); }

    // labels: focus first (always, decluttered), then a capped ambient set by
    // importance, gated by a zoom band. Never the old "every hub at once" soup.
    boxes.length = 0;
    const hoverId = hover && hover.id;
    for (let i = 0; i < N; i++) {
      const n = nodes[i], s = sp[i];
      if (!on(s, 10)) continue;
      if (n.id === hoverId || (selected && n.id === selected.id) || isFocus(n.id)) placeLabel(n, s, coreR(n, z), 1);
    }
    const band = z < 0.9 ? 0 : z < 2.2 ? 1 : 2;
    if (band > 0) {
      const cap = band === 1 ? 12 : 18;
      const a = 0.35 + 0.55 * Math.max(0, Math.min(1, (z - 0.9) / 1.3));
      let placed = 0;
      for (const idx of labelOrder) {
        if (placed >= cap) break;
        const n = nodes[idx], s = sp[idx];
        if (!s || !on(s, 10)) continue;
        if (dimmed(n)) continue;
        if (n.id === hoverId || (selected && n.id === selected.id) || isFocus(n.id)) continue;
        if (placeLabel(n, s, coreR(n, z), a)) placed++;
      }
    }
  }

  function coreR(n, z) { return n.id === G.meta.index_id ? Math.max(2, nodeRadius(n) * z) * 1.6 : Math.max(2, nodeRadius(n) * z); }

  function measureLabel(n) {
    if (n._lw != null) return;
    ctx.font = "600 10.5px Geist, sans-serif";
    let txt = n.label || n.id || "";
    if (ctx.measureText(txt).width > 140) {
      while (txt.length > 1 && ctx.measureText(txt + "…").width > 140) txt = txt.slice(0, -1);
      txt += "…";
    }
    n._ltxt = txt; n._lw = Math.min(140, ctx.measureText(txt).width);
  }
  function placeLabel(n, s, r, alpha) {
    measureLabel(n);
    const x = s.x + r + 4, y = s.y - 7, w = n._lw + 10, h = 14;
    for (const b of boxes) if (b.x < x + w && x < b.x + b.w && b.y < y + h && y < b.y + b.h) return false;
    boxes.push({ x, y, w, h });
    ctx.globalAlpha = alpha;
    ctx.fillStyle = "rgba(10,9,8,0.72)";
    ctx.beginPath();
    if (ctx.roundRect) ctx.roundRect(x, y, w, h, 4); else ctx.rect(x, y, w, h);
    ctx.fill();
    ctx.strokeStyle = "rgba(255,255,255,0.06)"; ctx.lineWidth = 1; ctx.stroke();
    ctx.fillStyle = "#e9e4da"; ctx.font = "600 10.5px Geist, sans-serif"; ctx.textBaseline = "middle";
    ctx.fillText(n._ltxt, x + 5, y + h / 2);
    ctx.globalAlpha = 1;
    return true;
  }

  function curve(a, b) {
    const dx = b.x - a.x, dy = b.y - a.y, len = Math.hypot(dx, dy) || 1;
    const off = Math.min(len * 0.10, 18); // flatter bow so dense short edges stay near-straight
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
  // a node is dimmed by a text filter, a focus neighborhood, or a legend spotlight.
  function dimmed(n) { return (query && !matches(n)) || (neighborSet && !isFocus(n.id)) || (spotlight != null && groupKeyOf(n) !== spotlight); }
  function clearSpotlight() { if (spotlight == null) return; spotlight = null; if (gl3d) gl3d.setSpotlight(null); buildLegend(); }

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
    const color = groupColorOf(n);
    const comm = activeGroups().find((c) => c.id === groupKeyOf(n)) || {};
    const editor = "vscode://file/" + joinPath(G.meta.vault, n.path) + ":" + (n.line || 1);
    const tags = (n.tags || []).map((x) => `<span class="chip">#${esc(x)}</span>`).join("");
    card.innerHTML = `
      <h2><span class="swatch" style="background:${esc(color)}"></span>${esc(n.label || n.id)}</h2>
      <div class="path">${esc(n.path)}</div>
      <div class="meta">
        ${n.type ? `<span><span class="chip">${esc(n.type)}</span></span>` : ""}
        <span>links <b>${n.degree | 0}</b></span>
        <span>${grouping === "domain" ? "topic" : "cluster"} <b>${esc(comm.label || ("#" + groupKeyOf(n)))}</b></span>
        <span>orbit <b>${n.orbit | 0}</b></span>
      </div>
      ${tags ? `<div class="tags">${tags}</div>` : ""}
      <div class="actions">
        <button class="btn" id="read">Read note</button>
        <button class="btn ghost" id="copy">copy path</button>
        <a class="btn ghost" href="${esc(editor)}" title="Open in your editor (only works if the vault is open locally)">editor</a>
      </div>`;
    card.classList.remove("hidden");
    const rd = $("read");
    if (rd) rd.onclick = () => window.Mesh && Mesh.openNote && Mesh.openNote(n.id);
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
      moved = false; lastX = e.clientX; lastY = e.clientY; lastInteract = now();
      camFollow = null; camReturn = null; // grabbing/panning takes manual control back
      const n = nodeAt(mx, my); // grab a node in either view; empty space pans
      if (n) {
        const w = worldAt(mx, my);
        drag = { node: n, vx: 0, vy: 0, wx: w.x, wy: w.y };
        if (view === "graph") alpha = Math.max(alpha, 0.9);
        else drag.influence = buildInfluence(n); // galaxy: pull the local cluster elastically
        canvas.classList.add("panning");
      } else { panning = true; canvas.classList.add("panning"); }
    });
    window.addEventListener("mouseup", () => {
      if (drag) { if (view === "graph") { drag.node.vx = drag.vx; drag.node.vy = drag.vy; } drag = null; } // galaxy: offset springs back
      panning = false; canvas.classList.remove("panning");
    });
    window.addEventListener("mousemove", (e) => {
      const ddx = e.clientX - lastX, ddy = e.clientY - lastY;
      if (Math.abs(ddx) + Math.abs(ddy) > 2) moved = true;
      lastX = e.clientX; lastY = e.clientY; lastInteract = now();
      if (panning) { cam.x -= ddx / cam.zoom; cam.y -= ddy / cam.zoom; return; }
      const rect = canvas.getBoundingClientRect(); // once per event, not per axis
      const mx = e.clientX - rect.left, my = e.clientY - rect.top;
      if (drag) {
        const w = worldAt(mx, my);
        drag.wx = w.x; drag.wy = w.y; // galaxy: the loop applies this to the held node's offset
        if (view === "graph") {
          drag.vx = w.x - drag.node.gx; drag.vy = w.y - drag.node.gy;
          drag.node.gx = w.x; drag.node.gy = w.y; drag.node.vx = 0; drag.node.vy = 0;
        }
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
      if (n) { focusNodeById(n.id); if (window.Mesh && Mesh.openNote) Mesh.openNote(n.id); } // fly in + read
      else { selected = null; setFocus(null); hideCard(); clearFocus2d(); clearSpotlight(); if (window.Mesh && Mesh.closeNote) Mesh.closeNote(); }
    });
    canvas.addEventListener("wheel", (e) => {
      e.preventDefault();
      lastInteract = now();
      camFollow = null; camReturn = null; // zooming takes manual control back
      const rect = canvas.getBoundingClientRect(), sx = e.clientX - rect.left, sy = e.clientY - rect.top;
      const w = worldAt(sx, sy);
      cam.zoom = Math.max(0.08, Math.min(8, cam.zoom * Math.exp(-e.deltaY * 0.0012)));
      cam.x = w.x - (sx - W / 2) / cam.zoom; cam.y = w.y - (sy - H / 2) / cam.zoom;
    }, { passive: false });

    $("view-graph").onclick = () => setView("graph");
    $("view-galaxy").onclick = () => setView("galaxy");
    if ($("view-galaxy3d")) $("view-galaxy3d").onclick = () => setView("galaxy3d");
    $("q").addEventListener("input", (e) => runSearch(e.target.value));
    window.addEventListener("resize", resize);
    window.addEventListener("keydown", (e) => {
      if (e.key === "Escape") { selected = null; setFocus(null); hideCard(); clearFocus2d(); clearSpotlight(); $("q").value = ""; query = ""; }
    });
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden && !running) { running = true; requestAnimationFrame(loop); }
    });
  }

  function setViewTabs(v) {
    for (const id of ["graph", "galaxy", "galaxy3d"]) {
      const el = $("view-" + id);
      if (!el) continue;
      el.classList.toggle("active", id === v);
      el.setAttribute("aria-selected", id === v ? "true" : "false");
    }
  }
  // Create the 3D galaxy for the active grouping. The galaxy bakes node positions +
  // colors from the grouping at init, so switching grouping disposes + recalls this.
  function initGl3d() {
    if (!window.Mesh3D) return;
    const dom = grouping === "domain";
    gl3d = window.Mesh3D.init(canvas3d, G, {
      commColor: dom ? domainColor : commColor,
      groups: dom ? G.domains : G.communities,
      groupField: dom ? "domain" : "community",
      indexId: G.meta.index_id, still: captureStill,
      onSelect: (n) => {
        selected = n; setFocus(n);
        if (n) { showCard(n); if (gl3d) gl3d.focusNode(n.id); if (window.Mesh && Mesh.openNote) Mesh.openNote(n.id); }
        else { hideCard(); if (gl3d) gl3d.clearFocus(); if (window.Mesh && Mesh.closeNote) Mesh.closeNote(); }
      },
      onHover: (n) => { if (n) showCard(n); else if (!selected) hideCard(); },
    });
  }
  function setView(v) {
    if (v === view) return;
    if (v === "galaxy3d") {
      if (!gl3d) initGl3d();
      if (!gl3d) { // no WebGL2: disable the tab and fall back to the 2D galaxy
        const tab = $("view-galaxy3d");
        if (tab) { tab.disabled = true; tab.title = "WebGL2 unavailable"; }
        return setView("galaxy");
      }
      view = "galaxy3d";
      canvas.hidden = true; canvas3d.hidden = false;
      gl3d.resize();
      if (selected) gl3d.setHighlight(selected.id);
      setViewTabs(v);
      return;
    }
    view = v;
    canvas3d.hidden = true; canvas.hidden = false; // back to the 2D canvas
    if (v === "graph") alpha = Math.max(alpha, 0.5); // re-energize the sim on return
    else syncGalaxyScale(); // size the galaxy to the (now-settled) graph so dots match
    setViewTabs(v);
    fitView();
    if (query) runSearch(query);
  }

  // ---- chrome / states ----
  function setStats() { $("stats").textContent = `${G.meta.node_count} notes / ${G.meta.edge_count} links / ${activeGroups().length} ${grouping === "domain" ? "topics" : "clusters"}`; }
  function buildLegend() {
    const top = activeGroups().slice(0, 9).filter((c) => c.label);
    if (!top.length) return;
    const el = $("legend");
    el.innerHTML = "";
    // header doubles as a Clusters | Topics grouping switch
    const head = document.createElement("div");
    head.className = "legend-head";
    head.style.cssText = "display:flex;gap:6px;align-items:center;padding:0";
    [["community", "Clusters"], ["domain", "Topics"]].forEach(([g, lbl]) => {
      const t = document.createElement("button");
      t.textContent = lbl;
      t.title = g === "domain" ? "Group by topic domain (Engineering, Marketing, Sales, ...)" : "Group by the emergent link clusters";
      t.style.cssText = "flex:1;padding:3px 8px;border-radius:6px;border:1px solid rgba(255,255,255,.14);font:inherit;font-size:11px;cursor:pointer;color:inherit;background:" + (grouping === g ? "rgba(255,255,255,.16)" : "transparent");
      t.onclick = () => setGrouping(g);
      head.appendChild(t);
    });
    el.appendChild(head);
    top.forEach((c) => {
      const b = document.createElement("button");
      b.className = "row" + (spotlight === c.id ? " active" : "");
      b.title = "Spotlight this " + (grouping === "domain" ? "topic" : "cluster") + " (dim the rest). Click again to clear.";
      b.innerHTML = `<i style="background:${esc(c.color)}"></i><span>${esc(c.label)} (${c.size | 0})</span>`;
      b.onclick = () => { spotlight = (spotlight === c.id) ? null : c.id; if (gl3d) gl3d.setSpotlight(spotlight); buildLegend(); };
      el.appendChild(b);
    });
    el.classList.remove("hidden");
  }
  // Switch the active grouping (emergent clusters <-> topic domains). The 3D galaxy
  // bakes positions + colors at init, so it is disposed and recreated; the 2D views
  // re-read colors via groupColorOf on the next frame.
  function setGrouping(g) {
    if (g === grouping) return;
    grouping = g;
    spotlight = null;
    if (gl3d) { gl3d.dispose(true); gl3d = null; } // keep the GL context for the immediate re-init
    if (view === "galaxy3d") { initGl3d(); if (gl3d) { gl3d.resize(); if (selected) gl3d.setHighlight(selected.id); } }
    buildLegend(); setStats();
  }
  // ---- clusters explorer: browse communities + their notes, click to read ----
  function focusNodeById(id) {
    const n = G.nodes.find((x) => x.id === id);
    if (!n) return;
    selected = n; setFocus(n); showCard(n);
    if (view === "galaxy3d") { if (gl3d) gl3d.focusNode(id); } // fly the camera into the star
    else { if (!camFollow) preFocus = { x: cam.x, y: cam.y, zoom: cam.zoom }; camFollow = n; camReturn = null; lastInteract = now(); }
  }
  function clearFocus2d() {
    if (!camFollow) return;
    camReturn = preFocus || { x: cam.x, y: cam.y, zoom: cam.zoom };
    camFollow = null;
  }
  function buildExplorer() {
    const list = $("exp-list");
    if (!list) return;
    const byComm = new Map();
    for (const n of G.nodes) { if (!byComm.has(n.community)) byComm.set(n.community, []); byComm.get(n.community).push(n); }
    const order = [];
    const seen = new Set();
    for (const c of G.communities.slice().sort((a, b) => (b.size | 0) - (a.size | 0))) { if (byComm.has(c.id)) { order.push(c); seen.add(c.id); } }
    for (const [cid, mem] of byComm) if (!seen.has(cid)) order.push({ id: cid, label: "#" + cid, color: commColor.get(cid) || "#7c766e", size: mem.length });

    const typeSummary = (mem) => {
      const t = new Map();
      for (const m of mem) { const k = m.type || "note"; t.set(k, (t.get(k) || 0) + 1); }
      return [...t.entries()].sort((a, b) => b[1] - a[1]).map(([k, v]) => `${v} ${esc(k)}`).join(" &middot; ");
    };
    list.innerHTML = order.map((c) => {
      const mem = (byComm.get(c.id) || []).slice().sort((a, b) => (b.degree | 0) - (a.degree | 0) || ((a.label || a.id) < (b.label || b.id) ? -1 : 1));
      const notes = mem.map((n) =>
        `<button class="exp-note" data-id="${esc(n.id)}" data-name="${esc((n.label || n.id) + " " + (n.path || ""))}">` +
        `<span class="exp-type">${esc(n.type || "note")}</span>` +
        `<span class="exp-name">${esc(n.label || n.id)}</span>` +
        `<span class="exp-path">${esc(n.path || "")}</span></button>`).join("");
      return `<div class="exp-group"><button class="exp-comm"><i class="exp-sw" style="background:${esc(c.color)}"></i>` +
        `<span class="exp-clabel">${esc(c.label || ("#" + c.id))}</span><span class="exp-count">${mem.length}</span></button>` +
        `<div class="exp-sub">${typeSummary(mem)}</div><div class="exp-notes hidden">${notes}</div></div>`;
    }).join("");

    list.onclick = (e) => {
      const comm = e.target.closest(".exp-comm");
      if (comm) { const nn = comm.parentElement.querySelector(".exp-notes"); if (nn) nn.classList.toggle("hidden"); return; }
      const note = e.target.closest(".exp-note");
      if (note) { focusNodeById(note.dataset.id); if (window.Mesh && Mesh.openNote) Mesh.openNote(note.dataset.id); }
    };
    const filter = $("exp-filter");
    if (filter) filter.oninput = () => {
      const q = filter.value.trim().toLowerCase();
      list.querySelectorAll(".exp-group").forEach((g) => {
        let any = false;
        g.querySelectorAll(".exp-note").forEach((b) => { const hit = !q || (b.dataset.name || "").toLowerCase().includes(q); b.style.display = hit ? "" : "none"; if (hit) any = true; });
        g.style.display = any ? "" : "none";
        const nn = g.querySelector(".exp-notes"); if (q && nn) nn.classList.remove("hidden");
      });
    };

    const browseBtn = $("browse"), explorer = $("explorer"), expClose = $("exp-close");
    const toggleExplorer = (show) => {
      if (!explorer) return;
      const open = show == null ? explorer.classList.contains("hidden") : show;
      explorer.classList.toggle("hidden", !open);
      explorer.setAttribute("aria-hidden", String(!open));
      if (browseBtn) browseBtn.classList.toggle("active", open);
    };
    if (browseBtn) browseBtn.onclick = () => toggleExplorer();
    if (expClose) expClose.onclick = () => toggleExplorer(false);
  }

  function resize() {
    dpr = Math.max(1, window.devicePixelRatio || 1);
    W = window.innerWidth; H = window.innerHeight;
    canvas.width = W * dpr; canvas.height = H * dpr;
    canvas.style.width = W + "px"; canvas.style.height = H + "px";
    // soft, wide vignette only: a hint of depth at the far corners, NO hard frame
    const g = ctx.createRadialGradient(W / 2, H / 2, Math.min(W, H) * 0.55, W / 2, H / 2, Math.max(W, H) * 0.95);
    g.addColorStop(0, "rgba(6,5,10,0)"); g.addColorStop(1, "rgba(0,0,0,0.34)");
    vignette = g;
    stars = []; const n = Math.min(320, Math.round(W * H / 7000));
    for (let i = 0; i < n; i++) stars.push({ x: rand(i * 7) * W, y: rand(i * 13 + 3) * H, r: rand(i * 5) > 0.88 ? 2 : 1, a: 0.07 + rand(i * 3) * 0.2 });
    if (gl3d) gl3d.resize();
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
