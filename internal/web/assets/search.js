// search.js: the Search view. A query box runs the same fused retrieval the agent
// gets over MCP (GET /api/search) and renders ranked cards; opening a card fetches
// the note (GET /api/note/{id}) into a reading pane, with a jump back to the graph.
// Registers on Mesh.views.search. Vanilla, no deps.
(function () {
  "use strict";
  const Mesh = (window.Mesh = window.Mesh || {});
  Mesh.views = Mesh.views || {};

  function esc(s) { return (s == null ? "" : String(s)).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])); }
  // the retriever wraps matched terms in [brackets]; render them as marks.
  function snippet(s) { return esc(s).replace(/\[([^\]]+)\]/g, "<mark>$1</mark>"); }

  function cardHTML(c) {
    const tier = c.Tier0 ? '<span class="t0">tier-0</span>' : "";
    const score = typeof c.Score === "number" ? c.Score.toFixed(2) : "";
    return (
      '<button class="rcard" data-id="' + esc(c.NoteID) + '">' +
      '<div class="rc-head"><span class="rc-title">' + esc(c.Title || c.NoteID) + "</span>" + tier + '<span class="rc-score">' + score + "</span></div>" +
      '<div class="rc-path">' + esc(c.Path) + "</div>" +
      (c.Snippet ? '<div class="rc-snip">' + snippet(c.Snippet) + "</div>" : "") +
      (c.Reason ? '<div class="rc-reason">' + esc(c.Reason) + "</div>" : "") +
      "</button>"
    );
  }

  Mesh.views.search = function (el, M) {
    const inner = document.createElement("div");
    inner.className = "panel-inner";
    inner.innerHTML =
      '<p class="panel-h">Retrieve</p>' +
      '<h1 class="panel-title">Search</h1>' +
      '<p class="panel-lead">Searches the full text of every note and ranks by relevance. This is the same engine your AI agent uses. (The box on the Graph view is different: it only hides or shows nodes by name.)</p>' +
      '<div class="srch-box"><input id="srch-q" type="search" placeholder="search the whole vault" autocomplete="off" spellcheck="false"></div>' +
      '<div id="srch-results" class="srch-results"><p class="srch-hint">Type to search. Results are the fused full-text + graph + semantic ranking, with decisions, gotchas, and post-mortems surfaced first.</p></div>';
    el.replaceChildren(inner);

    const q = inner.querySelector("#srch-q");
    const results = inner.querySelector("#srch-results");
    let timer, lastSeq = 0;

    async function run() {
      const term = q.value.trim();
      if (!term) {
        results.innerHTML = '<p class="srch-hint">Type to search.</p>';
        return;
      }
      const seq = ++lastSeq;
      results.innerHTML = '<p class="srch-hint">Searching...</p>';
      try {
        const data = await M.api("/api/search?q=" + encodeURIComponent(term) + "&limit=15");
        if (seq !== lastSeq) return; // a newer query superseded this one
        const cards = data.cards || [];
        if (!cards.length) {
          results.innerHTML = '<p class="srch-hint">No matches for "' + esc(term) + '".</p>';
          return;
        }
        results.innerHTML = '<p class="srch-count">' + cards.length + " results &middot; " + (data.tokens || 0) + " tokens</p>" + cards.map(cardHTML).join("");
        results.querySelectorAll(".rcard").forEach((b) => b.addEventListener("click", () => openNote(b.dataset.id)));
      } catch (e) {
        if (seq === lastSeq) results.innerHTML = '<p class="srch-hint">Search failed: ' + esc(e.message) + "</p>";
      }
    }

    async function openNote(id) {
      results.innerHTML = '<p class="srch-hint">Loading...</p>';
      try {
        const n = await M.api("/api/note/" + encodeURIComponent(id));
        const bodyHTML = n.html || ('<pre class="note-md">' + esc(n.markdown) + "</pre>");
        results.innerHTML =
          '<div class="note-pane">' +
          '<div class="note-bar"><button class="btn ghost" id="note-back">&larr; results</button>' +
          '<span class="note-path">' + esc(n.path) + "</span>" +
          '<button class="btn ghost" id="note-graph">show in graph</button></div>' +
          '<div class="note-body prose">' + bodyHTML + "</div></div>";
        results.querySelector("#note-back").addEventListener("click", run);
        results.querySelector("#note-graph").addEventListener("click", () => {
          if (Mesh.route) Mesh.route("graph");
          location.hash = "";
          const gq = document.getElementById("q");
          if (gq) { gq.value = id; gq.dispatchEvent(new Event("input", { bubbles: true })); }
        });
      } catch (e) {
        results.innerHTML = '<p class="srch-hint">Could not open note: ' + esc(e.message) + "</p>";
      }
    }

    q.addEventListener("input", () => {
      clearTimeout(timer);
      timer = setTimeout(run, 250);
    });
    q.focus();
  };
})();
