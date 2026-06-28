# Knowledge health (keeping the vault trustworthy)

A knowledge base is only useful if you can trust it. Mesh runs a lifecycle check that
flags notes which have gone stale or wrong, so the vault self-heals instead of rotting.
Run it with `mesh health`, the `mesh_health` MCP tool, or read the counts on the
Dashboard.

## What it checks

- **Dead references.** A note that cites a source file which no longer exists in the
  code index (it moved or was deleted). Mesh only flags a path inside a directory it
  actually indexes, so cross-repo or illustrative filenames never cry wolf.
- **Overdue reviews.** A note with a `review_by` date in the past. Time-sensitive
  knowledge (a "current" status, a temporary workaround) can carry a review date so it
  gets re-checked instead of silently aging.
- **Contradictions.** Two tier-0 notes (decisions, gotchas, post-mortems) that share a
  tag where one note's "do" strongly overlaps another's "dont", i.e. one recommends
  what the other forbids. A dependency-free heuristic flags the pair; the optional
  sync-curator can confirm with an LLM.

Findings are written to the index and surfaced as counts on the Dashboard and in full
by the tool, grouped by issue. Fixing or updating the flagged note clears it on the
next check.

## Freshness in ranking (optional)

Retrieval can apply a gentle freshness decay so an equal-but-stale note ranks below a
fresh one. It is type-aware: institutional memory (decisions, gotchas, post-mortems)
decays slowly or not at all, while a `note` or `status` decays on a configurable
half-life. It is off by default (Settings: `freshness_half_life_days`), so nothing
changes silently.

## The point

Health is what lets the flywheel compound without turning into a junk drawer. An agent
that writes back also gets nudged, via `mesh_health`, to fix what its change made stale,
so the vault stays a thing you can rely on rather than a pile that grows.
