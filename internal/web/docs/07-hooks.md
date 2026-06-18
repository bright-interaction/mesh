# Make your agent use Mesh automatically (session hooks)

This is the superpower. Mesh is most valuable when an agent **reads it at the
start of every session** and **writes back what it learned at the end**, every
time, without anyone remembering to. Two Claude Code session hooks make that the
default.

## What they do

- **SessionStart -> the agent reads the mesh.** When a session begins, a hook runs
  `mesh orient` and injects a short orientation: the most-connected entry-point
  notes, what changed in the last 7 days, and how to retrieve cheaply. The agent
  starts already knowing the shape of your knowledge.
- **Stop -> the agent writes back.** When the agent is about to finish, a hook
  nudges it once to record any decision, gotcha, or post-mortem with
  `mesh_append_note`. It only nudges once per session, and not at all if the agent
  already wrote something, so it never loops.

Together that is the **flywheel**: every session starts informed and ends a little
smarter, so knowledge compounds across sessions and teammates instead of being
relearned each time.

## These are session hooks, not git hooks

A common mix-up: this is **not** a git pre-push or post-push hook. Those fire on
code pushes and are a separate layer (Mesh's own post-commit reindex, your repo's
sync). The read/write-back discipline is tied to the **agent session lifecycle**,
which is what Claude Code's SessionStart and Stop hooks control. (SessionEnd exists
but can only do cleanup, so the write-back nudge lives on Stop.)

## Setting it up

The easiest way: just ask your agent. Once Mesh is connected over MCP, say
"set up the Mesh session hooks" and it will call `mesh_setup_hooks`, explain the
trade-offs, ask which project and whether to enforce write-back, and wire it in.

Or do it yourself from the CLI:

```
mesh hooks install /path/to/vault                 # read at start + nudge write-back
mesh hooks install /path/to/vault --read-only     # just the start-of-session read
mesh hooks install /path/to/vault --dry-run       # preview the settings.json first
mesh hooks uninstall                              # remove them
```

It merges into the project's `.claude/settings.json` (it never clobbers your other
settings and is safe to run twice). After installing, run `/hooks` in Claude Code
to verify and start a new session.

## Read-only vs enforce write-back

- **Read-only** adds only the SessionStart orientation. Low friction, no nagging.
- **Enforce write-back** also adds the Stop nudge. Use it when you want the flywheel
  to actually turn, the agent gets one reminder per session to capture what it
  learned. You can always switch with `uninstall` then `install --read-only`.
