// mesh ui 3D galaxy: a raw WebGL2 renderer (no three.js, no CDN, no deps) that lays
// the graph out as a real spiral galaxy: community clusters strung along spiral arms
// on a thin disc, a bright multi-layer core bulge at the index "sun", volumetric
// nebula dust, a twinkling multi-temperature starfield, depth fog, glowing edge
// filaments, and a cheap layered-halo bloom (no FBO pipeline). Instanced billboarded
// additive sprites hit 60fps at 1-2k notes. Exposed as
// window.Mesh3D.init(canvas, G, opts) -> api | null; init returns null when WebGL2 is
// unavailable so app.js falls back to the 2D galaxy.
(() => {
  "use strict";

  // --- tiny mat4 (column-major) ---
  function perspective(fovy, aspect, near, far) {
    const f = 1 / Math.tan(fovy / 2), nf = 1 / (near - far);
    return [f / aspect, 0, 0, 0, 0, f, 0, 0, 0, 0, (far + near) * nf, -1, 0, 0, 2 * far * near * nf, 0];
  }
  function mul(a, b) {
    const o = new Array(16);
    for (let c = 0; c < 4; c++) for (let r = 0; r < 4; r++) {
      o[c * 4 + r] = a[r] * b[c * 4] + a[4 + r] * b[c * 4 + 1] + a[8 + r] * b[c * 4 + 2] + a[12 + r] * b[c * 4 + 3];
    }
    return o;
  }
  // orbit view: rotate the world by yaw (Y) then pitch (X), then pull back by dist.
  function viewMatrix(yaw, pitch, dist) {
    const cy = Math.cos(yaw), sy = Math.sin(yaw), cp = Math.cos(pitch), sp = Math.sin(pitch);
    const ry = [cy, 0, -sy, 0, 0, 1, 0, 0, sy, 0, cy, 0, 0, 0, 0, 1];
    const rx = [1, 0, 0, 0, 0, cp, sp, 0, 0, -sp, cp, 0, 0, 0, 0, 1];
    const tr = [1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, -dist, 1];
    return mul(tr, mul(rx, ry));
  }

  function compile(gl, type, src) {
    const s = gl.createShader(type);
    gl.shaderSource(s, src); gl.compileShader(s);
    if (!gl.getShaderParameter(s, gl.COMPILE_STATUS)) { console.error("mesh3d shader:", gl.getShaderInfoLog(s)); return null; }
    return s;
  }
  function program(gl, vs, fs) {
    const v = compile(gl, gl.VERTEX_SHADER, vs), f = compile(gl, gl.FRAGMENT_SHADER, fs);
    if (!v || !f) return null;
    const p = gl.createProgram();
    gl.attachShader(p, v); gl.attachShader(p, f); gl.linkProgram(p);
    if (!gl.getProgramParameter(p, gl.LINK_STATUS)) { console.error("mesh3d link:", gl.getProgramInfoLog(p)); return null; }
    return p;
  }

  // deterministic [0,1) hash so the layout never jitters between frames or reloads.
  function rand(seed) {
    let x = (seed * 2654435761) >>> 0;
    x ^= x >>> 15; x = (x * 2246822519) >>> 0; x ^= x >>> 13;
    return (x >>> 0) / 4294967296;
  }

  function hexToRGB(hex) {
    let h = (hex || "#7c766e").replace("#", "");
    if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
    return [(parseInt(h.slice(0, 2), 16) || 0) / 255, (parseInt(h.slice(2, 4), 16) || 0) / 255, (parseInt(h.slice(4, 6), 16) || 0) / 255];
  }

  // One flexible sprite program drives every layer (stars, nebula, bulge, halos,
  // cores, the sun). Per-draw uniforms shape each layer: uSizeMul scales the
  // billboard, uSoft sets the falloff (low = wide soft cloud, high = tight point),
  // uIntensity sets brightness, uTwinkle adds per-sprite shimmer, uFog dims with
  // depth so the disc reads as 3D. The sun (iFlag>0.5) pulses and reads warm.
  const SPRITE_VS = `#version 300 es
  layout(location=0) in vec2 corner;
  layout(location=1) in vec3 iPos;
  layout(location=2) in float iSize;
  layout(location=3) in vec3 iColor;
  layout(location=4) in float iFlag;   // 1 = the index "sun"
  uniform mat4 uProj, uView;
  uniform float uHi;        // index of the highlighted instance, -1 none
  uniform float uTime;
  uniform float uSizeMul;   // per-layer billboard scale (halo pass uses >1)
  uniform float uTwinkle;   // per-layer shimmer amount
  out vec2 vUV; out vec3 vColor; out float vGlow; out float vViewZ;
  void main(){
    vec4 vp = uView * vec4(iPos, 1.0);
    float size = iSize * uSizeMul;
    bool sun = iFlag > 0.5;
    if (sun) size *= 1.7 + 0.10 * sin(uTime * 0.8);
    bool hi = abs(float(gl_InstanceID) - uHi) < 0.5;
    if (hi) size *= 1.5;
    float tw = 1.0 - uTwinkle * 0.5 + uTwinkle * 0.5 * sin(uTime * 1.6 + float(gl_InstanceID) * 0.7);
    vp.xy += corner * size;                       // view-space billboard (depth-scales)
    gl_Position = uProj * vp;
    vUV = corner;
    vColor = sun ? mix(iColor, vec3(1.0, 0.96, 0.86), 0.78) : iColor;
    vGlow = (sun ? 1.9 : (hi ? 1.7 : 1.0)) * tw;
    vViewZ = -vp.z;
  }`;

  const SPRITE_FS = `#version 300 es
  precision highp float;
  in vec2 vUV; in vec3 vColor; in float vGlow; in float vViewZ;
  out vec4 frag;
  uniform float uSoft;       // core falloff exponent (high = tight, sharp star)
  uniform float uHalo;       // amount of a faint wide halo around the core
  uniform float uIntensity;  // brightness
  uniform float uFog;        // depth-fade strength (0 = none, e.g. stars)
  uniform float uCamDist;    // camera distance, so fog tracks zoom
  void main(){
    float d = length(vUV);
    if (d > 1.0) discard;
    float c = 1.0 - d;
    // a sharp bright core (high power) + an optional faint wide halo = a real star,
    // not a fuzzy blob. The white-hot centre comes from additive over-exposure.
    float g = pow(c, uSoft) + pow(c, 1.7) * uHalo;
    float fade = mix(1.0, clamp(0.5 + (uCamDist - vViewZ) / 200.0, 0.18, 1.0), uFog);
    float glow = g * vGlow * uIntensity * fade;
    frag = vec4(vColor * glow, glow);            // additive (alpha = glow)
  }`;

  // edges: glowing filaments tinted by their endpoints, faded with depth.
  const LINE_VS = `#version 300 es
  layout(location=0) in vec3 aPos;
  layout(location=1) in vec3 aColor;
  uniform mat4 uProj, uView;
  out vec3 vC; out float vZ;
  void main(){ vec4 vp = uView * vec4(aPos, 1.0); vZ = -vp.z; gl_Position = uProj * vp; vC = aColor; }`;
  const LINE_FS = `#version 300 es
  precision highp float;
  in vec3 vC; in float vZ; out vec4 frag;
  uniform float uCamDist;
  void main(){
    float fade = clamp(0.5 + (uCamDist - vZ) / 200.0, 0.12, 1.0);
    float a = 0.055 * fade;
    frag = vec4((vC * 0.6 + vec3(0.3, 0.34, 0.5)) * a, a);
  }`;

  function init(canvas, G, opts) {
    const gl = canvas.getContext("webgl2", { antialias: true, alpha: false, premultipliedAlpha: false });
    if (!gl) return null;
    opts = opts || {};
    const commColor = opts.commColor || new Map();
    const indexId = opts.indexId || (G.meta && G.meta.index_id) || "";

    const sprite = program(gl, SPRITE_VS, SPRITE_FS);
    const line = program(gl, LINE_VS, LINE_FS);
    if (!sprite || !line) return null;

    // cache uniform locations.
    const su = {};
    ["uProj", "uView", "uHi", "uTime", "uSizeMul", "uTwinkle", "uSoft", "uHalo", "uIntensity", "uFog", "uCamDist"].forEach((n) => (su[n] = gl.getUniformLocation(sprite, n)));
    const lu = {};
    ["uProj", "uView", "uCamDist"].forEach((n) => (lu[n] = gl.getUniformLocation(line, n)));

    const nodes = G.nodes || [];
    const N = nodes.length;
    const idIndex = new Map();
    nodes.forEach((n, i) => idIndex.set(n.id, i));

    // --- galactic layout: a thin disc of spiral arms. Communities are strung along a
    // few arms at growing radius; nodes scatter around their community, flattened in Y
    // (a disc, not a ball); low-orbit (more central) notes sit tighter and flatter.
    // The index note is the bright core at the origin. ---
    const maxOrbit = Math.max(1, (G.meta && G.meta.max_orbit) || 1);
    const comms = (G.communities || []).map((c) => c.id);
    const commRank = new Map(comms.map((id, i) => [id, i]));
    const C = Math.max(1, comms.length);
    const DISC_R = 92, DISC_THICK = 13;
    const ARMS = Math.max(2, Math.min(4, Math.round(Math.sqrt(C))));
    const ARM_LEN = Math.ceil(C / ARMS);
    const TWO_PI = Math.PI * 2;
    function centroid(commId) {
      const k = commRank.has(commId) ? commRank.get(commId) : C - 1;
      const arm = k % ARMS;
      const along = Math.floor(k / ARMS);
      const rf = (along + 0.6) / ARM_LEN;                 // 0..1 along the arm
      const radius = DISC_R * (0.16 + 0.84 * rf);
      const theta = arm * (TWO_PI / ARMS) + rf * 2.2 + (rand(k * 13 + 1) - 0.5) * 0.18; // 2.2 = arm wind
      const cx = Math.cos(theta) * radius, cz = Math.sin(theta) * radius;
      const cy = (rand(k * 7 + 3) - 0.5) * DISC_THICK * (0.35 + 0.65 * (1 - rf));        // thinner outward
      return [cx, cy, cz];
    }

    const pos = new Float32Array(N * 3);
    const size = new Float32Array(N);
    const color = new Float32Array(N * 3);
    const flag = new Float32Array(N);
    for (let i = 0; i < N; i++) {
      const n = nodes[i];
      const isSun = n.id === indexId;
      let x, y, z;
      if (isSun) { x = 0; y = 0; z = 0; }
      else {
        const c = centroid(n.community);
        const local = 5 + (n.community % 4) * 1.4;
        const tight = 0.45 + 0.55 * ((n.orbit || 0) / maxOrbit); // low orbit = tighter to centroid
        const sx = (rand(i * 3 + 1) - 0.5), sy = (rand(i * 7 + 2) - 0.5), sz = (rand(i * 11 + 5) - 0.5);
        x = c[0] + sx * local * 2 * tight;
        y = c[1] + sy * local * 0.7 * tight; // flattened into the disc
        z = c[2] + sz * local * 2 * tight;
      }
      pos[i * 3] = x; pos[i * 3 + 1] = y; pos[i * 3 + 2] = z;
      // smaller, capped: a crisp colored star, never a giant blob (hubs were blowing out)
      size[i] = isSun ? 3.4 : Math.min(2.3, Math.max(0.85, (n.size || 1) * 0.62));
      const rgb = hexToRGB(commColor.get(n.community));
      color[i * 3] = rgb[0]; color[i * 3 + 1] = rgb[1]; color[i * 3 + 2] = rgb[2];
      flag[i] = isSun ? 1 : 0;
    }

    // --- sprite VAO builder (quad at loc0 + four per-instance attribs) ---
    const quad = new Float32Array([-1, -1, 1, -1, -1, 1, 1, 1]);
    function makeSprites(p, s, c, f) {
      const vao = gl.createVertexArray();
      gl.bindVertexArray(vao);
      const qb = gl.createBuffer(); gl.bindBuffer(gl.ARRAY_BUFFER, qb); gl.bufferData(gl.ARRAY_BUFFER, quad, gl.STATIC_DRAW);
      gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
      const mk = (loc, data, n) => {
        const b = gl.createBuffer(); gl.bindBuffer(gl.ARRAY_BUFFER, b); gl.bufferData(gl.ARRAY_BUFFER, data, gl.STATIC_DRAW);
        gl.enableVertexAttribArray(loc); gl.vertexAttribPointer(loc, n, gl.FLOAT, false, 0, 0); gl.vertexAttribDivisor(loc, 1);
      };
      mk(1, p, 3); mk(2, s, 1); mk(3, c, 3); mk(4, f, 1);
      gl.bindVertexArray(null);
      return vao;
    }
    const nodeVAO = makeSprites(pos, size, color, flag);

    // --- nebula dust: big soft community-tinted clouds along the arms + a warm core
    // halo, so the gaps fill with glowing gas. Decorative, not pickable. ---
    const neb = [];
    for (let k = 0; k < C; k++) {
      const c = centroid(comms[k]);
      const rgb = hexToRGB(commColor.get(comms[k]));
      const clouds = 3;
      for (let j = 0; j < clouds; j++) {
        const ox = (rand(k * 31 + j * 7 + 1) - 0.5) * 20, oy = (rand(k * 17 + j * 5 + 2) - 0.5) * 7, oz = (rand(k * 43 + j * 3 + 3) - 0.5) * 20;
        neb.push({ p: [c[0] + ox, c[1] + oy, c[2] + oz], s: 9 + rand(k * 9 + j) * 13, c: rgb });
      }
    }
    // warm gas in the bulge
    for (let j = 0; j < 8; j++) {
      const a = rand(j * 13 + 1) * TWO_PI, r = rand(j * 7 + 2) * 18;
      neb.push({ p: [Math.cos(a) * r, (rand(j * 5 + 3) - 0.5) * 6, Math.sin(a) * r], s: 14 + rand(j * 3) * 14, c: [0.95, 0.78, 0.5] });
    }
    const NEB = neb.length;
    const npos = new Float32Array(NEB * 3), nsize = new Float32Array(NEB), ncol = new Float32Array(NEB * 3), nflag = new Float32Array(NEB);
    neb.forEach((d, i) => {
      npos[i * 3] = d.p[0]; npos[i * 3 + 1] = d.p[1]; npos[i * 3 + 2] = d.p[2];
      nsize[i] = d.s; ncol[i * 3] = d.c[0]; ncol[i * 3 + 1] = d.c[1]; ncol[i * 3 + 2] = d.c[2];
    });
    const nebVAO = makeSprites(npos, nsize, ncol, nflag);

    // --- core bulge: a dense ball of small warm sprites around the sun, so the centre
    // reads full and bright like a galactic bulge. ---
    const BULGE = 110;
    const bpos = new Float32Array(BULGE * 3), bsize = new Float32Array(BULGE), bcol = new Float32Array(BULGE * 3), bflag = new Float32Array(BULGE);
    for (let i = 0; i < BULGE; i++) {
      const a = rand(i * 3 + 1) * TWO_PI, u = rand(i * 7 + 2), r = Math.pow(u, 0.6) * 16;
      bpos[i * 3] = Math.cos(a) * r; bpos[i * 3 + 1] = (rand(i * 11 + 3) - 0.5) * 7 * (1 - u); bpos[i * 3 + 2] = Math.sin(a) * r;
      bsize[i] = 1.1 + rand(i * 5) * 1.7;
      const warm = 0.85 + rand(i * 9) * 0.15;
      bcol[i * 3] = warm; bcol[i * 3 + 1] = warm * 0.86; bcol[i * 3 + 2] = warm * 0.6;
    }
    const bulgeVAO = makeSprites(bpos, bsize, bcol, bflag);

    // --- core corona: concentric warm halos at the origin for a layered glow. All
    // drawn in one instanced call, so the per-halo intensity is baked into its colour
    // (bigger halo = dimmer), giving a soft layered bloom around the core. ---
    const corona = [[14, 0.55], [26, 0.32], [44, 0.16]];
    const CORO = corona.length;
    const cpos = new Float32Array(CORO * 3), csize = new Float32Array(CORO), ccol = new Float32Array(CORO * 3), cflag = new Float32Array(CORO);
    corona.forEach((c, i) => { csize[i] = c[0]; const g = c[1]; ccol[i * 3] = 1.0 * g; ccol[i * 3 + 1] = 0.9 * g; ccol[i * 3 + 2] = 0.68 * g; });
    const coronaVAO = makeSprites(cpos, csize, ccol, cflag);

    // --- edges -> tinted line buffer (endpoint position + its node colour) ---
    const edges = G.edges || [];
    const lp = [], lc = [];
    for (const e of edges) {
      const a = idIndex.get(e.source), b = idIndex.get(e.target);
      if (a === undefined || b === undefined) continue;
      lp.push(pos[a * 3], pos[a * 3 + 1], pos[a * 3 + 2], pos[b * 3], pos[b * 3 + 1], pos[b * 3 + 2]);
      lc.push(color[a * 3], color[a * 3 + 1], color[a * 3 + 2], color[b * 3], color[b * 3 + 1], color[b * 3 + 2]);
    }
    const lineVerts = new Float32Array(lp), lineCols = new Float32Array(lc);
    const lvao = gl.createVertexArray();
    gl.bindVertexArray(lvao);
    const lb = gl.createBuffer(); gl.bindBuffer(gl.ARRAY_BUFFER, lb); gl.bufferData(gl.ARRAY_BUFFER, lineVerts, gl.STATIC_DRAW);
    gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 3, gl.FLOAT, false, 0, 0);
    const lcb = gl.createBuffer(); gl.bindBuffer(gl.ARRAY_BUFFER, lcb); gl.bufferData(gl.ARRAY_BUFFER, lineCols, gl.STATIC_DRAW);
    gl.enableVertexAttribArray(1); gl.vertexAttribPointer(1, 3, gl.FLOAT, false, 0, 0);
    gl.bindVertexArray(null);

    // --- starfield: a deep sphere of points, two colour temperatures, a few bright
    // ones, all twinkling. ---
    const STAR = 1200;
    const spos = new Float32Array(STAR * 3), ssize = new Float32Array(STAR), scol = new Float32Array(STAR * 3), sflag = new Float32Array(STAR);
    for (let i = 0; i < STAR; i++) {
      const y = 1 - (2 * i + 1) / STAR, r = Math.sqrt(Math.max(0, 1 - y * y)), phi = i * 2.399963229728653, R = 240 + rand(i * 9) * 120;
      spos[i * 3] = Math.cos(phi) * r * R; spos[i * 3 + 1] = y * R; spos[i * 3 + 2] = Math.sin(phi) * r * R;
      const bright = rand(i * 13) > 0.97;
      ssize[i] = (bright ? 1.6 : 0.45) + rand(i * 3) * 0.7;
      const warm = rand(i * 5) > 0.6; // ~40% warm, 60% cool-white
      if (warm) { scol[i * 3] = 1.0; scol[i * 3 + 1] = 0.88; scol[i * 3 + 2] = 0.72; }
      else { const w = 0.7 + rand(i * 17) * 0.3; scol[i * 3] = w * 0.86; scol[i * 3 + 1] = w * 0.92; scol[i * 3 + 2] = w * 1.1; }
    }
    const starVAO = makeSprites(spos, ssize, scol, sflag);

    // --- field stars: a DENSE disc of fine sharp points tracing the spiral arms, so
    // the galaxy reads as a real star field rather than a handful of blobs. Cool
    // silvery-blue with a warm minority; decorative, not pickable. This is the layer
    // that gives the disc its structure and density. ---
    const FIELD = 2400;
    const fpos = new Float32Array(FIELD * 3), fsize = new Float32Array(FIELD), fcol = new Float32Array(FIELD * 3), fflag = new Float32Array(FIELD);
    for (let i = 0; i < FIELD; i++) {
      const arm = i % ARMS;
      const t = Math.sqrt(rand(i * 3 + 1));                  // 0..1, fills out to the rim
      const radius = DISC_R * (0.06 + 0.98 * t);
      const s1 = rand(i * 7 + 2) - 0.5, s2 = rand(i * 11 + 3) - 0.5;
      const scatter = (s1 + s2) * 0.42;                      // ~gaussian spread around the arm
      const angle = arm * (TWO_PI / ARMS) + t * 2.2 + scatter; // same wind as the clusters
      const rr = radius + (rand(i * 13 + 4) - 0.5) * (6 + 12 * t);
      fpos[i * 3] = Math.cos(angle) * rr;
      fpos[i * 3 + 1] = (rand(i * 5 + 6) - 0.5) * DISC_THICK * (0.45 + 0.55 * (1 - t)) + (s1 + s2) * 2.0;
      fpos[i * 3 + 2] = Math.sin(angle) * rr;
      fsize[i] = 0.42 + rand(i * 17) * 0.7;
      const warm = rand(i * 19) > 0.74;
      const w = 0.5 + rand(i * 23) * 0.42;
      if (warm) { fcol[i * 3] = w * 1.05; fcol[i * 3 + 1] = w * 0.9; fcol[i * 3 + 2] = w * 0.72; }
      else { fcol[i * 3] = w * 0.82; fcol[i * 3 + 1] = w * 0.9; fcol[i * 3 + 2] = w * 1.1; }
    }
    const fieldVAO = makeSprites(fpos, fsize, fcol, fflag);

    // --- camera + interaction (with flick inertia + idle drift) ---
    const cam = { yaw: 0.6, pitch: -0.5, dist: 205 };
    const vel = { yaw: 0, pitch: 0 };
    let W = 1, H = 1, dpr = 1, proj = perspective(1.05, 1, 1, 3000), vp = viewMatrix(cam.yaw, cam.pitch, cam.dist);
    let drag = false, lx = 0, ly = 0, moved = false, hi = -1, time = 0, renderPitch = cam.pitch;

    function resize() {
      dpr = Math.min(2, Math.max(1, window.devicePixelRatio || 1));
      W = window.innerWidth; H = window.innerHeight;
      canvas.width = Math.round(W * dpr); canvas.height = Math.round(H * dpr);
      canvas.style.width = W + "px"; canvas.style.height = H + "px";
      proj = perspective(1.05, W / Math.max(1, H), 1, 3000);
    }
    resize();

    // pick: project every node to screen, return the nearest within a px radius.
    function pickAt(mx, my) {
      const v = viewMatrix(cam.yaw, renderPitch, cam.dist);
      const m = mul(proj, v);
      let best = -1, bestD = Infinity;
      for (let i = 0; i < N; i++) {
        const x = pos[i * 3], y = pos[i * 3 + 1], z = pos[i * 3 + 2];
        const cx = m[0] * x + m[4] * y + m[8] * z + m[12];
        const cy = m[1] * x + m[5] * y + m[9] * z + m[13];
        const cw = m[3] * x + m[7] * y + m[11] * z + m[15];
        if (cw <= 0) continue;
        const sx = (cx / cw * 0.5 + 0.5) * W, sy = (1 - (cy / cw * 0.5 + 0.5)) * H;
        const dx = sx - mx, dy = sy - my, d2 = dx * dx + dy * dy;
        const rad = Math.max(9, size[i] * 90 / cw);
        if (d2 <= rad * rad && d2 < bestD) { bestD = d2; best = i; }
      }
      return best;
    }

    function onDown(e) { drag = true; moved = false; lx = e.clientX; ly = e.clientY; vel.yaw = 0; vel.pitch = 0; }
    function onUp(e) {
      drag = false;
      if (!moved && opts.onSelect) {
        const rect = canvas.getBoundingClientRect();
        const i = pickAt(e.clientX - rect.left, e.clientY - rect.top);
        opts.onSelect(i >= 0 ? nodes[i] : null);
        hi = i;
      }
    }
    function onMove(e) {
      if (drag) {
        const dx = e.clientX - lx, dy = e.clientY - ly; lx = e.clientX; ly = e.clientY;
        if (Math.abs(dx) + Math.abs(dy) > 2) moved = true;
        const dyaw = dx * 0.005, dpitch = dy * 0.005;
        cam.yaw += dyaw; cam.pitch = Math.max(-1.45, Math.min(1.45, cam.pitch + dpitch));
        vel.yaw = dyaw; vel.pitch = dpitch; // remember for flick inertia
      } else if (opts.onHover) {
        const rect = canvas.getBoundingClientRect();
        const i = pickAt(e.clientX - rect.left, e.clientY - rect.top);
        if (i !== hi) { hi = i; opts.onHover(i >= 0 ? nodes[i] : null); }
        canvas.style.cursor = i >= 0 ? "pointer" : "grab";
      }
    }
    function onWheel(e) { e.preventDefault(); cam.dist *= Math.exp(e.deltaY * 0.001); cam.dist = Math.max(20, Math.min(1100, cam.dist)); }
    canvas.addEventListener("mousedown", onDown);
    window.addEventListener("mouseup", onUp);
    window.addEventListener("mousemove", onMove);
    canvas.addEventListener("wheel", onWheel, { passive: false });

    let autoSpin = true;
    function setHighlight(id) { hi = (id != null && idIndex.has(id)) ? idIndex.get(id) : -1; }

    // draw one sprite layer with its look uniforms.
    function drawLayer(vao, count, o) {
      gl.uniform1f(su.uSizeMul, o.sizeMul == null ? 1 : o.sizeMul);
      gl.uniform1f(su.uSoft, o.soft);
      gl.uniform1f(su.uHalo, o.halo || 0);
      gl.uniform1f(su.uIntensity, o.intensity);
      gl.uniform1f(su.uTwinkle, o.twinkle || 0);
      gl.uniform1f(su.uFog, o.fog || 0);
      gl.uniform1f(su.uHi, o.hi == null ? -1 : o.hi);
      gl.bindVertexArray(vao);
      gl.drawArraysInstanced(gl.TRIANGLE_STRIP, 0, 4, count);
    }

    function frame() {
      time += 0.03;
      if (!drag) {
        cam.yaw += vel.yaw; cam.pitch = Math.max(-1.45, Math.min(1.45, cam.pitch + vel.pitch));
        vel.yaw *= 0.94; vel.pitch *= 0.94;
        if (autoSpin && Math.abs(vel.yaw) < 0.0007) cam.yaw += 0.0011; // idle drift
      }
      renderPitch = cam.pitch + Math.sin(time * 0.18) * 0.035; // gentle breathing tilt
      vp = viewMatrix(cam.yaw, renderPitch, cam.dist);

      gl.viewport(0, 0, canvas.width, canvas.height);
      gl.clearColor(0.012, 0.011, 0.026, 1);
      gl.clear(gl.COLOR_BUFFER_BIT);
      gl.enable(gl.BLEND);
      gl.blendFunc(gl.SRC_ALPHA, gl.ONE); // additive glow
      gl.disable(gl.DEPTH_TEST);

      gl.useProgram(sprite);
      gl.uniformMatrix4fv(su.uProj, false, proj);
      gl.uniformMatrix4fv(su.uView, false, vp);
      gl.uniform1f(su.uTime, time);
      gl.uniform1f(su.uCamDist, cam.dist);

      // 1. nebula gas (faint, wide, fogged: a hint of colour behind the stars)
      drawLayer(nebVAO, NEB, { soft: 1.0, intensity: 0.05, twinkle: 0.0, fog: 0.8 });
      // 2. deep-space backdrop stars (sharp points, twinkle, no fog so they never vanish)
      drawLayer(starVAO, STAR, { soft: 6.0, intensity: 1.0, twinkle: 1.0, fog: 0.0 });
      // 3. the dense disc field stars: sharp silvery points tracing the spiral arms
      drawLayer(fieldVAO, FIELD, { soft: 6.0, halo: 0.08, intensity: 0.95, twinkle: 0.8, fog: 0.55 });

      // 4. edges (tinted glowing filaments)
      if (lineVerts.length) {
        gl.useProgram(line);
        gl.uniformMatrix4fv(lu.uProj, false, proj);
        gl.uniformMatrix4fv(lu.uView, false, vp);
        gl.uniform1f(lu.uCamDist, cam.dist);
        gl.bindVertexArray(lvao);
        gl.drawArrays(gl.LINES, 0, lineVerts.length / 3);
        gl.useProgram(sprite);
      }

      // 5. core corona (concentric warm halos at the origin; intensity baked per halo)
      drawLayer(coronaVAO, CORO, { soft: 1.2, intensity: 1.0, twinkle: 0.0, fog: 0.0 });
      // 6. bulge dust (sharp warm core stars)
      drawLayer(bulgeVAO, BULGE, { soft: 4.0, halo: 0.1, intensity: 0.7, twinkle: 0.6, fog: 0.3 });
      // 7. node bloom (modest tight halo on the named stars, not a giant blur)
      drawLayer(nodeVAO, N, { soft: 2.2, intensity: 0.14, twinkle: 0.4, fog: 0.6, sizeMul: 1.7, hi: hi });
      // 8. node cores (sharp, bright, community-coloured: the named stars pop)
      drawLayer(nodeVAO, N, { soft: 5.0, halo: 0.3, intensity: 1.25, twinkle: 0.5, fog: 0.6, sizeMul: 1.0, hi: hi });
      gl.bindVertexArray(null);
    }

    function dispose() {
      canvas.removeEventListener("mousedown", onDown);
      window.removeEventListener("mouseup", onUp);
      window.removeEventListener("mousemove", onMove);
      canvas.removeEventListener("wheel", onWheel);
      const ext = gl.getExtension("WEBGL_lose_context"); if (ext) ext.loseContext();
    }

    return { frame, resize, dispose, setHighlight, setData() {} };
  }

  window.Mesh3D = { init };
})();
