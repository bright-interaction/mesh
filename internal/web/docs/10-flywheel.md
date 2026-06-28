# The flywheel (how Mesh gets smarter with use)

The flywheel is the one thing about Mesh that compounds: agents write back what they
learn, the next agent inherits it, and the vault gets more useful every session. This
page is how Mesh feeds, measures, and proves that loop.

## The loop

1. An agent does work and learns something non-obvious (a decision, a gotcha, a
   post-mortem).
2. It calls `mesh_append_note` with a one-line do / dont / why. Mesh files it.
3. The next agent, on its next session, retrieves that note and starts smarter.

The session hooks (`mesh hooks install`) make this automatic: read the mesh at the
start of every session, get nudged to write back before finishing.

## Measuring it (the Dashboard)

A flywheel only matters if written notes actually get reused, so Mesh measures it
instead of asserting it. The Dashboard shows:

- **Reuse rate.** Of the notes that were written back, how many were fetched again in
  a LATER session (a fetch at least ten minutes after a note was authored counts as
  cross-session reuse, which works for both a solo CLI and a long-lived team hub).
- **Time to first reuse.** The median lag from writing a note to it being used again.
- **Write-back input health.** Write-backs per 100 reads, so you can see whether the
  loop is being fed or starved.

The number is honest and per-vault. It reflects your accumulated agent-authored notes
from day one (Mesh seeds the measurement from the existing corpus), and it climbs as
those notes get reused going forward.

## Feeding it automatically (the Review queue)

Most sessions end without the agent writing anything back, even with the nudge. So Mesh
can pull the durable learnings out of a finished session for you. When a session ends
with no write-back, an opt-in Stop hook (`mesh hooks install --extract`) extracts a few
candidate notes from the transcript using your own LLM and puts them in the **Review**
tab.

You review them with one click: **Keep** promotes a candidate into a real note (and it
becomes searchable immediately); **Discard** drops it. Nothing lands unreviewed.

Two honest properties of auto-extraction:

- It trades precision for coverage. On a benchmark it surfaced a candidate in far more
  sessions than the manual nudge (which was near zero), at moderate precision, which is
  exactly why a human reviews before anything is kept.
- Candidates that merely restate a note you already have are filtered out (deduped
  against the vault), so the queue shows new knowledge, not echoes.

You can also run it by hand: `mesh extract <transcript>` to preview, or
`mesh extract --recurring <dir>` to find problems that recur across many sessions (a
systemic issue worth a permanent fix, not just another note).
