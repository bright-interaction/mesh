// review.js: the Review queue. Auto-extracted write-back candidates (the input side of
// the flywheel) that a human promotes into the vault with one click, or discards. This
// is what makes auto-extraction safe: nothing lands unreviewed. Reads GET /api/pending;
// POST /api/pending/promote|discard. Registers Mesh.views.review. Vanilla, no deps.
(function () {
  "use strict";
  const Mesh = (window.Mesh = window.Mesh || {});
  Mesh.views = Mesh.views || {};

  function esc(s) { return (s == null ? "" : String(s)).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])); }

  // Keep the nav badge in sync with the queue depth, so a reviewer notices new items
  // without opening the view. Best-effort (silent before login).
  function refreshBadge(M) {
    const el = document.getElementById("review-badge");
    if (!el) return;
    (M || Mesh).api("/api/pending").then(function (d) {
      const n = (d.pending || []).length;
      if (n > 0) {
        el.textContent = String(n);
        el.hidden = false;
        el.style.cssText = "margin-left:.4rem;font:600 .7rem ui-monospace,monospace;background:#fb923c;color:#0d1117;border-radius:7px;padding:0 .4rem";
      } else {
        el.hidden = true;
      }
    }).catch(function () {});
  }
  Mesh.refreshReviewBadge = refreshBadge;

  function card(p) {
    const meta = [p.confidence ? "confidence: " + esc(p.confidence) : "", p.source ? "from " + esc(p.source) : ""].filter(Boolean).join(" &middot; ");
    return (
      '<div class="rev-card" data-id="' + esc(p.id) + '" style="border:1px solid var(--hair,#21262d);border-radius:12px;padding:1rem 1.1rem;margin:0 0 1rem">' +
      '<div style="display:flex;align-items:center;gap:.6rem;margin-bottom:.5rem">' +
      '<span style="font:600 .7rem ui-monospace,monospace;text-transform:uppercase;letter-spacing:.06em;color:#fb923c;border:1px solid #3a2516;border-radius:6px;padding:.1rem .45rem">' + esc(p.type) + "</span>" +
      '<span style="font-weight:600;font-size:1.02rem">' + esc(p.title) + "</span></div>" +
      (p.do ? '<div style="font-size:.9rem;margin:.2rem 0"><b style="color:#3fb950">do</b> ' + esc(p.do) + "</div>" : "") +
      (p.dont ? '<div style="font-size:.9rem;margin:.2rem 0"><b style="color:#f85149">don\'t</b> ' + esc(p.dont) + "</div>" : "") +
      (p.why ? '<div style="font-size:.9rem;margin:.2rem 0;color:#9da7b3"><b>why</b> ' + esc(p.why) + "</div>" : "") +
      (meta ? '<div style="font-size:.78rem;color:#6e7681;margin-top:.5rem">' + meta + "</div>" : "") +
      '<div style="display:flex;gap:.6rem;margin-top:.8rem">' +
      '<button class="rev-act" data-act="promote" data-id="' + esc(p.id) + '" style="background:#238636;color:#fff;border:0;border-radius:7px;padding:.4rem .9rem;font-weight:600;cursor:pointer">Keep it</button>' +
      '<button class="rev-act" data-act="discard" data-id="' + esc(p.id) + '" style="background:transparent;color:#9da7b3;border:1px solid var(--hair,#21262d);border-radius:7px;padding:.4rem .9rem;cursor:pointer">Discard</button>' +
      "</div></div>"
    );
  }

  Mesh.views.review = function (el, M) {
    const inner = document.createElement("div");
    inner.className = "panel-inner";
    inner.innerHTML =
      '<p class="panel-h">Flywheel</p>' +
      '<h1 class="panel-title">Review queue</h1>' +
      '<p class="panel-lead">Notes auto-extracted from sessions where the agent did not write back. Keep the good ones (one click adds them to the vault) or discard the rest. This is the human check on auto-extraction.</p>' +
      '<div id="rev-body"><p class="srch-hint">Loading...</p></div>';
    el.replaceChildren(inner);
    const body = inner.querySelector("#rev-body");

    function load() {
      M.api("/api/pending").then(function (d) {
        const items = d.pending || [];
        if (!items.length) {
          body.innerHTML = '<p style="color:#3fb950;font-size:.95rem">Nothing to review. Auto-extracted notes will appear here when a session ends without a write-back.</p>';
          refreshBadge(M);
          return;
        }
        body.innerHTML = items.map(card).join("");
      }).catch(function (e) {
        body.innerHTML = '<p class="srch-hint">Could not load the review queue: ' + esc(e.message) + "</p>";
      });
    }

    body.addEventListener("click", function (e) {
      const b = e.target.closest("button.rev-act");
      if (!b) return;
      const id = b.dataset.id, act = b.dataset.act;
      const cardEl = b.closest(".rev-card");
      b.disabled = true;
      M.api("/api/pending/" + act, { method: "POST", body: JSON.stringify({ id: id }) })
        .then(function () {
          if (cardEl) cardEl.remove();
          if (!body.querySelector(".rev-card")) load();
          refreshBadge(M);
        })
        .catch(function (err) { b.disabled = false; alert("Failed: " + err.message); });
    });

    load();
  };

  // Refresh the badge once at boot so the count shows before opening the view.
  if (document.readyState !== "loading") setTimeout(() => refreshBadge(Mesh), 800);
  else document.addEventListener("DOMContentLoaded", () => setTimeout(() => refreshBadge(Mesh), 800));
})();
