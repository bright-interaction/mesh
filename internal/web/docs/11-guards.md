# Guards (turning gotchas into enforcement)

A gotcha you wrote down is only worth something if it stops the mistake from coming
back. Mesh knows your gotchas, so it can propose the pre-commit checks that enforce
them. This closes the loop from "we learned this" to "the repo refuses to let it
happen again".

## How it works

```
mesh guards list       # gotchas that have a concrete anti-pattern (guard candidates)
mesh guards suggest    # the LLM proposes a real check for each (review before enabling)
```

`suggest` takes each high-confidence gotcha that has a concrete "dont" (an anti-pattern
to detect) and asks your own LLM to propose a guard: a grep-style regex, the file globs
it applies to, a failure message, and a severity. Gotchas that are about judgment or
architecture (not mechanically checkable) are honestly marked as not applicable.

The applicable ones are emitted as a paste-ready bash block you can drop into your
pre-commit hook. For example, the gotcha "use bun, not npm" becomes a check that flags
`npm install` or a stray `package-lock.json`.

## You are the gate

This is a human-in-the-loop tool, on purpose. Mesh proposes; you review and paste in
the ones that fit. The generated script is a starting point: patterns that need
lookahead (which `grep -E` cannot run) are skipped with a note rather than emitted
broken. A guard that fires on legitimate code is worse than no guard, so review first.

## Why it matters

The same knowledge that an agent reads to avoid a mistake can now refuse the mistake at
commit time. Developer-laptop guards (the pre-commit hook) plus a server-side deploy
gate is the belt-and-suspenders pattern: the hook catches it early, the gate enforces
it. Mesh just makes writing the hook a review step instead of a from-scratch task.
