// mesh ui 3D galaxy: a raw WebGL2 renderer (no three.js, no CDN, no deps). A real
// spiral galaxy: community clusters strung along spiral arms on a thin disc that
// slowly turns (differential rotation, inner faster), a blazing multi-layer core
// bulge with dust lanes cutting the arms, a dense twinkling field-star disc, a real
// bloom post-process (HDR-ish bright-pass + separable blur + tonemapped composite),
// and a cinematic fly-in on open. Falls back to direct rendering if bloom FBOs are
// unavailable. Exposed as window.Mesh3D.init(canvas, G, opts) -> api | null.
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
  // orbit view about a center point (cx,cy,cz): translate the world so center is at
  // the origin, rotate (yaw,pitch), then pull back by dist. The center lets the
  // camera fly into and orbit a single note instead of always the galaxy origin.
  function viewMatrix(yaw, pitch, dist, cx, cy, cz) {
    const cyw = Math.cos(yaw), syw = Math.sin(yaw), cp = Math.cos(pitch), sp = Math.sin(pitch);
    const ry = [cyw, 0, -syw, 0, 0, 1, 0, 0, syw, 0, cyw, 0, 0, 0, 0, 1];
    const rx = [1, 0, 0, 0, 0, cp, sp, 0, 0, -sp, cp, 0, 0, 0, 0, 1];
    const tr = [1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, -dist, 1];
    const tc = [1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, -(cx || 0), -(cy || 0), -(cz || 0), 1];
    return mul(tr, mul(rx, mul(ry, tc)));
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

  // differential rotation about Y: inner radii turn faster, like a real galaxy. The
  // SAME formula is used in the shaders and in JS picking, so clicks stay accurate.
  const SPIN_GLSL = `
  vec3 spin(vec3 p, float t, float omega){
    float r = length(p.xz);
    if (r < 0.001 || omega == 0.0) return p;
    float a = t * omega * (0.4 + 30.0 / (r + 18.0));
    float c = cos(a), s = sin(a);
    return vec3(p.x * c - p.z * s, p.y, p.x * s + p.z * c);
  }`;
  function spinJS(x, y, z, t, omega) {
    const r = Math.hypot(x, z);
    if (r < 0.001 || omega === 0) return [x, y, z];
    const a = t * omega * (0.4 + 30.0 / (r + 18.0));
    const c = Math.cos(a), s = Math.sin(a);
    return [x * c - z * s, y, x * s + z * c];
  }

  const SPRITE_VS = `#version 300 es
  layout(location=0) in vec2 corner;
  layout(location=1) in vec3 iPos;
  layout(location=2) in float iSize;
  layout(location=3) in vec3 iColor;
  layout(location=4) in float iFlag;
  layout(location=5) in float iComm;
  uniform mat4 uProj, uView;
  uniform float uHi, uTime, uSpinTime, uSizeMul, uTwinkle, uOmega, uSpotComm;
  out vec2 vUV; out vec3 vColor; out float vGlow; out float vViewZ;
  ${SPIN_GLSL}
  void main(){
    vec3 wp = spin(iPos, uSpinTime, uOmega);
    vec4 vp = uView * vec4(wp, 1.0);
    float size = iSize * uSizeMul;
    bool sun = iFlag > 0.5;
    if (sun) size *= 1.7 + 0.10 * sin(uTime * 0.8);
    bool hi = abs(float(gl_InstanceID) - uHi) < 0.5;
    if (hi) size *= 1.5;
    float tw = 1.0 - uTwinkle * 0.5 + uTwinkle * 0.5 * sin(uTime * 1.6 + float(gl_InstanceID) * 0.7);
    vp.xy += corner * size;
    gl_Position = uProj * vp;
    vUV = corner;
    vColor = sun ? mix(iColor, vec3(1.0, 0.96, 0.86), 0.78) : iColor;
    vGlow = (sun ? 1.9 : (hi ? 1.7 : 1.0)) * tw;
    if (uSpotComm >= 0.0 && abs(iComm - uSpotComm) > 0.5) vGlow *= 0.10; // legend spotlight dims the rest
    vViewZ = -vp.z;
  }`;

  const SPRITE_FS = `#version 300 es
  precision highp float;
  in vec2 vUV; in vec3 vColor; in float vGlow; in float vViewZ;
  out vec4 frag;
  uniform float uSoft, uHalo, uIntensity, uFog, uCamDist;
  void main(){
    float d = length(vUV);
    if (d > 1.0) discard;
    float c = 1.0 - d;
    float g = pow(c, uSoft) + pow(c, 1.7) * uHalo;
    float fade = mix(1.0, clamp(0.5 + (uCamDist - vViewZ) / 200.0, 0.18, 1.0), uFog);
    float glow = g * vGlow * uIntensity * fade;
    frag = vec4(vColor * glow, glow);
  }`;

  const LINE_VS = `#version 300 es
  layout(location=0) in vec3 aPos;
  layout(location=1) in vec3 aColor;
  uniform mat4 uProj, uView;
  uniform float uSpinTime, uOmega;
  out vec3 vC; out float vZ;
  ${SPIN_GLSL}
  void main(){ vec3 wp = spin(aPos, uSpinTime, uOmega); vec4 vp = uView * vec4(wp, 1.0); vZ = -vp.z; gl_Position = uProj * vp; vC = aColor; }`;
  const LINE_FS = `#version 300 es
  precision highp float;
  in vec3 vC; in float vZ; out vec4 frag;
  uniform float uCamDist;
  void main(){
    float fade = clamp(0.5 + (uCamDist - vZ) / 200.0, 0.12, 1.0);
    float a = 0.055 * fade;
    frag = vec4((vC * 0.6 + vec3(0.3, 0.34, 0.5)) * a, a);
  }`;

  // fullscreen post-process programs (bloom).
  const FS_VS = `#version 300 es
  layout(location=0) in vec2 corner;
  out vec2 vUv;
  void main(){ vUv = corner * 0.5 + 0.5; gl_Position = vec4(corner, 0.0, 1.0); }`;
  const BRIGHT_FS = `#version 300 es
  precision highp float; in vec2 vUv; out vec4 frag;
  uniform sampler2D uTex; uniform float uThresh;
  void main(){ vec3 c = texture(uTex, vUv).rgb; frag = vec4(max(c - uThresh, 0.0) * 1.4, 1.0); }`;
  const BLUR_FS = `#version 300 es
  precision highp float; in vec2 vUv; out vec4 frag;
  uniform sampler2D uTex; uniform vec2 uDir;
  void main(){
    vec3 s = texture(uTex, vUv).rgb * 0.227027;
    s += texture(uTex, vUv + uDir * 1.384615).rgb * 0.316216;
    s += texture(uTex, vUv - uDir * 1.384615).rgb * 0.316216;
    s += texture(uTex, vUv + uDir * 3.230769).rgb * 0.070270;
    s += texture(uTex, vUv - uDir * 3.230769).rgb * 0.070270;
    frag = vec4(s, 1.0);
  }`;
  const COMPOSITE_FS = `#version 300 es
  precision highp float; in vec2 vUv; out vec4 frag;
  uniform sampler2D uScene; uniform sampler2D uBloom; uniform float uBloomStr;
  void main(){
    float r = length(vUv - 0.5);
    vec3 c = texture(uScene, vUv).rgb + texture(uBloom, vUv).rgb * uBloomStr;
    c += vec3(0.014, 0.020, 0.046) * (1.0 - smoothstep(0.0, 0.78, r)); // deep-blue ambient haze
    c = (c * (2.51 * c + 0.03)) / (c * (2.43 * c + 0.59) + 0.14);      // ACES-ish tonemap
    c *= 1.0 - 0.34 * smoothstep(0.42, 1.08, r);                       // cinematic vignette
    frag = vec4(c, 1.0);
  }`;

  const SPIN = 0.04; // disc angular-speed base (inner clusters turn faster)

  function init(canvas, G, opts) {
    const gl = canvas.getContext("webgl2", { antialias: true, alpha: false, premultipliedAlpha: false });
    if (!gl) return null;
    opts = opts || {};
    const commColor = opts.commColor || new Map();
    const indexId = opts.indexId || (G.meta && G.meta.index_id) || "";

    const sprite = program(gl, SPRITE_VS, SPRITE_FS);
    const line = program(gl, LINE_VS, LINE_FS);
    const bright = program(gl, FS_VS, BRIGHT_FS);
    const blur = program(gl, FS_VS, BLUR_FS);
    const composite = program(gl, FS_VS, COMPOSITE_FS);
    if (!sprite || !line) return null;

    const su = {};
    ["uProj", "uView", "uHi", "uTime", "uSpinTime", "uSizeMul", "uTwinkle", "uSoft", "uHalo", "uIntensity", "uFog", "uCamDist", "uOmega", "uSpotComm"].forEach((n) => (su[n] = gl.getUniformLocation(sprite, n)));
    const lu = {};
    ["uProj", "uView", "uCamDist", "uSpinTime", "uOmega"].forEach((n) => (lu[n] = gl.getUniformLocation(line, n)));

    const nodes = G.nodes || [];
    const N = nodes.length;
    const idIndex = new Map();
    nodes.forEach((n, i) => idIndex.set(n.id, i));

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
      const rf = (along + 0.6) / ARM_LEN;
      const radius = DISC_R * (0.16 + 0.84 * rf);
      const theta = arm * (TWO_PI / ARMS) + rf * 2.2 + (rand(k * 13 + 1) - 0.5) * 0.18;
      const cy = (rand(k * 7 + 3) - 0.5) * DISC_THICK * (0.35 + 0.65 * (1 - rf));
      return [Math.cos(theta) * radius, cy, Math.sin(theta) * radius];
    }

    const pos = new Float32Array(N * 3), size = new Float32Array(N), color = new Float32Array(N * 3), flag = new Float32Array(N), comm = new Float32Array(N);
    for (let i = 0; i < N; i++) {
      const n = nodes[i];
      comm[i] = n.community || 0;
      const isSun = n.id === indexId;
      let x, y, z;
      if (isSun) { x = 0; y = 0; z = 0; }
      else {
        const c = centroid(n.community);
        const local = 5 + (n.community % 4) * 1.4;
        const tight = 0.45 + 0.55 * ((n.orbit || 0) / maxOrbit);
        const sx = rand(i * 3 + 1) - 0.5, sy = rand(i * 7 + 2) - 0.5, sz = rand(i * 11 + 5) - 0.5;
        x = c[0] + sx * local * 2 * tight; y = c[1] + sy * local * 0.7 * tight; z = c[2] + sz * local * 2 * tight;
      }
      pos[i * 3] = x; pos[i * 3 + 1] = y; pos[i * 3 + 2] = z;
      size[i] = isSun ? 3.8 : Math.min(3.2, Math.max(1.15, (n.size || 1) * 0.78));
      const rgb = hexToRGB(commColor.get(n.community));
      color[i * 3] = rgb[0]; color[i * 3 + 1] = rgb[1]; color[i * 3 + 2] = rgb[2];
      flag[i] = isSun ? 1 : 0;
    }

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
    // node-only per-instance community (location 5) so the legend can spotlight a cluster.
    let spotComm = -1;
    gl.bindVertexArray(nodeVAO);
    const cb = gl.createBuffer(); gl.bindBuffer(gl.ARRAY_BUFFER, cb); gl.bufferData(gl.ARRAY_BUFFER, comm, gl.STATIC_DRAW);
    gl.enableVertexAttribArray(5); gl.vertexAttribPointer(5, 1, gl.FLOAT, false, 0, 0); gl.vertexAttribDivisor(5, 1);
    gl.bindVertexArray(null);

    // fullscreen quad VAO (for the bloom passes)
    const fsVAO = gl.createVertexArray();
    gl.bindVertexArray(fsVAO);
    const fsb = gl.createBuffer(); gl.bindBuffer(gl.ARRAY_BUFFER, fsb); gl.bufferData(gl.ARRAY_BUFFER, quad, gl.STATIC_DRAW);
    gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
    gl.bindVertexArray(null);

    // nebula gas
    const neb = [];
    for (let k = 0; k < C; k++) {
      const c = centroid(comms[k]); const rgb = hexToRGB(commColor.get(comms[k]));
      for (let j = 0; j < 3; j++) {
        const ox = (rand(k * 31 + j * 7 + 1) - 0.5) * 20, oy = (rand(k * 17 + j * 5 + 2) - 0.5) * 7, oz = (rand(k * 43 + j * 3 + 3) - 0.5) * 20;
        neb.push({ p: [c[0] + ox, c[1] + oy, c[2] + oz], s: 9 + rand(k * 9 + j) * 13, c: rgb });
      }
    }
    for (let j = 0; j < 8; j++) {
      const a = rand(j * 13 + 1) * TWO_PI, r = rand(j * 7 + 2) * 18;
      neb.push({ p: [Math.cos(a) * r, (rand(j * 5 + 3) - 0.5) * 6, Math.sin(a) * r], s: 14 + rand(j * 3) * 14, c: [0.95, 0.78, 0.5] });
    }
    // pink HII / star-forming regions scattered along the arms (real galaxies glow
    // pink where new stars ignite the hydrogen) - a big realism + beauty lever.
    for (let k = 0; k < C; k++) {
      if (rand(k * 53 + 7) < 0.55) continue;
      const c = centroid(comms[k]);
      const ox = (rand(k * 29 + 2) - 0.5) * 16, oz = (rand(k * 37 + 4) - 0.5) * 16;
      neb.push({ p: [c[0] + ox, c[1] + (rand(k * 5 + 1) - 0.5) * 5, c[2] + oz], s: 6 + rand(k * 9) * 9, c: [0.95, 0.42, 0.6] });
    }
    const NEB = neb.length;
    const npos = new Float32Array(NEB * 3), nsize = new Float32Array(NEB), ncol = new Float32Array(NEB * 3), nflag = new Float32Array(NEB);
    neb.forEach((d, i) => { npos[i * 3] = d.p[0]; npos[i * 3 + 1] = d.p[1]; npos[i * 3 + 2] = d.p[2]; nsize[i] = d.s; ncol[i * 3] = d.c[0]; ncol[i * 3 + 1] = d.c[1]; ncol[i * 3 + 2] = d.c[2]; });
    const nebVAO = makeSprites(npos, nsize, ncol, nflag);

    // core bulge (denser + brighter than before, for a blazing core)
    const BULGE = 150;
    const bpos = new Float32Array(BULGE * 3), bsize = new Float32Array(BULGE), bcol = new Float32Array(BULGE * 3), bflag = new Float32Array(BULGE);
    for (let i = 0; i < BULGE; i++) {
      const a = rand(i * 3 + 1) * TWO_PI, u = rand(i * 7 + 2), r = Math.pow(u, 0.6) * 17;
      bpos[i * 3] = Math.cos(a) * r; bpos[i * 3 + 1] = (rand(i * 11 + 3) - 0.5) * 7 * (1 - u); bpos[i * 3 + 2] = Math.sin(a) * r;
      bsize[i] = 1.1 + rand(i * 5) * 1.8;
      const warm = 0.88 + rand(i * 9) * 0.12;
      bcol[i * 3] = warm; bcol[i * 3 + 1] = warm * 0.86; bcol[i * 3 + 2] = warm * 0.6;
    }
    const bulgeVAO = makeSprites(bpos, bsize, bcol, bflag);

    // blazing core corona: concentric warm halos + a near-white centre (intensity
    // baked into the colour, since all are one instanced draw).
    const corona = [[6, 1.15, [1.0, 0.98, 0.92]], [22, 0.85, [1.0, 0.9, 0.66]], [46, 0.5, [1.0, 0.85, 0.58]], [86, 0.26, [1.0, 0.8, 0.52]], [140, 0.12, [1.0, 0.78, 0.5]]];
    const CORO = corona.length;
    const cpos = new Float32Array(CORO * 3), csize = new Float32Array(CORO), ccol = new Float32Array(CORO * 3), cflag = new Float32Array(CORO);
    corona.forEach((c, i) => { csize[i] = c[0]; const g = c[1]; ccol[i * 3] = c[2][0] * g; ccol[i * 3 + 1] = c[2][1] * g; ccol[i * 3 + 2] = c[2][2] * g; });
    const coronaVAO = makeSprites(cpos, csize, ccol, cflag);

    // edges -> tinted line buffer
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

    // background starfield (does NOT spin: the far sky)
    const STAR = 1200;
    const spos = new Float32Array(STAR * 3), ssize = new Float32Array(STAR), scol = new Float32Array(STAR * 3), sflag = new Float32Array(STAR);
    for (let i = 0; i < STAR; i++) {
      const y = 1 - (2 * i + 1) / STAR, r = Math.sqrt(Math.max(0, 1 - y * y)), phi = i * 2.399963229728653, R = 240 + rand(i * 9) * 120;
      spos[i * 3] = Math.cos(phi) * r * R; spos[i * 3 + 1] = y * R; spos[i * 3 + 2] = Math.sin(phi) * r * R;
      const bs = rand(i * 13) > 0.97;
      ssize[i] = (bs ? 1.6 : 0.45) + rand(i * 3) * 0.7;
      if (rand(i * 5) > 0.6) { scol[i * 3] = 1.0; scol[i * 3 + 1] = 0.88; scol[i * 3 + 2] = 0.72; }
      else { const w = 0.7 + rand(i * 17) * 0.3; scol[i * 3] = w * 0.86; scol[i * 3 + 1] = w * 0.92; scol[i * 3 + 2] = w * 1.1; }
    }
    const starVAO = makeSprites(spos, ssize, scol, sflag);

    // dense disc field stars tracing the arms, with DUST LANES (a dim band offset
    // from each arm ridge so the arms read as dusty, not uniform).
    const FIELD = 4400;
    const fpos = new Float32Array(FIELD * 3), fsize = new Float32Array(FIELD), fcol = new Float32Array(FIELD * 3), fflag = new Float32Array(FIELD);
    for (let i = 0; i < FIELD; i++) {
      const arm = i % ARMS;
      const t = Math.sqrt(rand(i * 3 + 1));
      const radius = DISC_R * (0.06 + 0.98 * t);
      const s1 = rand(i * 7 + 2) - 0.5, s2 = rand(i * 11 + 3) - 0.5;
      const scatter = (s1 + s2) * 0.42;
      const angle = arm * (TWO_PI / ARMS) + t * 2.2 + scatter;
      const rr = radius + (rand(i * 13 + 4) - 0.5) * (6 + 12 * t);
      fpos[i * 3] = Math.cos(angle) * rr;
      fpos[i * 3 + 1] = (rand(i * 5 + 6) - 0.5) * DISC_THICK * (0.45 + 0.55 * (1 - t)) + (s1 + s2) * 2.0;
      fpos[i * 3 + 2] = Math.sin(angle) * rr;
      // dust lane: a darker band a touch ahead of the arm ridge
      const lane = Math.abs(scatter - 0.16) < 0.055 ? 0.22 : 1.0;
      fsize[i] = (0.55 + rand(i * 17) * 0.9) * (lane < 1 ? 0.7 : 1);
      const w = (0.5 + rand(i * 23) * 0.42) * lane;
      const rad = Math.min(1, rr / DISC_R);            // 0 core .. 1 rim
      if (rand(i * 19) > 0.82) { fcol[i * 3] = w * 1.06; fcol[i * 3 + 1] = w * 0.84; fcol[i * 3 + 2] = w * 0.56; } // warm old stars
      else { // young blue stars, bluer toward the rim, warmer toward the core
        fcol[i * 3] = w * (0.72 + 0.16 * (1 - rad));
        fcol[i * 3 + 1] = w * 0.9;
        fcol[i * 3 + 2] = w * (0.92 + 0.28 * rad);
      }
    }
    const fieldVAO = makeSprites(fpos, fsize, fcol, fflag);

    // --- bloom FBOs (guarded; falls back to direct render) ---
    const bu = { uTex: gl.getUniformLocation(bright, "uTex"), uThresh: gl.getUniformLocation(bright, "uThresh") };
    const blu = { uTex: gl.getUniformLocation(blur, "uTex"), uDir: gl.getUniformLocation(blur, "uDir") };
    const cu = { uScene: gl.getUniformLocation(composite, "uScene"), uBloom: gl.getUniformLocation(composite, "uBloom"), uBloomStr: gl.getUniformLocation(composite, "uBloomStr") };
    let bloomOK = !!(bright && blur && composite);
    let sceneFBO = null, sceneTex = null, bFBO = [null, null], bTex = [null, null], bw = 1, bh = 1;
    function mkTex(w, h) {
      const t = gl.createTexture();
      gl.bindTexture(gl.TEXTURE_2D, t);
      gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, w, h, 0, gl.RGBA, gl.UNSIGNED_BYTE, null);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
      return t;
    }
    function mkFBO(t) {
      const f = gl.createFramebuffer();
      gl.bindFramebuffer(gl.FRAMEBUFFER, f);
      gl.framebufferTexture2D(gl.FRAMEBUFFER, gl.COLOR_ATTACHMENT0, gl.TEXTURE_2D, t, 0);
      const ok = gl.checkFramebufferStatus(gl.FRAMEBUFFER) === gl.FRAMEBUFFER_COMPLETE;
      gl.bindFramebuffer(gl.FRAMEBUFFER, null);
      return ok ? f : null;
    }
    function setupBloom(fw, fh) {
      if (!bloomOK) return;
      [sceneTex, bTex[0], bTex[1]].forEach((t) => t && gl.deleteTexture(t));
      [sceneFBO, bFBO[0], bFBO[1]].forEach((f) => f && gl.deleteFramebuffer(f));
      bw = Math.max(1, fw >> 1); bh = Math.max(1, fh >> 1);
      sceneTex = mkTex(fw, fh); sceneFBO = mkFBO(sceneTex);
      bTex[0] = mkTex(bw, bh); bFBO[0] = mkFBO(bTex[0]);
      bTex[1] = mkTex(bw, bh); bFBO[1] = mkFBO(bTex[1]);
      if (!sceneFBO || !bFBO[0] || !bFBO[1]) { bloomOK = false; console.warn("mesh3d: bloom unavailable, direct render"); }
    }

    // --- camera (cinematic fly-in, then flick inertia) ---
    const REST_DIST = 84;
    const cam = { yaw: 2.0, pitch: -0.5, dist: 560, cx: 0, cy: 0, cz: 0 };
    const vel = { yaw: 0, pitch: 0 };
    let W = 1, H = 1, dpr = 1, proj = perspective(1.05, 1, 1, 4000), vp = viewMatrix(cam.yaw, cam.pitch, cam.dist, 0, 0, 0);
    let drag = false, lx = 0, ly = 0, moved = false, hi = -1, time = 0, spinTime = 0, renderPitch = cam.pitch;
    let introT = 0, introDone = false, focusIdx = -1, anim = null;
    const endIntro = () => { introDone = true; };
    // animate the orbit center + distance, used to fly into a note and back out.
    function startAnim(toC, toD, after) { anim = { fromC: [cam.cx, cam.cy, cam.cz], toC: toC, fromD: cam.dist, toD: toD, t: 0, after: after || null }; }

    function resize() {
      dpr = Math.min(2, Math.max(1, window.devicePixelRatio || 1));
      W = window.innerWidth; H = window.innerHeight;
      canvas.width = Math.round(W * dpr); canvas.height = Math.round(H * dpr);
      canvas.style.width = W + "px"; canvas.style.height = H + "px";
      proj = perspective(1.05, W / Math.max(1, H), 1, 4000);
      setupBloom(canvas.width, canvas.height);
    }
    resize();

    function pickAt(mx, my) {
      const v = viewMatrix(cam.yaw, renderPitch, cam.dist, cam.cx, cam.cy, cam.cz);
      const m = mul(proj, v);
      let best = -1, bestD = Infinity;
      for (let i = 0; i < N; i++) {
        const sp = spinJS(pos[i * 3], pos[i * 3 + 1], pos[i * 3 + 2], spinTime, SPIN);
        const cx = m[0] * sp[0] + m[4] * sp[1] + m[8] * sp[2] + m[12];
        const cy = m[1] * sp[0] + m[5] * sp[1] + m[9] * sp[2] + m[13];
        const cw = m[3] * sp[0] + m[7] * sp[1] + m[11] * sp[2] + m[15];
        if (cw <= 0) continue;
        const sx = (cx / cw * 0.5 + 0.5) * W, sy = (1 - (cy / cw * 0.5 + 0.5)) * H;
        const dx = sx - mx, dy = sy - my, d2 = dx * dx + dy * dy;
        const rad = Math.max(9, size[i] * 90 / cw);
        if (d2 <= rad * rad && d2 < bestD) { bestD = d2; best = i; }
      }
      return best;
    }

    function onDown(e) { drag = true; moved = false; lx = e.clientX; ly = e.clientY; vel.yaw = 0; vel.pitch = 0; endIntro(); }
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
        vel.yaw = dyaw; vel.pitch = dpitch; anim = null; // dragging cancels a fly-in tween
      } else if (opts.onHover) {
        const rect = canvas.getBoundingClientRect();
        const i = pickAt(e.clientX - rect.left, e.clientY - rect.top);
        if (i !== hi) { hi = i; opts.onHover(i >= 0 ? nodes[i] : null); }
        canvas.style.cursor = i >= 0 ? "pointer" : "grab";
      }
    }
    function onWheel(e) { e.preventDefault(); endIntro(); cam.dist *= Math.exp(e.deltaY * 0.001); cam.dist = Math.max(20, Math.min(1100, cam.dist)); }
    canvas.addEventListener("mousedown", onDown);
    window.addEventListener("mouseup", onUp);
    window.addEventListener("mousemove", onMove);
    canvas.addEventListener("wheel", onWheel, { passive: false });

    function setHighlight(id) { hi = (id != null && idIndex.has(id)) ? idIndex.get(id) : -1; }
    function setSpotlight(commId) { spotComm = (commId == null) ? -1 : commId; } // legend cluster spotlight

    // Fly the camera into a note and lock onto it (the disc spin freezes so the note
    // stays put while you read). clearFocus eases back out and resumes the spin.
    function focusNode(id) {
      const i = (id != null && idIndex.has(id)) ? idIndex.get(id) : -1;
      if (i < 0) return;
      hi = i; focusIdx = i; endIntro();
      const sp = spinJS(pos[i * 3], pos[i * 3 + 1], pos[i * 3 + 2], spinTime, SPIN);
      startAnim([sp[0], sp[1], sp[2]], 42);
    }
    function clearFocus() {
      if (focusIdx < 0 && !anim) return;
      startAnim([0, 0, 0], REST_DIST, () => { focusIdx = -1; });
    }

    function setLayer(o) {
      gl.uniform1f(su.uSizeMul, o.sizeMul == null ? 1 : o.sizeMul);
      gl.uniform1f(su.uSoft, o.soft); gl.uniform1f(su.uHalo, o.halo || 0);
      gl.uniform1f(su.uIntensity, o.intensity); gl.uniform1f(su.uTwinkle, o.twinkle || 0);
      gl.uniform1f(su.uFog, o.fog || 0); gl.uniform1f(su.uHi, o.hi == null ? -1 : o.hi);
      gl.uniform1f(su.uOmega, o.omega || 0); gl.uniform1f(su.uSpotComm, o.spot == null ? -1 : o.spot);
    }
    function drawLayer(vao, count, o) { setLayer(o); gl.bindVertexArray(vao); gl.drawArraysInstanced(gl.TRIANGLE_STRIP, 0, 4, count); }

    function drawScene() {
      gl.clearColor(0.012, 0.011, 0.026, 1);
      gl.clear(gl.COLOR_BUFFER_BIT);
      gl.enable(gl.BLEND); gl.blendFunc(gl.SRC_ALPHA, gl.ONE); gl.disable(gl.DEPTH_TEST);

      gl.useProgram(sprite);
      gl.uniformMatrix4fv(su.uProj, false, proj); gl.uniformMatrix4fv(su.uView, false, vp);
      gl.uniform1f(su.uTime, time); gl.uniform1f(su.uSpinTime, spinTime); gl.uniform1f(su.uCamDist, cam.dist);

      drawLayer(nebVAO, NEB, { soft: 1.0, intensity: 0.13, fog: 0.8, omega: SPIN });
      drawLayer(starVAO, STAR, { soft: 6.0, intensity: 1.0, twinkle: 1.0, fog: 0.0, omega: 0 });
      drawLayer(fieldVAO, FIELD, { soft: 6.0, halo: 0.14, intensity: 1.18, twinkle: 0.8, fog: 0.55, omega: SPIN });

      if (lineVerts.length) {
        gl.useProgram(line);
        gl.uniformMatrix4fv(lu.uProj, false, proj); gl.uniformMatrix4fv(lu.uView, false, vp);
        gl.uniform1f(lu.uCamDist, cam.dist); gl.uniform1f(lu.uSpinTime, spinTime); gl.uniform1f(lu.uOmega, SPIN);
        gl.bindVertexArray(lvao); gl.drawArrays(gl.LINES, 0, lineVerts.length / 3);
        gl.useProgram(sprite);
        gl.uniformMatrix4fv(su.uProj, false, proj); gl.uniformMatrix4fv(su.uView, false, vp);
        gl.uniform1f(su.uTime, time); gl.uniform1f(su.uSpinTime, spinTime); gl.uniform1f(su.uCamDist, cam.dist);
      }

      drawLayer(coronaVAO, CORO, { soft: 1.2, intensity: 1.0, fog: 0.0, omega: 0 });
      drawLayer(bulgeVAO, BULGE, { soft: 4.0, halo: 0.1, intensity: 0.8, twinkle: 0.6, fog: 0.3, omega: SPIN });
      drawLayer(nodeVAO, N, { soft: 4.4, halo: 0.48, intensity: 1.62, twinkle: 0.5, fog: 0.6, sizeMul: 1.0, hi: hi, omega: SPIN, spot: spotComm });
      gl.bindVertexArray(null);
    }

    function frame() {
      time += 0.03;
      if (focusIdx < 0) spinTime += 0.03; // the disc turns only when not locked on a note
      if (!introDone) {
        introT += 0.016;
        const p = Math.min(1, introT / 2.8), e = 1 - Math.pow(1 - p, 3);
        cam.dist = 560 - (560 - REST_DIST) * e;
        cam.yaw = 2.0 - 1.4 * e;
        if (p >= 1) introDone = true;
      } else if (anim) {
        anim.t += 0.016 / 0.8;
        const p = Math.min(1, anim.t), e = 1 - Math.pow(1 - p, 3);
        cam.cx = anim.fromC[0] + (anim.toC[0] - anim.fromC[0]) * e;
        cam.cy = anim.fromC[1] + (anim.toC[1] - anim.fromC[1]) * e;
        cam.cz = anim.fromC[2] + (anim.toC[2] - anim.fromC[2]) * e;
        cam.dist = anim.fromD + (anim.toD - anim.fromD) * e;
        if (p >= 1) { const cb = anim.after; anim = null; if (cb) cb(); }
      } else if (!drag) {
        cam.yaw += vel.yaw; cam.pitch = Math.max(-1.45, Math.min(1.45, cam.pitch + vel.pitch));
        vel.yaw *= 0.94; vel.pitch *= 0.94;
      }
      renderPitch = cam.pitch + Math.sin(time * 0.18) * 0.03;
      vp = viewMatrix(cam.yaw, renderPitch, cam.dist, cam.cx, cam.cy, cam.cz);

      if (bloomOK) {
        gl.bindFramebuffer(gl.FRAMEBUFFER, sceneFBO);
        gl.viewport(0, 0, canvas.width, canvas.height);
        drawScene();
        // bright-pass downsample
        gl.disable(gl.BLEND);
        gl.bindFramebuffer(gl.FRAMEBUFFER, bFBO[0]); gl.viewport(0, 0, bw, bh);
        gl.useProgram(bright); gl.activeTexture(gl.TEXTURE0); gl.bindTexture(gl.TEXTURE_2D, sceneTex);
        gl.uniform1i(bu.uTex, 0); gl.uniform1f(bu.uThresh, 0.26);
        gl.bindVertexArray(fsVAO); gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
        // separable blur, ping-pong
        gl.useProgram(blur); gl.uniform1i(blu.uTex, 0);
        let src = 0, dst = 1;
        for (let k = 0; k < 4; k++) {
          gl.bindFramebuffer(gl.FRAMEBUFFER, bFBO[dst]); gl.viewport(0, 0, bw, bh);
          gl.bindTexture(gl.TEXTURE_2D, bTex[src]);
          if (k % 2 === 0) gl.uniform2f(blu.uDir, 1 / bw, 0); else gl.uniform2f(blu.uDir, 0, 1 / bh);
          gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
          const t = src; src = dst; dst = t;
        }
        // composite to screen
        gl.bindFramebuffer(gl.FRAMEBUFFER, null); gl.viewport(0, 0, canvas.width, canvas.height);
        gl.useProgram(composite);
        gl.activeTexture(gl.TEXTURE0); gl.bindTexture(gl.TEXTURE_2D, sceneTex); gl.uniform1i(cu.uScene, 0);
        gl.activeTexture(gl.TEXTURE1); gl.bindTexture(gl.TEXTURE_2D, bTex[src]); gl.uniform1i(cu.uBloom, 1);
        gl.uniform1f(cu.uBloomStr, 1.55);
        gl.bindVertexArray(fsVAO); gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
        gl.bindVertexArray(null); gl.activeTexture(gl.TEXTURE0);
      } else {
        gl.bindFramebuffer(gl.FRAMEBUFFER, null);
        gl.viewport(0, 0, canvas.width, canvas.height);
        drawScene();
      }
    }

    function dispose() {
      canvas.removeEventListener("mousedown", onDown);
      window.removeEventListener("mouseup", onUp);
      window.removeEventListener("mousemove", onMove);
      canvas.removeEventListener("wheel", onWheel);
      const ext = gl.getExtension("WEBGL_lose_context"); if (ext) ext.loseContext();
    }

    return { frame, resize, dispose, setHighlight, setSpotlight, focusNode, clearFocus, setData() {} };
  }

  window.Mesh3D = { init };
})();
