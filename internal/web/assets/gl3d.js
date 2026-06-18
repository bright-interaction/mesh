// mesh ui 3D galaxy: a raw WebGL2 renderer (no three.js, no CDN, no deps) that
// shows the same graph as a deep-space cluster of glowing community shells orbiting
// the index "sun". Instanced billboarded sprites with additive glow hit 60fps at
// 1-2k notes. Exposed as window.Mesh3D.init(canvas, G, opts) -> api | null; init
// returns null when WebGL2 is unavailable so app.js falls back to the 2D galaxy.
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

  const SPRITE_VS = `#version 300 es
  layout(location=0) in vec2 corner;
  layout(location=1) in vec3 iPos;
  layout(location=2) in float iSize;
  layout(location=3) in vec3 iColor;
  layout(location=4) in float iFlag;   // 1 = the index "sun"
  uniform mat4 uProj, uView;
  uniform float uHi;                    // index of the highlighted instance, -1 none
  uniform float uTime;
  out vec2 vUV; out vec3 vColor; out float vGlow;
  void main(){
    vec4 vp = uView * vec4(iPos, 1.0);
    float size = iSize;
    bool sun = iFlag > 0.5;
    if (sun) size *= 1.9 + 0.12 * sin(uTime);   // gentle pulse
    bool hi = abs(float(gl_InstanceID) - uHi) < 0.5;
    if (hi) size *= 1.6;
    vp.xy += corner * size;                       // view-space billboard (depth-scales)
    gl_Position = uProj * vp;
    vUV = corner;
    vColor = sun ? mix(iColor, vec3(1.0, 0.95, 0.86), 0.7) : iColor;
    vGlow = sun ? 1.6 : (hi ? 1.5 : 1.0);
  }`;

  const SPRITE_FS = `#version 300 es
  precision highp float;
  in vec2 vUV; in vec3 vColor; in float vGlow;
  out vec4 frag;
  void main(){
    float d = length(vUV);
    if (d > 1.0) discard;
    float core = smoothstep(1.0, 0.0, d);
    float glow = pow(core, 2.4) * vGlow;
    frag = vec4(vColor * glow, glow);            // additive (alpha = glow)
  }`;

  const LINE_VS = `#version 300 es
  layout(location=0) in vec3 aPos;
  uniform mat4 uProj, uView;
  void main(){ gl_Position = uProj * uView * vec4(aPos, 1.0); }`;
  const LINE_FS = `#version 300 es
  precision highp float;
  out vec4 frag;
  void main(){ frag = vec4(0.55, 0.6, 0.78, 0.05); }`;

  function init(canvas, G, opts) {
    const gl = canvas.getContext("webgl2", { antialias: true, alpha: false, premultipliedAlpha: false });
    if (!gl) return null;
    opts = opts || {};
    const commColor = opts.commColor || new Map();
    const indexId = opts.indexId || (G.meta && G.meta.index_id) || "";

    const sprite = program(gl, SPRITE_VS, SPRITE_FS);
    const line = program(gl, LINE_VS, LINE_FS);
    if (!sprite || !line) return null;

    const nodes = G.nodes || [];
    const N = nodes.length;
    const idIndex = new Map();
    nodes.forEach((n, i) => idIndex.set(n.id, i));

    // --- 3D layout: community centroids on a Fibonacci sphere; nodes scattered on a
    // small local sphere around their centroid; the index note at the origin (sun). ---
    const maxOrbit = Math.max(1, (G.meta && G.meta.max_orbit) || 1);
    const comms = (G.communities || []).map((c) => c.id);
    const commRank = new Map(comms.map((id, i) => [id, i]));
    const C = Math.max(1, comms.length);
    const SHELL = 46;
    function centroid(commId) {
      const k = commRank.has(commId) ? commRank.get(commId) : C - 1;
      // Fibonacci sphere point for cluster k.
      const y = 1 - (2 * k + 1) / C;          // -1..1
      const r = Math.sqrt(Math.max(0, 1 - y * y));
      const phi = k * 2.399963229728653;       // golden angle
      return [Math.cos(phi) * r * SHELL, y * SHELL, Math.sin(phi) * r * SHELL];
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
        const local = 6 + (n.community % 5) * 1.6;        // cluster spread
        const sx = (rand(i * 3 + 1) - 0.5), sy = (rand(i * 7 + 2) - 0.5), sz = (rand(i * 11 + 5) - 0.5);
        // pull low-orbit notes a touch toward the center so depth reads as relatedness
        const pull = 0.55 + 0.45 * ((n.orbit || 0) / maxOrbit);
        x = c[0] * pull + sx * local * 2; y = c[1] * pull + sy * local * 2; z = c[2] * pull + sz * local * 2;
      }
      pos[i * 3] = x; pos[i * 3 + 1] = y; pos[i * 3 + 2] = z;
      size[i] = isSun ? 3.0 : Math.max(0.9, (n.size || 1) * 0.95);
      const rgb = hexToRGB(commColor.get(n.community));
      color[i * 3] = rgb[0]; color[i * 3 + 1] = rgb[1]; color[i * 3 + 2] = rgb[2];
      flag[i] = isSun ? 1 : 0;
    }

    // --- buffers ---
    const quad = new Float32Array([-1, -1, 1, -1, -1, 1, 1, 1]);
    const vao = gl.createVertexArray();
    gl.bindVertexArray(vao);
    const qb = gl.createBuffer(); gl.bindBuffer(gl.ARRAY_BUFFER, qb); gl.bufferData(gl.ARRAY_BUFFER, quad, gl.STATIC_DRAW);
    gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
    const mk = (loc, data, n) => {
      const b = gl.createBuffer(); gl.bindBuffer(gl.ARRAY_BUFFER, b); gl.bufferData(gl.ARRAY_BUFFER, data, gl.STATIC_DRAW);
      gl.enableVertexAttribArray(loc); gl.vertexAttribPointer(loc, n, gl.FLOAT, false, 0, 0); gl.vertexAttribDivisor(loc, 1);
    };
    mk(1, pos, 3); mk(2, size, 1); mk(3, color, 3); mk(4, flag, 1);
    gl.bindVertexArray(null);

    // edges -> line vertex buffer (two endpoints each), faint connective web.
    const edges = G.edges || [];
    const lp = [];
    for (const e of edges) {
      const a = idIndex.get(e.source), b = idIndex.get(e.target);
      if (a === undefined || b === undefined) continue;
      lp.push(pos[a * 3], pos[a * 3 + 1], pos[a * 3 + 2], pos[b * 3], pos[b * 3 + 1], pos[b * 3 + 2]);
    }
    const lineVerts = new Float32Array(lp);
    const lvao = gl.createVertexArray();
    gl.bindVertexArray(lvao);
    const lb = gl.createBuffer(); gl.bindBuffer(gl.ARRAY_BUFFER, lb); gl.bufferData(gl.ARRAY_BUFFER, lineVerts, gl.STATIC_DRAW);
    gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 3, gl.FLOAT, false, 0, 0);
    gl.bindVertexArray(null);

    // starfield: distant faint points on a big sphere, drawn with the sprite program.
    const STAR = 420;
    const spos = new Float32Array(STAR * 3), ssize = new Float32Array(STAR), scol = new Float32Array(STAR * 3), sflag = new Float32Array(STAR);
    for (let i = 0; i < STAR; i++) {
      const y = 1 - (2 * i + 1) / STAR, r = Math.sqrt(Math.max(0, 1 - y * y)), phi = i * 2.399963229728653, R = 220 + rand(i * 9) * 80;
      spos[i * 3] = Math.cos(phi) * r * R; spos[i * 3 + 1] = y * R; spos[i * 3 + 2] = Math.sin(phi) * r * R;
      ssize[i] = 0.5 + rand(i * 3) * 0.7;
      const w = 0.5 + rand(i * 5) * 0.5; scol[i * 3] = w; scol[i * 3 + 1] = w; scol[i * 3 + 2] = w * 1.05;
      sflag[i] = 0;
    }
    const svao = gl.createVertexArray();
    gl.bindVertexArray(svao);
    const sqb = gl.createBuffer(); gl.bindBuffer(gl.ARRAY_BUFFER, sqb); gl.bufferData(gl.ARRAY_BUFFER, quad, gl.STATIC_DRAW);
    gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
    mk(1, spos, 3); mk(2, ssize, 1); mk(3, scol, 3); mk(4, sflag, 1);
    gl.bindVertexArray(null);

    // --- camera + interaction ---
    const cam = { yaw: 0.6, pitch: -0.35, dist: 150 };
    let W = 1, H = 1, dpr = 1, proj = perspective(1.05, 1, 1, 2000), vp = viewMatrix(cam.yaw, cam.pitch, cam.dist);
    let drag = false, lx = 0, ly = 0, moved = false, hi = -1, time = 0;

    function resize() {
      dpr = Math.max(1, window.devicePixelRatio || 1);
      W = window.innerWidth; H = window.innerHeight;
      canvas.width = Math.round(W * dpr); canvas.height = Math.round(H * dpr);
      canvas.style.width = W + "px"; canvas.style.height = H + "px";
      proj = perspective(1.05, W / Math.max(1, H), 1, 2000);
    }
    resize();

    // pick: project every node to screen, return the nearest within a px radius.
    function pickAt(mx, my) {
      vp = viewMatrix(cam.yaw, cam.pitch, cam.dist);
      const m = mul(proj, vp);
      let best = -1, bestD = Infinity;
      for (let i = 0; i < N; i++) {
        const x = pos[i * 3], y = pos[i * 3 + 1], z = pos[i * 3 + 2];
        const cx = m[0] * x + m[4] * y + m[8] * z + m[12];
        const cy = m[1] * x + m[5] * y + m[9] * z + m[13];
        const cw = m[3] * x + m[7] * y + m[11] * z + m[15];
        if (cw <= 0) continue; // behind the camera
        const sx = (cx / cw * 0.5 + 0.5) * W, sy = (1 - (cy / cw * 0.5 + 0.5)) * H;
        const dx = sx - mx, dy = sy - my, d2 = dx * dx + dy * dy;
        const rad = Math.max(9, size[i] * 90 / cw); // depth-scaled hit radius
        if (d2 <= rad * rad && d2 < bestD) { bestD = d2; best = i; }
      }
      return best;
    }

    function onDown(e) { drag = true; moved = false; lx = e.clientX; ly = e.clientY; }
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
        cam.yaw += dx * 0.005; cam.pitch += dy * 0.005;
        cam.pitch = Math.max(-1.45, Math.min(1.45, cam.pitch));
      } else if (opts.onHover) {
        const rect = canvas.getBoundingClientRect();
        const i = pickAt(e.clientX - rect.left, e.clientY - rect.top);
        if (i !== hi) { hi = i; opts.onHover(i >= 0 ? nodes[i] : null); }
        canvas.style.cursor = i >= 0 ? "pointer" : "grab";
      }
    }
    function onWheel(e) { e.preventDefault(); cam.dist *= Math.exp(e.deltaY * 0.001); cam.dist = Math.max(20, Math.min(900, cam.dist)); }
    canvas.addEventListener("mousedown", onDown);
    window.addEventListener("mouseup", onUp);
    window.addEventListener("mousemove", onMove);
    canvas.addEventListener("wheel", onWheel, { passive: false });

    let autoSpin = true;
    function setHighlight(id) { hi = (id != null && idIndex.has(id)) ? idIndex.get(id) : -1; }

    function frame() {
      time += 0.03;
      if (autoSpin && !drag) cam.yaw += 0.0009;
      vp = viewMatrix(cam.yaw, cam.pitch, cam.dist);
      gl.viewport(0, 0, canvas.width, canvas.height);
      gl.clearColor(0.02, 0.018, 0.035, 1);
      gl.clear(gl.COLOR_BUFFER_BIT);
      gl.enable(gl.BLEND);
      gl.blendFunc(gl.SRC_ALPHA, gl.ONE); // additive glow
      gl.disable(gl.DEPTH_TEST);

      // starfield (behind everything; additive, faint)
      gl.useProgram(sprite);
      gl.uniformMatrix4fv(gl.getUniformLocation(sprite, "uProj"), false, proj);
      gl.uniformMatrix4fv(gl.getUniformLocation(sprite, "uView"), false, vp);
      gl.uniform1f(gl.getUniformLocation(sprite, "uHi"), -1);
      gl.uniform1f(gl.getUniformLocation(sprite, "uTime"), time);
      gl.bindVertexArray(svao);
      gl.drawArraysInstanced(gl.TRIANGLE_STRIP, 0, 4, STAR);

      // edges (very faint connective web)
      if (lineVerts.length) {
        gl.useProgram(line);
        gl.uniformMatrix4fv(gl.getUniformLocation(line, "uProj"), false, proj);
        gl.uniformMatrix4fv(gl.getUniformLocation(line, "uView"), false, vp);
        gl.bindVertexArray(lvao);
        gl.drawArrays(gl.LINES, 0, lineVerts.length / 3);
      }

      // nodes (glowing community-colored sprites + the sun)
      gl.useProgram(sprite);
      gl.uniform1f(gl.getUniformLocation(sprite, "uHi"), hi);
      gl.bindVertexArray(vao);
      gl.drawArraysInstanced(gl.TRIANGLE_STRIP, 0, 4, N);
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
